package usage

import (
	"context"
	"errors"
	"math"
	"strconv"
	"testing"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/Unluckyathecking/crucible/gateway/internal/observability"
)

// stubReconciler is a deterministic reconcilerIface for tests that need controlled failures.
type stubReconciler struct{ err error }

func (s *stubReconciler) BacklogStats(context.Context) (int64, int64, float64, error) {
	return 0, 0, 0, s.err
}
func (s *stubReconciler) UnbillableUsage(context.Context) (int64, int64, error) {
	return 0, 0, s.err
}


// TestBacklogStats_flushedRowExcluded verifies that a row with flushed_to_stripe=TRUE
// is not counted in the backlog. Uses a before/after delta against the shared DB so the
// assertion is deterministic regardless of other tests' rows.
func TestBacklogStats_flushedRowExcluded(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	custID, apiKeyID := setupTestCustomer(t, pool)
	t.Cleanup(func() { deleteUsageRows(t, pool, custID) })

	// Give the customer a stripe_customer_id so they're eligible for BacklogStats.
	stripeID := "cus_bs_flushed_" + custID.String()
	if _, err := pool.Exec(ctx,
		`UPDATE customers SET stripe_customer_id=$1 WHERE id=$2`, stripeID, custID,
	); err != nil {
		t.Fatalf("set stripe_customer_id: %v", err)
	}

	rec := NewReconciler(pool)

	// Baseline before inserting our flushed row.
	baseUnits, baseRows, _, err := rec.BacklogStats(ctx)
	if err != nil {
		t.Fatalf("BacklogStats baseline: %v", err)
	}

	if _, err := pool.Exec(ctx,
		`INSERT INTO usage_events (customer_id, api_key_id, operation, billable_units, request_id, flushed_to_stripe)
		 VALUES ($1, $2, 'bs.flushed', 7, $3, TRUE)`,
		custID, apiKeyID, "req-bs-flushed-"+custID.String(),
	); err != nil {
		t.Fatalf("insert flushed row: %v", err)
	}

	// After: BacklogStats must not change — flushed rows are excluded.
	afterUnits, afterRows, _, err := rec.BacklogStats(ctx)
	if err != nil {
		t.Fatalf("BacklogStats after insert: %v", err)
	}
	if afterUnits != baseUnits {
		t.Errorf("flushed row inflated BacklogStats units: before=%d after=%d", baseUnits, afterUnits)
	}
	if afterRows != baseRows {
		t.Errorf("flushed row inflated BacklogStats rows: before=%d after=%d", baseRows, afterRows)
	}
}

// TestBacklogStats_countsUnflushed seeds a known set of unflushed rows for a Stripe-linked
// customer and asserts BacklogStats returns the correct aggregate units and a positive age.
// Uses a before/after delta so the assertion is deterministic on a shared DB.
// BacklogStats mirrors the flusher's stripe_customer_id IS NOT NULL filter so it only
// counts rows the flusher can actually process.
func TestBacklogStats_countsUnflushed(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	custID, apiKeyID := setupTestCustomer(t, pool)
	t.Cleanup(func() { deleteUsageRows(t, pool, custID) })

	// Give the customer a stripe_customer_id so BacklogStats counts their rows.
	stripeID := "cus_bs_uf_" + custID.String()
	if _, err := pool.Exec(ctx,
		`UPDATE customers SET stripe_customer_id=$1 WHERE id=$2`, stripeID, custID,
	); err != nil {
		t.Fatalf("set stripe_customer_id: %v", err)
	}

	rec := NewReconciler(pool)

	// Baseline before inserting any rows.
	baseUnits, baseRows, _, err := rec.BacklogStats(ctx)
	if err != nil {
		t.Fatalf("BacklogStats baseline: %v", err)
	}

	// Insert 3 unflushed rows with known units (5 + 10 + 15 = 30).
	// Explicit created_at 1 second in the past guarantees afterAge > 0 even on fast systems.
	const wantDeltaUnits = int64(30)
	for i, u := range []int{5, 10, 15} {
		reqID := "req-bs-uf-" + custID.String() + strconv.Itoa(i)
		if _, err := pool.Exec(ctx,
			`INSERT INTO usage_events (customer_id, api_key_id, operation, billable_units, request_id, created_at)
			 VALUES ($1, $2, 'bs.unflushed', $3, $4, NOW() - interval '1 second')`,
			custID, apiKeyID, u, reqID,
		); err != nil {
			t.Fatalf("insert row %d: %v", i, err)
		}
	}
	// Insert one flushed row — must NOT change the delta.
	if _, err := pool.Exec(ctx,
		`INSERT INTO usage_events (customer_id, api_key_id, operation, billable_units, request_id, flushed_to_stripe)
		 VALUES ($1, $2, 'bs.flushed', 999, $3, TRUE)`,
		custID, apiKeyID, "req-bs-flushed-"+custID.String(),
	); err != nil {
		t.Fatalf("insert flushed row: %v", err)
	}

	// After: delta must be exactly our 3 unflushed rows; flushed row (999 units) is excluded.
	afterUnits, afterRows, afterAge, err := rec.BacklogStats(ctx)
	if err != nil {
		t.Fatalf("BacklogStats after insert: %v", err)
	}
	if afterUnits-baseUnits != wantDeltaUnits {
		t.Errorf("BacklogStats units delta = %d, want %d (3 unflushed rows, flushed excluded)",
			afterUnits-baseUnits, wantDeltaUnits)
	}
	if afterRows-baseRows != 3 {
		t.Errorf("BacklogStats rows delta = %d, want 3", afterRows-baseRows)
	}
	if afterAge <= 0 {
		t.Errorf("BacklogStats ageSecs = %f, want > 0 when unflushed rows exist", afterAge)
	}
}

