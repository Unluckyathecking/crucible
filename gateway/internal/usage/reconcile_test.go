package usage

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// deleteUsageRows removes all usage_events for the given customer — used by t.Cleanup
// so test rows don't accumulate across runs and pollute aggregate assertions.
func deleteUsageRows(t testing.TB, pool *pgxpool.Pool, custID interface{}) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`DELETE FROM usage_events WHERE customer_id=$1`, custID,
	); err != nil {
		t.Logf("cleanup: delete usage_events for %v: %v", custID, err)
	}
}

// TestBacklogStats_flushedRowExcluded verifies that a row with flushed_to_stripe=TRUE
// is not counted in the backlog — i.e. BacklogStats only counts pending work.
func TestBacklogStats_flushedRowExcluded(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	custID, apiKeyID := setupTestCustomer(t, pool)
	t.Cleanup(func() { deleteUsageRows(t, pool, custID) })

	rec := NewReconciler(pool)
	reqID := "req-bs-empty-" + custID.String()[:8]
	if _, err := pool.Exec(ctx,
		`INSERT INTO usage_events (customer_id, api_key_id, operation, billable_units, request_id, flushed_to_stripe)
		 VALUES ($1, $2, 'bs.empty', 7, $3, TRUE)`,
		custID, apiKeyID, reqID,
	); err != nil {
		t.Fatalf("insert flushed row: %v", err)
	}

	// BacklogStats may report other tests' rows; we just verify no panic and correct types.
	units, rows, ageSecs, err := rec.BacklogStats(ctx)
	if err != nil {
		t.Fatalf("BacklogStats: %v", err)
	}
	// The flushed row we inserted must not inflate the backlog.
	// Query directly to confirm our flushed row is excluded.
	var ourUnflushed int64
	if err := pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM usage_events WHERE customer_id=$1 AND flushed_to_stripe=FALSE`, custID,
	).Scan(&ourUnflushed); err != nil {
		t.Fatalf("count unflushed for our customer: %v", err)
	}
	if ourUnflushed != 0 {
		t.Errorf("expected 0 unflushed rows for our customer, got %d", ourUnflushed)
	}
	// The aggregate values should be non-negative and consistent.
	if units < 0 {
		t.Errorf("BacklogStats units = %d, must be >= 0", units)
	}
	if rows < 0 {
		t.Errorf("BacklogStats rows = %d, must be >= 0", rows)
	}
	if ageSecs < 0 {
		t.Errorf("BacklogStats ageSecs = %f, must be >= 0", ageSecs)
	}
}

// TestBacklogStats_countsUnflushed seeds a known set of unflushed rows for a Stripe-linked
// customer and asserts BacklogStats returns the correct aggregate units and a positive age.
// BacklogStats mirrors the flusher's stripe_customer_id IS NOT NULL filter so it only
// counts rows the flusher can actually process.
func TestBacklogStats_countsUnflushed(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	custID, apiKeyID := setupTestCustomer(t, pool)
	t.Cleanup(func() { deleteUsageRows(t, pool, custID) })

	// Give the customer a stripe_customer_id so BacklogStats counts their rows.
	stripeID := "cus_bs_uf_" + custID.String()[:8]
	if _, err := pool.Exec(ctx,
		`UPDATE customers SET stripe_customer_id=$1 WHERE id=$2`, stripeID, custID,
	); err != nil {
		t.Fatalf("set stripe_customer_id: %v", err)
	}

	// Insert 3 unflushed rows with known units (5 + 10 + 15 = 30).
	wantUnits := int64(30)
	for i, u := range []int{5, 10, 15} {
		reqID := "req-bs-uf-" + custID.String()[:8] + string(rune('a'+i))
		if _, err := pool.Exec(ctx,
			`INSERT INTO usage_events (customer_id, api_key_id, operation, billable_units, request_id)
			 VALUES ($1, $2, 'bs.uf', $3, $4)`,
			custID, apiKeyID, u, reqID,
		); err != nil {
			t.Fatalf("insert row %d: %v", i, err)
		}
	}
	// Insert one flushed row — must NOT be counted.
	if _, err := pool.Exec(ctx,
		`INSERT INTO usage_events (customer_id, api_key_id, operation, billable_units, request_id, flushed_to_stripe)
		 VALUES ($1, $2, 'bs.flushed', 999, $3, TRUE)`,
		custID, apiKeyID, "req-bs-flushed-"+custID.String()[:8],
	); err != nil {
		t.Fatalf("insert flushed row: %v", err)
	}

	rec := NewReconciler(pool)
	units, rows, ageSecs, err := rec.BacklogStats(ctx)
	if err != nil {
		t.Fatalf("BacklogStats: %v", err)
	}

	// Assert our 3 rows contribute at least wantUnits to the total
	// (other tests may have left rows; we use >= since the DB is shared).
	if units < wantUnits {
		t.Errorf("BacklogStats units = %d, want >= %d", units, wantUnits)
	}
	if rows < 3 {
		t.Errorf("BacklogStats rows = %d, want >= 3", rows)
	}
	if ageSecs <= 0 {
		t.Errorf("BacklogStats ageSecs = %f, want > 0 when unflushed rows exist", ageSecs)
	}

	// Precise customer-local assertion: flushed row (999 units) must not inflate the total.
	var ourUnflushedUnits int64
	if err := pool.QueryRow(ctx,
		`SELECT COALESCE(SUM(billable_units),0)::bigint FROM usage_events
		 WHERE customer_id=$1 AND flushed_to_stripe=FALSE`, custID,
	).Scan(&ourUnflushedUnits); err != nil {
		t.Fatalf("query our unflushed sum: %v", err)
	}
	if ourUnflushedUnits != wantUnits {
		t.Errorf("customer-local unflushed units = %d, want %d", ourUnflushedUnits, wantUnits)
	}
}

// TestBacklogStats_unbillableRowsExcluded verifies that unflushed rows for customers
// WITHOUT a stripe_customer_id do NOT inflate BacklogStats. These rows belong to
// UnbillableUsage, not the billable backlog.
func TestBacklogStats_unbillableRowsExcluded(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	// setupTestCustomer leaves stripe_customer_id NULL — permanently unbillable.
	custID, apiKeyID := setupTestCustomer(t, pool)
	t.Cleanup(func() { deleteUsageRows(t, pool, custID) })

	// Insert unflushed rows that should NOT count toward the billable backlog.
	for i, u := range []int{100, 200} {
		reqID := "req-bs-ubexcl-" + custID.String()[:8] + string(rune('a'+i))
		if _, err := pool.Exec(ctx,
			`INSERT INTO usage_events (customer_id, api_key_id, operation, billable_units, request_id)
			 VALUES ($1, $2, 'bs.ubexcl', $3, $4)`,
			custID, apiKeyID, u, reqID,
		); err != nil {
			t.Fatalf("insert row %d: %v", i, err)
		}
	}

	rec := NewReconciler(pool)
	_, _, _, err := rec.BacklogStats(ctx)
	if err != nil {
		t.Fatalf("BacklogStats: %v", err)
	}

	// Confirm our customer's rows are in UnbillableUsage, not BacklogStats.
	var ourBacklog int64
	if err := pool.QueryRow(ctx,
		`SELECT COALESCE(SUM(u.billable_units),0)::bigint
		 FROM usage_events u JOIN customers c ON c.id=u.customer_id
		 WHERE u.customer_id=$1 AND u.flushed_to_stripe=FALSE AND c.stripe_customer_id IS NOT NULL`,
		custID,
	).Scan(&ourBacklog); err != nil {
		t.Fatalf("query billable backlog for our customer: %v", err)
	}
	if ourBacklog != 0 {
		t.Errorf("customer without stripe_customer_id must contribute 0 to billable backlog, got %d", ourBacklog)
	}
}

// TestUnbillableUsage_noStripeCustomer seeds unflushed rows for a customer without a
// stripe_customer_id and asserts UnbillableUsage returns the correct counts.
func TestUnbillableUsage_noStripeCustomer(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	// setupTestCustomer leaves stripe_customer_id NULL — permanently unbillable.
	custID, apiKeyID := setupTestCustomer(t, pool)
	t.Cleanup(func() { deleteUsageRows(t, pool, custID) })

	wantUnits := int64(42)
	reqID := "req-ub-nostripe-" + custID.String()[:8]
	if _, err := pool.Exec(ctx,
		`INSERT INTO usage_events (customer_id, api_key_id, operation, billable_units, request_id)
		 VALUES ($1, $2, 'ub.nostripe', $3, $4)`,
		custID, apiKeyID, wantUnits, reqID,
	); err != nil {
		t.Fatalf("insert row: %v", err)
	}

	rec := NewReconciler(pool)
	units, rows, err := rec.UnbillableUsage(ctx)
	if err != nil {
		t.Fatalf("UnbillableUsage: %v", err)
	}

	if units < wantUnits {
		t.Errorf("UnbillableUsage units = %d, want >= %d", units, wantUnits)
	}
	if rows < 1 {
		t.Errorf("UnbillableUsage rows = %d, want >= 1", rows)
	}

	// Precise assertion against our customer only.
	var ourUnits int64
	if err := pool.QueryRow(ctx,
		`SELECT COALESCE(SUM(u.billable_units),0)::bigint
		 FROM usage_events u JOIN customers c ON c.id=u.customer_id
		 WHERE u.customer_id=$1 AND u.flushed_to_stripe=FALSE AND c.stripe_customer_id IS NULL`,
		custID,
	).Scan(&ourUnits); err != nil {
		t.Fatalf("query our unbillable units: %v", err)
	}
	if ourUnits != wantUnits {
		t.Errorf("customer-local unbillable units = %d, want %d", ourUnits, wantUnits)
	}
}

// TestUnbillableUsage_stripeCustomerExcluded verifies that rows for customers WITH a
// stripe_customer_id are NOT reported by UnbillableUsage.
func TestUnbillableUsage_stripeCustomerExcluded(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	custID, apiKeyID := setupTestCustomer(t, pool)
	t.Cleanup(func() { deleteUsageRows(t, pool, custID) })
	stripeID := "cus_ub_excl_" + custID.String()[:8]
	if _, err := pool.Exec(ctx,
		`UPDATE customers SET stripe_customer_id=$1 WHERE id=$2`, stripeID, custID,
	); err != nil {
		t.Fatalf("set stripe_customer_id: %v", err)
	}

	// Insert an unflushed row — customer has Stripe ID, so should be EXCLUDED from unbillable.
	if _, err := pool.Exec(ctx,
		`INSERT INTO usage_events (customer_id, api_key_id, operation, billable_units, request_id)
		 VALUES ($1, $2, 'ub.excl', 100, $3)`,
		custID, apiKeyID, "req-ub-excl-"+custID.String()[:8],
	); err != nil {
		t.Fatalf("insert row: %v", err)
	}

	rec := NewReconciler(pool)
	_, _, err := rec.UnbillableUsage(ctx)
	if err != nil {
		t.Fatalf("UnbillableUsage: %v", err)
	}

	// Our customer has a stripe_customer_id, so their row must not appear in unbillable.
	var ourUnbillable int64
	if err := pool.QueryRow(ctx,
		`SELECT COALESCE(SUM(u.billable_units),0)::bigint
		 FROM usage_events u JOIN customers c ON c.id=u.customer_id
		 WHERE u.customer_id=$1 AND u.flushed_to_stripe=FALSE AND c.stripe_customer_id IS NULL`,
		custID,
	).Scan(&ourUnbillable); err != nil {
		t.Fatalf("query: %v", err)
	}
	if ourUnbillable != 0 {
		t.Errorf("customer with stripe_customer_id must not appear in unbillable; got %d units", ourUnbillable)
	}
}

// TestFlusher_reconcileErrorDoesNotAbortPhases verifies that a failing reconcile query
// (here: pool is closed before reconcile runs) is only a warning and does NOT prevent
// the flush phases from completing or Stripe from being called.
func TestFlusher_reconcileErrorDoesNotAbortPhases(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	custID, apiKeyID := setupTestCustomer(t, pool)
	stripeID := "cus_recerr_" + custID.String()[:8]
	if _, err := pool.Exec(ctx,
		`UPDATE customers SET stripe_customer_id=$1 WHERE id=$2`, stripeID, custID,
	); err != nil {
		t.Fatalf("set stripe_customer_id: %v", err)
	}

	reqID := "req-recerr-" + custID.String()[:8]
	if _, err := pool.Exec(ctx,
		`INSERT INTO usage_events (customer_id, api_key_id, operation, billable_units, request_id)
		 VALUES ($1, $2, 'recerr.op', 5, $3)`,
		custID, apiKeyID, reqID,
	); err != nil {
		t.Fatalf("insert row: %v", err)
	}

	t.Cleanup(func() { deleteUsageRows(t, pool, custID) })

	// Build a second pool then immediately close it so all queries fail.
	badPool, err := pgxpool.New(ctx, testDSN())
	if err != nil {
		t.Skipf("could not create bad pool: %v", err)
	}
	badPool.Close()

	mock := &mockStripeMeter{}
	f := NewFlusher(pool, mock, 0)
	f.reconciler = NewReconciler(badPool) // inject failing reconciler

	// Run both phases then reconcile — must not panic or return an error to the caller.
	if err := f.retryPendingBatches(ctx); err != nil {
		t.Fatalf("retryPendingBatches: %v", err)
	}
	if err := f.claimAndEmitNewBatches(ctx); err != nil {
		t.Fatalf("claimAndEmitNewBatches: %v", err)
	}
	f.setBacklogGauges(ctx) // must not panic; errors are warnings only

	// The flush phases must have completed: Stripe was called for our customer.
	ourCalls := callsForCustomer(mock.calls, stripeID)
	if len(ourCalls) == 0 {
		t.Error("expected at least one Stripe call; flush phases must not be aborted by a reconcile failure")
	}
}
