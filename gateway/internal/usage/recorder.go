// Package usage records and flushes billable usage events.
package usage

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Unluckyathecking/crucible/gateway/internal/observability"
	"github.com/Unluckyathecking/crucible/gateway/internal/quota"
)

type Recorder struct {
	db    *pgxpool.Pool
	quota *quota.Tracker // optional; nil disables monthly quota tracking
}

// NewRecorder returns a Recorder that writes to Postgres and (if quota is non-nil)
// also increments the in-month Redis counter used by the quota middleware.
func NewRecorder(db *pgxpool.Pool, q *quota.Tracker) *Recorder { return &Recorder{db: db, quota: q} }

// Record inserts one usage_events row and updates the customer's monthly quota counter.
// Postgres is the source of truth (durable); the Redis counter is a fast-read mirror used
// for the per-request quota gate. A Redis failure is logged-but-tolerated — the Postgres
// row is what bills.
//
// On a successful Postgres insert, the quota middleware's record-signal in context is
// flipped via quota.MarkRecorded. The middleware uses that signal (not HTTP status) to
// decide whether to refund the up-front reserve — so HTTP 200 responses carrying a
// worker error envelope, which skip Record, correctly drop the reserve.
func (r *Recorder) Record(ctx context.Context, customerID, apiKeyID uuid.UUID, operation, requestID string, units uint64) error {
	_, err := r.db.Exec(ctx, `
		INSERT INTO usage_events (customer_id, api_key_id, operation, billable_units, request_id)
		VALUES ($1, $2, $3, $4, $5)
	`, customerID, apiKeyID, operation, units, requestID)
	if err != nil {
		return err
	}
	observability.UsageRecordsTotal.Inc()
	// Signal the quota middleware that real billable usage was recorded — it uses this
	// to decide whether to refund the up-front +1 reserve. No-op when quota middleware
	// isn't in the chain.
	quota.MarkRecorded(ctx)
	if r.quota != nil {
		// Best-effort. Quota middleware fails open on Redis errors so a counter blip
		// doesn't block traffic — and the Postgres row is the durable record.
		_ = r.quota.Add(ctx, customerID, units)
	}
	return nil
}