// TestBacklogStats_unbillableRowsExcluded verifies that unflushed rows for customers
// WITHOUT a stripe_customer_id do NOT inflate BacklogStats. Uses a before/after delta
// so the assertion is deterministic on a shared DB.
func TestBacklogStats_unbillableRowsExcluded(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	// setupTestCustomer leaves stripe_customer_id NULL — permanently unbillable.
	custID, apiKeyID := setupTestCustomer(t, pool)
	t.Cleanup(func() { deleteUsageRows(t, pool, custID) })

	rec := NewReconciler(pool)

	// Baseline before inserting unbillable rows.
	baseUnits, baseRows, _, err := rec.BacklogStats(ctx)
	if err != nil {
		t.Fatalf("BacklogStats baseline: %v", err)
	}

	// Insert unflushed rows — customer has no stripe_customer_id, so these are unbillable.
	for i, u := range []int{100, 200} {
		reqID := "req-bs-ubexcl-" + custID.String() + strconv.Itoa(i)
		if _, err := pool.Exec(ctx,
			`INSERT INTO usage_events (customer_id, api_key_id, operation, billable_units, request_id)
			 VALUES ($1, $2, 'bs.ubexcl', $3, $4)`,
			custID, apiKeyID, u, reqID,
		); err != nil {
			t.Fatalf("insert row %d: %v", i, err)
		}
	}

	// After: BacklogStats must not change — unbillable rows are excluded by the Stripe filter.
	afterUnits, afterRows, _, err := rec.BacklogStats(ctx)
	if err != nil {
		t.Fatalf("BacklogStats after insert: %v", err)
	}
	if afterUnits != baseUnits {
		t.Errorf("unbillable rows inflated BacklogStats units: before=%d after=%d (delta=%d)",
			baseUnits, afterUnits, afterUnits-baseUnits)
	}
	if afterRows != baseRows {
		t.Errorf("unbillable rows inflated BacklogStats rows: before=%d after=%d (delta=%d)",
			baseRows, afterRows, afterRows-baseRows)
	}
}

