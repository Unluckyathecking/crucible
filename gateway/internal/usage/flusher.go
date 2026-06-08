package usage

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/rs/zerolog/log"

	"github.com/Unluckyathecking/crucible/gateway/internal/observability"
)

// StripeMeter abstracts the Stripe meter_event emitter so the flusher can be tested without Stripe.
type StripeMeter interface {
	EmitMeterEvent(ctx context.Context, stripeCustomerID string, units uint64, idempotencyKey string) error
}

// reconcileQueryTimeout caps each backlog/unbillable reconcile query. Both queries run
// concurrently (one goroutine each), so the worst-case reconcile overhead equals a single
// query timeout (5 s) out of the default 30 s flusher period.
const reconcileQueryTimeout = 5 * time.Second

// batchPageSize limits the number of customers processed per flusher tick in each phase.
// At 30 s per tick, 100 customers × 1 Stripe API call each (~50 ms p95) ≈ 5 s max serial
// latency; in practice calls are fast and the limit is generous.
const batchPageSize = 100

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
	db                  *pgxpool.Pool
	stripe              StripeMeter
	period              time.Duration
	reconciler          *Reconciler
	reconcileErrCounter prometheus.Counter // defaults to observability.BillingReconcileErrorsTotal; injectable for tests
}

func NewFlusher(db *pgxpool.Pool, s StripeMeter, period time.Duration) *Flusher {
	var rec *Reconciler
	if db != nil {
		rec = NewReconciler(db)
	}
	return &Flusher{
		db:                  db,
		stripe:              s,
		period:              period,
		reconciler:          rec,
		reconcileErrCounter: observability.BillingReconcileErrorsTotal,
	}
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
// Prometheus gauges. Called after both flush phases each tick. The two queries run
// concurrently via a WaitGroup; worst-case latency is one query timeout (5 s), not two.
// A query failure produces a log warning and increments BillingReconcileErrorsTotal —
// it never aborts or affects the flush phases.
func (f *Flusher) setBacklogGauges(ctx context.Context) {
	if f.reconciler == nil {
		return
	}

	rCtx, rCancel := context.WithTimeout(ctx, reconcileQueryTimeout)
	defer rCancel()

	// Each result struct is owned by exactly one goroutine (br by goroutine-1,
	// ur by goroutine-2). The main goroutine reads them only after wg.Wait(),
	// which establishes the necessary happens-before edge. No mutex needed.
	type backlogResult struct {
		units, rows int64
		age         float64
		err         error
	}
	type unbillableResult struct {
		units, rows int64
		err         error
	}
	var (
		br backlogResult
		ur unbillableResult
		wg sync.WaitGroup
	)
	wg.Add(2)
	go func() {
		defer wg.Done()
		br.units, br.rows, br.age, br.err = f.reconciler.BacklogStats(rCtx)
	}()
	go func() {
		defer wg.Done()
		ur.units, ur.rows, ur.err = f.reconciler.UnbillableUsage(rCtx)
	}()
	wg.Wait()

	// Each query family is updated independently: if one fails, the other still
	// refreshes so its alerts remain live.
	if br.err != nil {
		// Leave gauges at their previous values: resetting to 0 would make a DB
		// timeout indistinguishable from an empty backlog and could clear active alerts.
		log.Warn().Err(br.err).Msg("flusher: reconcile BacklogStats failed; preserving previous gauge values")
	} else {
		if br.age < 0 {
			log.Warn().Float64("raw_age_seconds", br.age).Msg("flusher: clock skew detected (negative backlog age); clamping to 0")
			br.age = 0
		}
		observability.BillingBacklogUnits.Set(float64(br.units))
		observability.BillingBacklogRows.Set(float64(br.rows))
		observability.BillingBacklogOldestAgeSeconds.Set(br.age)
	}

	if ur.err != nil {
		log.Warn().Err(ur.err).Msg("flusher: reconcile UnbillableUsage failed; preserving previous gauge values")
	} else {
		observability.BillingUnbillableUnits.Set(float64(ur.units))
		observability.BillingUnbillableRows.Set(float64(ur.rows))
	}

	// Increment once per tick regardless of how many queries failed; per-query increments
	// would double-count a tick where both BacklogStats and UnbillableUsage fail, making
	// the counter drift away from the number of affected flusher ticks.
	if br.err != nil || ur.err != nil {
		f.reconcileErrCounter.Inc()
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
		ORDER BY MIN(u.created_at) ASC
		LIMIT $1
	`, batchPageSize)
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
			return fmt.Errorf("scan pending batch row: %w", err)
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
			ORDER BY MIN(u.created_at) ASC
			LIMIT $1
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
	`, batchPageSize)
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
			return fmt.Errorf("scan claimed batch row: %w", err)
		}
		batches = append(batches, b)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate claimed batches: %w", err)
	}

	var failed int
	for _, b := range batches {
		if b.units == 0 {
			// Zero-unit batches can't occur in production (server/routes.go enforces
			// BillableUnits >= 1). Mark flushed immediately without emitting to Stripe —
			// leaving them for Phase A retry would cause an infinite retry loop if units stay 0.
			log.Warn().Str("batch", b.batchID.String()).Msg("flusher: zero-unit batch; marking flushed without Stripe emit")
			if _, dbErr := f.db.Exec(ctx, `
				UPDATE usage_events
				SET flushed_to_stripe = TRUE
				WHERE batch_id = $1
				  AND customer_id = $2
				  AND flushed_to_stripe = FALSE
			`, b.batchID, b.customerID); dbErr != nil {
				log.Warn().Err(dbErr).Str("batch", b.batchID.String()).Msg("flusher: mark-flushed failed for zero-unit batch")
				failed++
			}
			continue
		}
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
		log.Warn().Err(err).Str("batch", batchID.String()).Msg("flusher: mark-flushed failed; next tick will re-emit (Stripe will dedupe)")
		return fmt.Errorf("mark flushed batch %s: %w", batchID, err)
	}
	if ct.RowsAffected() == 0 {
		log.Warn().Str("batch", batchID.String()).Str("customer", customerID.String()).Msg("flusher: mark-flushed affected 0 rows; batch_id/customer_id mismatch")
		return fmt.Errorf("mark flushed batch %s: 0 rows affected", batchID)
	}
	observability.BillingFlushTotal.WithLabelValues("ok").Inc()
	return nil
}
