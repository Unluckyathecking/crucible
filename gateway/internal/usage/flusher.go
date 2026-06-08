package usage

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/zerolog/log"

	"github.com/Unluckyathecking/crucible/gateway/internal/observability"
)

// StripeMeter abstracts the Stripe meter_event emitter so the flusher can be tested without Stripe.
type StripeMeter interface {
	EmitMeterEvent(ctx context.Context, stripeCustomerID string, units uint64, idempotencyKey string) error
}

// reconcileQueryTimeout caps each backlog/unbillable reconcile query. With two sequential
// queries each bounded at 5 s, the worst-case reconcile overhead is 10 s out of the
// default 30 s flusher period, leaving the majority of the budget for the flush phases.
const reconcileQueryTimeout = 5 * time.Second

// Flusher periodically emits Stripe meter_events for unflushed usage_events rows.
//
// The flow is two-phase to make the Stripe idempotency-key STABLE across retries:
//
//	Phase A — retryPendingBatches:
//	  Re-emit any batch_ids that exist on rows but haven't been marked flushed yet.
//	  Idempotency key = "crucible-batch-<batch_uuid>". Stripe dedupes on the server.
//
//	Phase B — claimAndEmitNewBatches:
//	  Find customers with unbatched (batch_id IS NULL) unflushed rows. For each:
//	  allocate a fresh UUID, stamp it onto all their unbatched rows in one statement,
//	  emit to Stripe with the new batch_id as the idem-key, then mark flushed.
//
// If we crash between any of (claim, emit, mark-flushed), the next tick picks the work
// back up with the SAME batch_id, so Stripe never double-counts. The pre-fix flusher
// derived the idem-key from changing row id ranges, which caused billing drift after a
// partial failure.
type Flusher struct {
	db         *pgxpool.Pool
	stripe     StripeMeter
	period     time.Duration
	reconciler *Reconciler
}

func NewFlusher(db *pgxpool.Pool, s StripeMeter, period time.Duration) *Flusher {
	var rec *Reconciler
	if db != nil {
		rec = NewReconciler(db)
	}
	return &Flusher{db: db, stripe: s, period: period, reconciler: rec}
}

// Run blocks until ctx is canceled, ticking every period.
func (f *Flusher) Run(ctx context.Context) {
	t := time.NewTicker(f.period)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := f.retryPendingBatches(ctx); err != nil {
				log.Warn().Err(err).Msg("flusher: retry-pending phase failed; will retry next tick")
			}
			if err := f.claimAndEmitNewBatches(ctx); err != nil {
				log.Warn().Err(err).Msg("flusher: claim-new phase failed; will retry next tick")
			}
			f.setBacklogGauges(ctx)
		}
	}
}

// setBacklogGauges queries the DB via the reconciler and updates the backlog/unbillable
// Prometheus gauges. Called after both flush phases each tick. A query failure only
// produces a log warning — it never aborts or affects the flush phases.
func (f *Flusher) setBacklogGauges(ctx context.Context) {
	if f.reconciler == nil {
		return
	}

	bCtx, bCancel := context.WithTimeout(ctx, reconcileQueryTimeout)
	defer bCancel()
	units, rows, ageSecs, err := f.reconciler.BacklogStats(bCtx)
	if err != nil {
		log.Warn().Err(err).Msg("flusher: reconcile BacklogStats failed; skipping backlog gauges")
	} else {
		observability.BillingBacklogUnits.Set(float64(units))
		observability.BillingBacklogRows.Set(float64(rows))
		observability.BillingBacklogOldestAgeSeconds.Set(ageSecs)
	}

	ubCtx, ubCancel := context.WithTimeout(ctx, reconcileQueryTimeout)
	defer ubCancel()
	ubUnits, ubRows, err := f.reconciler.UnbillableUsage(ubCtx)
	if err != nil {
		log.Warn().Err(err).Msg("flusher: reconcile UnbillableUsage failed; skipping unbillable gauge")
	} else {
		observability.BillingUnbillableUnits.Set(float64(ubUnits))
		observability.BillingUnbillableRows.Set(float64(ubRows))
	}
}