// TestUnbillableUsage_noStripeCustomer seeds unflushed rows for a customer without a
// stripe_customer_id and asserts UnbillableUsage returns the correct delta counts.
// Uses a before/after delta so the assertion is deterministic on a shared DB.
func TestUnbillableUsage_noStripeCustomer(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	// setupTestCustomer leaves stripe_customer_id NULL — permanently unbillable.
	custID, apiKeyID := setupTestCustomer(t, pool)
	t.Cleanup(func() { deleteUsageRows(t, pool, custID) })

	rec := NewReconciler(pool)

	// Baseline before inserting our row.
	baseUnits, baseRows, err := rec.UnbillableUsage(ctx)
	if err != nil {
		t.Fatalf("UnbillableUsage baseline: %v", err)
	}

	const wantDelta = int64(42)
	reqID := "req-ub-nostripe-" + custID.String()
	if _, err := pool.Exec(ctx,
		`INSERT INTO usage_events (customer_id, api_key_id, operation, billable_units, request_id)
		 VALUES ($1, $2, 'ub.nostripe', $3, $4)`,
		custID, apiKeyID, wantDelta, reqID,
	); err != nil {
		t.Fatalf("insert row: %v", err)
	}

	// After: delta must be exactly our one unbillable row.
	units, rows, err := rec.UnbillableUsage(ctx)
	if err != nil {
		t.Fatalf("UnbillableUsage after insert: %v", err)
	}
	if units-baseUnits != wantDelta {
		t.Errorf("UnbillableUsage units delta = %d, want %d", units-baseUnits, wantDelta)
	}
	if rows-baseRows != 1 {
		t.Errorf("UnbillableUsage rows delta = %d, want 1", rows-baseRows)
	}
}

// TestUnbillableUsage_stripeCustomerExcluded verifies that rows for customers WITH a
// stripe_customer_id are NOT reported by UnbillableUsage. Uses a before/after delta
// so the assertion is deterministic on a shared DB.
func TestUnbillableUsage_stripeCustomerExcluded(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	custID, apiKeyID := setupTestCustomer(t, pool)
	t.Cleanup(func() { deleteUsageRows(t, pool, custID) })
	stripeID := "cus_ub_excl_" + custID.String()
	if _, err := pool.Exec(ctx,
		`UPDATE customers SET stripe_customer_id=$1 WHERE id=$2`, stripeID, custID,
	); err != nil {
		t.Fatalf("set stripe_customer_id: %v", err)
	}

	rec := NewReconciler(pool)

	// Baseline before inserting the stripe-linked customer's row.
	baseUnits, baseRows, err := rec.UnbillableUsage(ctx)
	if err != nil {
		t.Fatalf("UnbillableUsage baseline: %v", err)
	}

	// Insert an unflushed row — customer has Stripe ID, so should be EXCLUDED from unbillable.
	if _, err := pool.Exec(ctx,
		`INSERT INTO usage_events (customer_id, api_key_id, operation, billable_units, request_id)
		 VALUES ($1, $2, 'ub.excl', 100, $3)`,
		custID, apiKeyID, "req-ub-excl-"+custID.String(),
	); err != nil {
		t.Fatalf("insert row: %v", err)
	}

	// After: delta must be zero — stripe-linked customer is excluded from unbillable.
	units, rows, err := rec.UnbillableUsage(ctx)
	if err != nil {
		t.Fatalf("UnbillableUsage after insert: %v", err)
	}
	if units != baseUnits {
		t.Errorf("UnbillableUsage units changed by Stripe-linked row: before=%d after=%d", baseUnits, units)
	}
	if rows != baseRows {
		t.Errorf("UnbillableUsage rows changed by Stripe-linked row: before=%d after=%d", baseRows, rows)
	}
}

