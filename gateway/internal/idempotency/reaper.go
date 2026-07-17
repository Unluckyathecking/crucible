package idempotency

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/rs/zerolog/log"

	"github.com/Unluckyathecking/crucible/gateway/internal/observability"
)

// reaperBatchSize bounds each DELETE to at most this many rows, mirroring
// jobs.Reaper's role: a large backlog built up over months can't be swept in
// one unbounded statement that holds a lock for the duration.
const reaperBatchSize = 500

// maxBatchesPerSweep caps how many batches a single tick will issue so a
// pathologically large backlog can't hold one goroutine in back-to-back DELETEs
// indefinitely; the remainder drains over subsequent ticks instead.
const maxBatchesPerSweep = 20

// Reaper periodically deletes idempotency_keys rows older than retention,
// in batches bounded by reaperBatchSize per tick.
//
// Idempotency keys are unique-per-request and rarely re-queried after
// completion; the Store's lazy expiry (store.go:105-112) only fires when
// the exact same (customer_id, key) is queried again and found expired, which
// never happens for the vast majority of keys. Without a background sweep
// the table grows without bound at high request volume.
type Reaper struct {
	db          *pgxpool.Pool
	retention   time.Duration
	interval    time.Duration
	batchSize   int
	reapedTotal prometheus.Counter
}

// NewReaper constructs a Reaper. retention <= 0 makes Run an inert no-op,
// preserving today's behaviour (no deletion) for product clones that never
// set IDEMPOTENCY_RETENTION_DAYS.
func NewReaper(db *pgxpool.Pool, retention, interval time.Duration) *Reaper {
	return &Reaper{
		db:          db,
		retention:   retention,
		interval:    interval,
		batchSize:   reaperBatchSize,
		reapedTotal: observability.IdempotencyKeysReapedTotal,
	}
}

// Run blocks until ctx is cancelled, sweeping every interval. Nil-safe: a
// nil Reaper, nil db, or non-positive retention returns immediately without
// starting a ticker — mirrors jobs.Reaper.Run and usage.Flusher.Run.
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

// sweep deletes rows older than retention in batches, repeating until a batch
// deletes fewer than batchSize rows (backlog drained), maxBatchesPerSweep
// batches have run (remainder rolls to next tick), or ctx is cancelled.
func (r *Reaper) sweep(ctx context.Context) {
	for i := 0; i < maxBatchesPerSweep; i++ {
		if err := ctx.Err(); err != nil {
			return
		}
		n, err := r.deleteBatch(ctx)
		if err != nil {
			log.Warn().Err(err).Msg("idempotency reaper: delete batch failed; will retry next tick")
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

// deleteBatch deletes up to batchSize idempotency_keys rows older than
// retention, keyed off created_at. Both pending (status_code IS NULL) and
// finalized rows are eligible once they are beyond the retention window:
// a pending row older than the retention window (typically days) has
// necessarily outlived any in-flight request and is safely orphaned.
func (r *Reaper) deleteBatch(ctx context.Context) (int64, error) {
	tag, err := r.db.Exec(ctx, `
		DELETE FROM idempotency_keys
		WHERE id IN (
			SELECT id FROM idempotency_keys
			WHERE created_at < NOW() - $1 * INTERVAL '1 second'
			LIMIT $2
		)
	`, r.retention.Seconds(), r.batchSize)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}