// retryPendingBatches re-emits batches that were claimed but never marked flushed.
// Safe to call repeatedly — Stripe dedupes on the stable batch_id idempotency key.
// Returns an error if any batch-level Stripe emit fails; the caller (Run) logs it and
// continues — the batches remain in the retry queue for the next tick.
func (f *Flusher) retryPendingBatches(ctx context.Context) error {
	rows, err := f.db.Query(ctx, `
		SELECT u.batch_id, c.stripe_customer_id, u.customer_id, SUM(u.billable_units)::bigint
		FROM usage_events u
		JOIN customers c ON c.id = u.customer_id
		WHERE u.batch_id IS NOT NULL AND u.flushed_to_stripe = FALSE
		  AND c.stripe_customer_id IS NOT NULL
		GROUP BY u.batch_id, c.stripe_customer_id, u.customer_id
		LIMIT 100
	`)
	if err != nil {
		return fmt.Errorf("query pending batches: %w", err)
	}
	defer rows.Close()
	type pending struct {
		batchID          uuid.UUID
		stripeCustomerID string
		customerID       uuid.UUID
		units            uint64
	}
	var batches []pending
	for rows.Next() {
		var p pending
		if err := rows.Scan(&p.batchID, &p.stripeCustomerID, &p.customerID, &p.units); err != nil {
			log.Warn().Err(err).Msg("flusher: failed to scan pending batch row; skipping")
			continue
		}
		batches = append(batches, p)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate pending: %w", err)
	}

	var failed int
	for _, b := range batches {
		if err := f.emitAndMark(ctx, b.batchID, b.stripeCustomerID, b.customerID, b.units); err != nil {
			failed++
		}
	}
	if failed > 0 {
		// Batch-level failures are retry-safe: Phase A re-picks them up next tick
		// with the same batch_id; Stripe deduplicates on the idempotency key.
		return fmt.Errorf("retry-pending: %d/%d batches failed emit", failed, len(batches))
	}
	return nil
}