// TestFlusher_reconcileErrorDoesNotAbortPhases verifies that setBacklogGauges failures
// (closed pool → all reconcile queries fail) do not prevent retryPendingBatches and
// claimAndEmitNewBatches from running or Stripe from being called. The three methods
// are tested directly rather than via Run() so the reconciler pool can be swapped
// independently of the flush pool. Must not call t.Parallel() — this test writes to
// package-level promauto gauges shared across the test process and is not safe for
// concurrent execution.
func TestFlusher_reconcileErrorDoesNotAbortPhases(t *testing.T) {
	// Save and restore global gauge state so other sequential tests see a clean slate.
	// Must not call t.Parallel() — package-level promauto gauges are shared process-wide.
	prevUnits := testutil.ToFloat64(observability.BillingBacklogUnits)
	prevRows := testutil.ToFloat64(observability.BillingBacklogRows)
	prevAge := testutil.ToFloat64(observability.BillingBacklogOldestAgeSeconds)
	prevUnbillable := testutil.ToFloat64(observability.BillingUnbillableUnits)
	prevUnbillableRows := testutil.ToFloat64(observability.BillingUnbillableRows)
	t.Cleanup(func() {
		observability.BillingBacklogUnits.Set(prevUnits)
		observability.BillingBacklogRows.Set(prevRows)
		observability.BillingBacklogOldestAgeSeconds.Set(prevAge)
		observability.BillingUnbillableUnits.Set(prevUnbillable)
		observability.BillingUnbillableRows.Set(prevUnbillableRows)
	})
	// Set to non-zero gaugePreservationSentinels so we can prove the error path PRESERVES these values
	// rather than resetting them to 0. If the code incorrectly did Set(0) on error,
	// the assertions below would catch it.
	const gaugePreservationSentinel = float64(42)
	observability.BillingBacklogUnits.Set(gaugePreservationSentinel)
	observability.BillingBacklogRows.Set(gaugePreservationSentinel)
	observability.BillingBacklogOldestAgeSeconds.Set(gaugePreservationSentinel)
	observability.BillingUnbillableUnits.Set(gaugePreservationSentinel)
	observability.BillingUnbillableRows.Set(gaugePreservationSentinel)

	pool := newTestPool(t)
	ctx := context.Background()

	custID, apiKeyID := setupTestCustomer(t, pool)
	t.Cleanup(func() { deleteUsageRows(t, pool, custID) })

	stripeID := "cus_recerr_" + custID.String()
	if _, err := pool.Exec(ctx,
		`UPDATE customers SET stripe_customer_id=$1 WHERE id=$2`, stripeID, custID,
	); err != nil {
		t.Fatalf("set stripe_customer_id: %v", err)
	}

	// Phase-A seed: a row with batch_id already stamped but not yet flushed.
	// retryPendingBatches queries WHERE batch_id IS NOT NULL.
	pendingBatchID := uuid.New()
	reqA := "req-recerr-A-" + custID.String()
	if _, err := pool.Exec(ctx,
		`INSERT INTO usage_events (customer_id, api_key_id, operation, billable_units, request_id, batch_id)
		 VALUES ($1, $2, 'recerr.phaseA', 5, $3, $4)`,
		custID, apiKeyID, reqA, pendingBatchID,
	); err != nil {
		t.Fatalf("insert phase-A row: %v", err)
	}

	// Phase-B seed: an unbatched row (batch_id IS NULL).
	// claimAndEmitNewBatches queries WHERE batch_id IS NULL.
	reqB := "req-recerr-B-" + custID.String()
	if _, err := pool.Exec(ctx,
		`INSERT INTO usage_events (customer_id, api_key_id, operation, billable_units, request_id)
		 VALUES ($1, $2, 'recerr.phaseB', 3, $3)`,
		custID, apiKeyID, reqB,
	); err != nil {
		t.Fatalf("insert phase-B row: %v", err)
	}

	mock := &mockStripeMeter{}
	f := NewFlusher(pool, mock, 0)
	// Inject a deterministic stub that always fails — avoids relying on closed-pool
	// timing semantics and makes the failure injection unconditionally reliable.
	f.reconciler = &stubReconciler{err: errors.New("injected reconcile failure")}

	// Inject a fresh counter so this test doesn't pollute the global BillingReconcileErrorsTotal.
	// The counter starts at 0, so we can assert an absolute value of 1 after setBacklogGauges.
	freshErrCounter := prometheus.NewCounter(prometheus.CounterOpts{
		Name: "crucible_test_reconcile_errors_for_" + t.Name(),
		Help: "test-only counter for reconcile errors",
	})
	f.reconcileErrCounter = freshErrCounter

	// Run both phases then reconcile — must not panic or return an error to the caller.
	if err := f.retryPendingBatches(ctx); err != nil {
		t.Fatalf("retryPendingBatches: %v", err)
	}
	if err := f.claimAndEmitNewBatches(ctx); err != nil {
		t.Fatalf("claimAndEmitNewBatches: %v", err)
	}
	// Safety cap: 3×reconcileQueryTimeout keeps the test aligned with the production
	// constant — if reconcileQueryTimeout changes this scales automatically.
	gaugeCtx, gaugeCancel := context.WithTimeout(ctx, 3*reconcileQueryTimeout)
	defer gaugeCancel()
	f.setBacklogGauges(gaugeCtx) // must not panic; errors are warnings only

	// Both queries fail (injected stub error). The counter must be incremented exactly once (not twice) —
	// the once-per-tick semantic prevents double-counting when both BacklogStats and UnbillableUsage fail.
	// freshErrCounter starts at 0, so we assert an absolute value of 1 with no global state dependency.
	if got := testutil.ToFloat64(freshErrCounter); got != 1 {
		t.Errorf("reconcileErrCounter = %g, want 1 (incremented once per tick even when both queries fail)", got)
	}

	// Both queries fail (injected stub); error path must PRESERVE gauge values at the gaugePreservationSentinel
	// (42). If the code incorrectly reset them to 0, the assertions below would fail.
	// Resetting to 0 on error makes a DB timeout indistinguishable from an empty backlog
	// and would clear active Prometheus alerts — so we prove that does NOT happen.
	if got := testutil.ToFloat64(observability.BillingBacklogUnits); got != gaugePreservationSentinel {
		t.Errorf("BillingBacklogUnits = %g after reconcile error, want %g (gaugePreservationSentinel preserved)", got, gaugePreservationSentinel)
	}
	if got := testutil.ToFloat64(observability.BillingBacklogRows); got != gaugePreservationSentinel {
		t.Errorf("BillingBacklogRows = %g after reconcile error, want %g (gaugePreservationSentinel preserved)", got, gaugePreservationSentinel)
	}
	if got := testutil.ToFloat64(observability.BillingBacklogOldestAgeSeconds); got != gaugePreservationSentinel {
		t.Errorf("BillingBacklogOldestAgeSeconds = %g after reconcile error, want %g (gaugePreservationSentinel preserved)", got, gaugePreservationSentinel)
	}
	if got := testutil.ToFloat64(observability.BillingUnbillableUnits); got != gaugePreservationSentinel {
		t.Errorf("BillingUnbillableUnits = %g after reconcile error, want %g (gaugePreservationSentinel preserved)", got, gaugePreservationSentinel)
	}
	if got := testutil.ToFloat64(observability.BillingUnbillableRows); got != gaugePreservationSentinel {
		t.Errorf("BillingUnbillableRows = %g after reconcile error, want %g (gaugePreservationSentinel preserved)", got, gaugePreservationSentinel)
	}

	// Both phases must have fired independently of the reconcile failure.
	// Phase A (retryPendingBatches) uses the stable batch_id we pre-stamped.
	// Phase B (claimAndEmitNewBatches) allocates a new batch_id for the unbatched row.

	// Verify phase B actually claimed the row by reading its assigned batch_id from the DB.
	var batchBPtr *uuid.UUID
	if err := pool.QueryRow(ctx,
		`SELECT batch_id FROM usage_events WHERE request_id=$1`, reqB,
	).Scan(&batchBPtr); err != nil {
		t.Fatalf("query phase-B batch_id: %v", err)
	}
	if batchBPtr == nil {
		t.Fatal("phase B did not claim the unbatched row (batch_id IS NULL)")
	}

	wantKeyA := "crucible-batch-" + pendingBatchID.String()
	wantKeyB := "crucible-batch-" + batchBPtr.String()
	var foundA, foundB bool
	for _, c := range mock.calls {
		if c.stripeCustomerID != stripeID {
			continue
		}
		if c.idempotencyKey == wantKeyA {
			foundA = true
		} else if c.idempotencyKey == wantKeyB {
			foundB = true
		}
	}
	if !foundA {
		t.Error("Phase A (retryPendingBatches) did not call Stripe; reconcile failure must not abort flush phases")
	}
	if !foundB {
		t.Error("Phase B (claimAndEmitNewBatches) did not call Stripe; reconcile failure must not abort flush phases")
	}
}

