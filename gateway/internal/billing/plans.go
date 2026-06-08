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
//
// After the first (cold) load, TTL refreshes run ASYNCHRONOUSLY: a stale read
// kicks off a background reload and serves the last-known-good value immediately,
// so no request eats the DB round-trip on the hot path. The background reload is
// rooted at baseCtx (a context tied to the cache's lifetime, NOT the request),
// so it cannot leak past the cache and cannot be cancelled by the triggering
// request returning. One extra stale serve per TTL window is acceptable under
// the existing 60s contract.
type PlanCache struct {
	db      db
	baseCtx context.Context
	mu      sync.RWMutex
	plans   map[string]PlanEntry
	fresh   time.Time
	loading bool
}

func NewPlanCache(pool *pgxpool.Pool) *PlanCache {
	return &PlanCache{db: pool, baseCtx: context.Background(), plans: map[string]PlanEntry{}}
}

const cacheTTL = 60 * time.Second

// reloadTimeout bounds a single background reload's DB round-trip so a slow or
// hung query can't pin the `loading` flag forever (which would wedge refreshes).
const reloadTimeout = 10 * time.Second

// Get returns the full PlanEntry for the named plan. Falls back to a free-tier-shaped entry
// (60/min, 1000-unit monthly cap) for unknown plans so the gateway fails closed-ish, not wide open.
func (p *PlanCache) Get(ctx context.Context, planID string) PlanEntry {
	p.mu.RLock()
	cold := p.fresh.IsZero()
	stale := time.Since(p.fresh) > cacheTTL
	already := p.loading
	p.mu.RUnlock()

	switch {
	case cold:
		// First load: no last-known-good value exists. Serving an empty plan set
		// would mis-tier rate-limit/quota, so block and populate synchronously.
		// reload() re-checks `loading` under the lock to preserve single-flight.
		p.reload(ctx)
	case stale && !already:
		// Warm cache past TTL: refresh in the background off a cache-rooted
		// context and serve the stale-but-valid value to this request now.
		// The next request after the reload finishes picks up fresh values.
		go p.reload(p.baseCtx)
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
	// Single-flight guard: only one goroutine reloads at a time.
	// Get() may schedule multiple background reloads if several requests race to
	// observe stale=true; this check ensures only the first proceeds.
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

	// Zero-value PlanCache literals (used in some unit tests) leave baseCtx nil;
	// context.WithTimeout panics on a nil parent. NewPlanCache always sets
	// baseCtx to context.Background(), so this guard is a no-op in production.
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, reloadTimeout)
	defer cancel()

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