// claimAndEmitNewBatches finds customers with unbatched events, allocates a UUID per customer,
// and stamps it onto all their unbatched rows in one bulk statement. Then emits + marks flushed.
// Returns an error if any batch-level Stripe emit fails; the caller (Run) logs it and
// continues — the claimed batch_ids remain for Phase A to retry next tick.
func (f *Flusher) claimAndEmitNewBatches(ctx context.Context) error {
	// Atomic bulk claim: find up to 100 customers with unbatched usage,
	// assign each a new batch_id, stamp their rows, and return the aggregated units.
	rows, err := f.db.Query(ctx, `
		WITH targets AS (
			SELECT u.customer_id, c.stripe_customer_id, gen_random_uuid() as new_batch_id
			FROM usage_events u
			JOIN customers c ON c.id = u.customer_id
			WHERE u.batch_id IS NULL AND u.flushed_to_stripe = FALSE
			  AND c.stripe_customer_id IS NOT NULL
			GROUP BY u.customer_id, c.stripe_customer_id
			LIMIT 100
		),
		claimed AS (
			UPDATE usage_events
			SET batch_id = targets.new_batch_id
			FROM targets
			WHERE usage_events.customer_id = targets.customer_id
			  AND usage_events.batch_id IS NULL
			  AND usage_events.flushed_to_stripe = FALSE
			RETURNING usage_events.batch_id, targets.stripe_customer_id, usage_events.customer_id, usage_events.billable_units
		)
		SELECT batch_id, stripe_customer_id, customer_id, COALESCE(SUM(billable_units), 0)::bigint
		FROM claimed
		GROUP BY batch_id, stripe_customer_id, customer_id
	`)
	if err != nil {
		return fmt.Errorf("bulk claim unbatched customers: %w", err)
	}
	defer rows.Close()
	type claimedBatch struct {
		batchID          uuid.UUID
		stripeCustomerID string
		customerID       uuid.UUID
		units            uint64
	}
	var batches []claimedBatch
	for rows.Next() {
		var b claimedBatch
		if err := rows.Scan(&b.batchID, &b.stripeCustomerID, &b.customerID, &b.units); err != nil {
			log.Warn().Err(err).Msg("flusher: failed to scan claimed batch row; skipping")
			continue
		}
		if b.units > 0 {
			batches = append(batches, b)
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate claimed batches: %w", err)
	}

	var failed int
	for _, b := range batches {
		if err := f.emitAndMark(ctx, b.batchID, b.stripeCustomerID, b.customerID, b.units); err != nil {
			failed++
		}
	}
	if failed > 0 {
		// Batch-level failures are retry-safe: Phase A re-picks the claimed-but-unflushed
		// rows up next tick with the same batch_id; Stripe deduplicates on the idempotency key.
		return fmt.Errorf("claim-new: %d/%d batches failed emit", failed, len(batches))
	}
	return nil
}

// emitAndMark emits a Stripe meter_event using batch_id as the idempotency key, then
// marks all rows in the batch flushed.
//
// Stripe emit failure returns an error so callers can count batch-level failures.
// The batch remains unflushable (flushed_to_stripe=FALSE, batch_id stamped), so Phase A
// (retryPendingBatches) picks it up on the next tick with the SAME batch_id — Stripe
// deduplicates on "crucible-batch-<uuid>", preventing double-billing.
//
// Mark-flushed failure is returned to the caller so the batch is counted in the
// failed summary and the operator can observe it via log warnings. The batch stays
// in Phase A's retry queue (batch_id stamped, flushed_to_stripe=FALSE) and is
// re-emitted next tick with the same idempotency key; Stripe deduplicates.
func (f *Flusher) emitAndMark(ctx context.Context, batchID uuid.UUID, stripeCustomerID string, customerID uuid.UUID, units uint64) error {
	idemKey := "crucible-batch-" + batchID.String()
	if err := f.stripe.EmitMeterEvent(ctx, stripeCustomerID, units, idemKey); err != nil {
		observability.BillingFlushTotal.WithLabelValues("error").Inc()
		log.Warn().Err(err).
			Str("batch", batchID.String()).
			Uint64("units", units).
			Msg("flusher: stripe emit failed; will retry next tick (same batch_id, idempotent)")
		return fmt.Errorf("stripe emit batch %s: %w", batchID, err)
	}
	// Filter by both batch_id and customer_id: batch_id is a fresh UUID per-customer per tick
	// (statistically unique), and customer_id adds defense-in-depth so a hypothetical UUID
	// collision or manual intervention can never mark another customer's rows as flushed.
	ct, err := f.db.Exec(ctx, `
		UPDATE usage_events
		SET flushed_to_stripe = TRUE
		WHERE batch_id = $1
		  AND customer_id = $2
		  AND flushed_to_stripe = FALSE
	`, batchID, customerID)
	if err != nil {
		observability.BillingFlushTotal.WithLabelValues("error").Inc()
		log.Warn().Err(err).Str("batch", batchID.String()).Msg("flusher: mark-flushed failed; next tick will re-emit (Stripe will dedupe)")
		return fmt.Errorf("mark flushed batch %s: %w", batchID, err)
	}
	if ct.RowsAffected() == 0 {
		observability.BillingFlushTotal.WithLabelValues("error").Inc()
		log.Warn().Str("batch", batchID.String()).Str("customer", customerID.String()).Msg("flusher: mark-flushed affected 0 rows; batch_id/customer_id mismatch")
		return fmt.Errorf("mark flushed batch %s: 0 rows affected", batchID)
	}
	observability.BillingFlushTotal.WithLabelValues("ok").Inc()
	return nil
}