// TestSetBacklogGauges_setsGauges verifies that setBacklogGauges writes real values into the
// Prometheus gauges on a success path (not just that errors leave them at 0). Must not call
// t.Parallel() — writes to package-level promauto gauges shared process-wide.
//
// Delta design: take a baseline before inserting test rows, then verify the gauge
// increased by exactly the expected amount (2 rows × 5 units = 10 units). This proves
// the reconcile query counts our specific rows correctly, independent of any pre-existing
// rows from other tests or prior runs.
func TestSetBacklogGauges_setsGauges(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	custID, apiKeyID := setupTestCustomer(t, pool)
	t.Cleanup(func() { deleteUsageRows(t, pool, custID) })

	stripeID := "cus_gauge_ok_" + custID.String()
	if _, err := pool.Exec(ctx,
		`UPDATE customers SET stripe_customer_id=$1 WHERE id=$2`, stripeID, custID,
	); err != nil {
		t.Fatalf("set stripe_customer_id: %v", err)
	}

	// Save and restore all gauges so other sequential tests see a clean slate.
	prevUnits := testutil.ToFloat64(observability.BillingBacklogUnits)
	prevRows := testutil.ToFloat64(observability.BillingBacklogRows)
	prevAge := testutil.ToFloat64(observability.BillingBacklogOldestAgeSeconds)
	prevUnbillable := testutil.ToFloat64(observability.BillingUnbillableUnits)
	prevUnbillableRows := testutil.ToFloat64(observability.BillingUnbillableRows)
	t.Cleanup(func() {
		observability.BillingBacklogUnits.Set(prevUnits)
		observability.BillingBacklogRows.Set(prevRows)
		observability.BillingBacklogOldestAgeSeconds.Set(prevAge)
		observability.BillingUnbillableUnits.Set(prevUnbillable)
		observability.BillingUnbillableRows.Set(prevUnbillableRows)
	})

	// Baseline BEFORE inserting our rows: accounts for any pre-existing unflushed rows
	// in the shared DB so the delta assertions below are deterministic.
	rec := NewReconciler(pool)
	baseUnits, baseRows, _, err := rec.BacklogStats(ctx)
	if err != nil {
		t.Fatalf("BacklogStats baseline: %v", err)
	}
	baseUbUnits, baseUbRows, err := rec.UnbillableUsage(ctx)
	if err != nil {
		t.Fatalf("UnbillableUsage baseline: %v", err)
	}

	// Insert 2 unflushed rows × 5 units = 10 units for our Stripe-linked customer.
	for i := 0; i < 2; i++ {
		reqID := "req-gauge-ok-" + custID.String() + strconv.Itoa(i)
		if _, err := pool.Exec(ctx,
			`INSERT INTO usage_events (customer_id, api_key_id, operation, billable_units, request_id)
			 VALUES ($1, $2, 'gauge.ok', 5, $3)`,
			custID, apiKeyID, reqID,
		); err != nil {
			t.Fatalf("insert row %d: %v", i, err)
		}
	}

	f := NewFlusher(pool, &mockStripeMeter{}, 0)
	f.setBacklogGauges(ctx)

	// Our 2 rows × 5 units must appear as an exact delta over the baseline.
	if got := int64(testutil.ToFloat64(observability.BillingBacklogRows)) - baseRows; got != 2 {
		t.Errorf("BillingBacklogRows delta = %d, want 2", got)
	}
	if got := int64(testutil.ToFloat64(observability.BillingBacklogUnits)) - baseUnits; got != 10 {
		t.Errorf("BillingBacklogUnits delta = %d, want 10 (2 rows × 5 units)", got)
	}
	gotAge := testutil.ToFloat64(observability.BillingBacklogOldestAgeSeconds)
	// Verify gauge matches the live DB state: call BacklogStats again and check the gauge
	// is within 2s of the fresh query result. Both queries run immediately after
	// setBacklogGauges, so clock drift between the two is negligible.
	_, _, dbAgeRef, ageErr := rec.BacklogStats(ctx)
	if ageErr != nil {
		t.Fatalf("BacklogStats age reference: %v", ageErr)
	}
	if gotAge <= 0 {
		t.Errorf("BillingBacklogOldestAgeSeconds = %g, want > 0 (unflushed rows exist)", gotAge)
	} else if dbAgeRef > 0 && math.Abs(gotAge-dbAgeRef) > 2.0 {
		t.Errorf("BillingBacklogOldestAgeSeconds gauge = %g, reconciler = %g (delta > 2s)", gotAge, dbAgeRef)
	}
	// Our customer is Stripe-linked, so unbillable gauges must not change.
	if got := int64(testutil.ToFloat64(observability.BillingUnbillableUnits)) - baseUbUnits; got != 0 {
		t.Errorf("BillingUnbillableUnits delta = %d, want 0 (Stripe-linked customer)", got)
	}
	if got := int64(testutil.ToFloat64(observability.BillingUnbillableRows)) - baseUbRows; got != 0 {
		t.Errorf("BillingUnbillableRows delta = %d, want 0 (Stripe-linked customer)", got)
	}
}
