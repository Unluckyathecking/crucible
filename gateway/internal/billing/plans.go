package billing

import (
	"context"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/zerolog/log"
)

// PlanEntry is the cached subset of a plans row that the gateway reads on every request.
type PlanEntry struct {
	RatePerMinute int
	MonthlyCap    int64 // 0 = unlimited
}

// PlanCache holds the plans table in memory and refreshes every minute.
// Reloads are single-flighted via `loading` — a sudden stampede of stale-cache reads
// after the TTL ticks won't fan out N concurrent DB queries.
type PlanCache struct {
	db      *pgxpool.Pool
	mu      sync.RWMutex
	plans   map[string]PlanEntry
	fresh   time.Time
	loading bool
}

func NewPlanCache(db *pgxpool.Pool) *PlanCache {
	return &PlanCache{db: db, plans: map[string]PlanEntry{}}
}

const cacheTTL = 60 * time.Second

// Get returns the full PlanEntry for the named plan. Falls back to a free-tier-shaped entry
// (60/min, 1000-unit monthly cap) for unknown plans so the gateway fails closed-ish, not wide open.
func (p *PlanCache) Get(ctx context.Context, planID string) PlanEntry {
	p.mu.RLock()
	stale := time.Since(p.fresh) > cacheTTL
	already := p.loading
	p.mu.RUnlock()

	if stale && !already {
		p.reload(ctx)
	}

	p.mu.RLock()
	defer p.mu.RUnlock()
	if e, ok := p.plans[planID]; ok {
		return e
	}
	return PlanEntry{RatePerMinute: 60, MonthlyCap: 1000}
}

// RatePerMinute is a thin convenience wrapper preserved for the rate-limit middleware.
func (p *PlanCache) RatePerMinute(ctx context.Context, planID string) int {
	return p.Get(ctx, planID).RatePerMinute
}

// MonthlyCap is a thin convenience wrapper used by the quota middleware. 0 = unlimited.
func (p *PlanCache) MonthlyCap(ctx context.Context, planID string) int64 {
	return p.Get(ctx, planID).MonthlyCap
}

func (p *PlanCache) reload(ctx context.Context) {
	p.mu.Lock()
	if p.loading {
		p.mu.Unlock()
		return
	}
	p.loading = true
	p.mu.Unlock()

	defer func() {
		p.mu.Lock()
		p.loading = false
		p.mu.Unlock()
	}()

	rows, err := p.db.Query(ctx, `SELECT id, rate_limit_per_minute, monthly_unit_cap FROM plans`)
	if err != nil {
		log.Warn().Err(err).Msg("plan cache reload failed; using last-known values")
		return
	}
	defer rows.Close()

	next := map[string]PlanEntry{}
	for rows.Next() {
		var id string
		var rate int
		var cap *int64 // monthly_unit_cap is nullable
		if err := rows.Scan(&id, &rate, &cap); err != nil {
			continue
		}
		entry := PlanEntry{RatePerMinute: rate}
		if cap != nil {
			entry.MonthlyCap = *cap
		}
		next[id] = entry
	}
	if err := rows.Err(); err != nil {
		log.Warn().Err(err).Msg("plan cache iteration error; keeping last-known values")
		return
	}
	p.mu.Lock()
	p.plans = next
	p.fresh = time.Now()
	p.mu.Unlock()
}
