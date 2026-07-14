package jobs

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/rs/zerolog/log"

	"github.com/Unluckyathecking/crucible/gateway/internal/observability"
)

// reaperBatchSize bounds each DELETE to at most this many rows, mirroring
// usage.batchPageSize's role in the flusher: a backlog of terminal rows
// built up over months can't be swept in one unbounded statement that locks
// async_jobs for the duration.
const reaperBatchSize = 500

// Reaper periodically deletes terminal (succeeded, failed) async_jobs rows
// older than retention, in batches bounded by reaperBatchSize per tick.
type Reaper struct {
	db          *pgxpool.Pool
	retention   time.Duration
	interval    time.Duration
	batchSize   int
	reapedTotal prometheus.Counter // injectable for tests; defaults to observability.JobsReapedTotal
}

// NewReaper constructs a Reaper. retention <= 0 makes Run an inert no-op —
// mirroring the zero-config-safe stance the existing Job knobs already take
// (e.g. routes_table.go's AsyncRoutes defaulting empty) — so a product
// clone that never sets JOB_RETENTION_DAYS keeps every async_jobs row
// forever, today's behaviour.
func NewReaper(db *pgxpool.Pool, retention, interval time.Duration) *Reaper {
	return &Reaper{
		db:          db,
		retention:   retention,
		interval:    interval,
		batchSize:   reaperBatchSize,
		reapedTotal: observability.JobsReapedTotal,
	}
}

// Run blocks until ctx is cancelled, sweeping every interval. Nil-safe: a
// nil Reaper, nil db, or non-positive retention makes Run return
// immediately without starting a ticker — mirrors usage.Flusher.Run and
// jobs.Executor.Run's nil-safe-receiver pattern so cmd/gateway/main.go can
// construct and start it unconditionally.
func (r *Reaper) Run(ctx context.Context) {
	if r == nil || r.db == nil || r.retention <= 0 {
		return
	}
	interval := r.interval
	if interval <= 0 {
		interval = time.Hour
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			r.sweep(ctx)
		}
	}
}

// sweep deletes terminal rows older than retention in batches of
// r.batchSize, repeating within this tick until a batch deletes fewer than
// r.batchSize rows (the backlog is drained) or ctx is cancelled — the same
// "keep going until a batch comes back short" bound the usage flusher's
// batchPageSize-limited phases use, so a large backlog can't hold a single
// DELETE (and its row locks) open indefinitely.
func (r *Reaper) sweep(ctx context.Context) {
	for {
		if err := ctx.Err(); err != nil {
			return
		}
		n, err := r.deleteBatch(ctx)
		if err != nil {
			log.Warn().Err(err).Msg("reaper: delete batch failed; will retry next tick")
			return
		}
		if n > 0 {
			r.reapedTotal.Add(float64(n))
		}
		if n < int64(r.batchSize) {
			return
		}
	}
}

// deleteBatch deletes up to r.batchSize terminal (succeeded, failed) rows
// older than retention, keyed off created_at — never queued or running rows,
// regardless of age.
func (r *Reaper) deleteBatch(ctx context.Context) (int64, error) {
	tag, err := r.db.Exec(ctx, `
		DELETE FROM async_jobs
		WHERE id IN (
			SELECT id FROM async_jobs
			WHERE status IN ('succeeded', 'failed')
			  AND created_at < NOW() - $1 * INTERVAL '1 second'
			LIMIT $2
		)
	`, r.retention.Seconds(), r.batchSize)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}
