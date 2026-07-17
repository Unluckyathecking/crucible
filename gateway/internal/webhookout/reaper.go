package webhookout

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/rs/zerolog/log"

	"github.com/Unluckyathecking/crucible/gateway/internal/observability"
)

// deliveryReaperBatchSize bounds each DELETE to at most this many rows,
// mirroring jobs.Reaper's reaperBatchSize bound.
const deliveryReaperBatchSize = 500

// deliveryMaxBatchesPerSweep caps per-tick batch count so a large backlog
// drains over multiple ticks rather than monopolising one goroutine.
const deliveryMaxBatchesPerSweep = 20

// DeliveryReaper periodically deletes terminal webhook_deliveries rows with
// status='delivered' that are older than the configured retention window, in
// batches bounded by deliveryReaperBatchSize per tick.
//
// dead_letter rows are NEVER deleted — operators replay those via the
// dead-letter replay console (webhookout/replay.go).
type DeliveryReaper struct {
	db          *pgxpool.Pool
	retention   time.Duration
	interval    time.Duration
	batchSize   int
	reapedTotal prometheus.Counter
}

// NewDeliveryReaper constructs a DeliveryReaper. retention <= 0 makes Run an
// inert no-op, preserving today's behaviour (no deletion) for product clones
// that never set WEBHOOK_DELIVERY_RETENTION_DAYS.
func NewDeliveryReaper(db *pgxpool.Pool, retention, interval time.Duration) *DeliveryReaper {
	return &DeliveryReaper{
		db:          db,
		retention:   retention,
		interval:    interval,
		batchSize:   deliveryReaperBatchSize,
		reapedTotal: observability.WebhookDeliveriesReapedTotal,
	}
}

// Run blocks until ctx is cancelled, sweeping every interval. Nil-safe: a
// nil DeliveryReaper, nil db, or non-positive retention returns immediately
// without starting a ticker — mirrors jobs.Reaper.Run.
func (r *DeliveryReaper) Run(ctx context.Context) {
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

// sweep deletes delivered rows older than retention in batches, repeating
// until a batch comes back short, the per-tick batch cap is reached, or ctx
// is cancelled.
func (r *DeliveryReaper) sweep(ctx context.Context) {
	for i := 0; i < deliveryMaxBatchesPerSweep; i++ {
		if err := ctx.Err(); err != nil {
			return
		}
		n, err := r.deleteBatch(ctx)
		if err != nil {
			log.Warn().Err(err).Msg("webhook delivery reaper: delete batch failed; will retry next tick")
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

// deleteBatch deletes up to batchSize webhook_deliveries rows that are:
//   - status = 'delivered' (the only terminal status eligible for deletion)
//   - created_at older than retention
//
// dead_letter rows are explicitly excluded by the status filter — they must
// survive for operator replay via webhookout/replay.go.
func (r *DeliveryReaper) deleteBatch(ctx context.Context) (int64, error) {
	tag, err := r.db.Exec(ctx, `
		DELETE FROM webhook_deliveries
		WHERE id IN (
			SELECT id FROM webhook_deliveries
			WHERE status = 'delivered'
			  AND created_at < NOW() - $1 * INTERVAL '1 second'
			LIMIT $2
		)
	`, r.retention.Seconds(), r.batchSize)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}
