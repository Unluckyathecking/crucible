package usage

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// ---- flusher phase tests ----
//
// These tests share the same Postgres instance and run with -shuffle=on.
// To stay order-independent we NEVER assert on global Stripe call counts.
// Instead every assertion targets only the stripe_customer_id of the customer
// created within the test. That customer UUID is unique per test run so
// cross-test pollution is impossible.

// callsForCustomer returns all Stripe calls that target the given stripe_customer_id.
func callsForCustomer(calls []meterCall, stripeID string) []meterCall {
	var out []meterCall
	for _, c := range calls {
		if c.stripeCustomerID == stripeID {
			out = append(out, c)
		}
	}
	return out
}

// TestRetryPendingBatches_reusesExistingBatchID verifies that phase-A (retryPendingBatches)
// emits the SAME batch UUID that was already stamped on the rows, rather than allocating a
// new one. This is the idempotency guarantee that prevents Stripe double-billing on retry.
func TestRetryPendingBatches_reusesExistingBatchID(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	// Create a customer with a stripe_customer_id (required for flusher JOIN).
	custID, apiKeyID := setupTestCustomer(t, pool)
	stripeID := "cus_retry_" + custID.String()[:8]
	if _, err := pool.Exec(ctx,
		`UPDATE customers SET stripe_customer_id=$1 WHERE id=$2`,
		stripeID, custID,
	); err != nil {
		t.Fatalf("set stripe_customer_id: %v", err)
	}

	// Insert two usage events and stamp a known batch_id (simulating a crash after claim).
	batchID := uuid.New()
	for i, reqID := range []string{"req-retry-a-" + custID.String()[:8], "req-retry-b-" + custID.String()[:8]} {
		if _, err := pool.Exec(ctx,
			`INSERT INTO usage_events (customer_id, api_key_id, operation, billable_units, request_id, batch_id)
			 VALUES ($1, $2, 'retry.op', $3, $4, $5)`,
			custID, apiKeyID, uint64(i+1)*10, reqID, batchID,
		); err != nil {
			t.Fatalf("insert row %d: %v", i, err)
		}
	}

	mock := &mockStripeMeter{}
	f := NewFlusher(pool, mock, 0)

	if err := f.retryPendingBatches(ctx); err != nil {
		t.Fatalf("retryPendingBatches: %v", err)
	}

	// Find calls for our customer — must be exactly one.
	ourCalls := callsForCustomer(mock.calls, stripeID)
	if len(ourCalls) != 1 {
		t.Fatalf("expected 1 Stripe call for %s, got %d", stripeID, len(ourCalls))
	}

	call := ourCalls[0]

	// The idempotency key must reuse the SAME batch UUID — not a fresh one.
	wantKey := "crucible-batch-" + batchID.String()
	if call.idempotencyKey != wantKey {
		t.Errorf("idempotencyKey = %q, want %q (same batch_id for Stripe dedup)", call.idempotencyKey, wantKey)
	}

	// Units should be the sum of both rows: 10 + 20 = 30.
	if call.units != 30 {
		t.Errorf("units = %d, want 30", call.units)
	}

	// After a successful retry the rows must be marked flushed.
	var unflushed int
	if err := pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM usage_events WHERE batch_id=$1 AND flushed_to_stripe=FALSE`,
		batchID,
	).Scan(&unflushed); err != nil {
		t.Fatalf("count unflushed: %v", err)
	}
	if unflushed != 0 {
		t.Errorf("expected 0 unflushed rows after retry, got %d", unflushed)
	}
}

// TestRetryPendingBatches_alreadyFlushedSkipped checks that rows with flushed_to_stripe=TRUE
// are excluded even when they carry a batch_id.
func TestRetryPendingBatches_alreadyFlushedSkipped(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	custID, apiKeyID := setupTestCustomer(t, pool)
	stripeID := "cus_skip_" + custID.String()[:8]
	if _, err := pool.Exec(ctx,
		`UPDATE customers SET stripe_customer_id=$1 WHERE id=$2`, stripeID, custID,
	); err != nil {
		t.Fatalf("set stripe_customer_id: %v", err)
	}

	batchID := uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO usage_events (customer_id, api_key_id, operation, billable_units, request_id, batch_id, flushed_to_stripe)
		 VALUES ($1, $2, 'skip.op', 5, $3, $4, TRUE)`,
		custID, apiKeyID, "req-skip-"+custID.String()[:8], batchID,
	); err != nil {
		t.Fatalf("insert flushed row: %v", err)
	}

	mock := &mockStripeMeter{}
	f := NewFlusher(pool, mock, 0)

	if err := f.retryPendingBatches(ctx); err != nil {
		t.Fatalf("retryPendingBatches: %v", err)
	}

	// No call must target this customer — the row is already flushed.
	if got := callsForCustomer(mock.calls, stripeID); len(got) != 0 {
		t.Errorf("expected 0 calls for already-flushed customer, got %d", len(got))
	}
}

// TestRetryPendingBatches_noStripeCustomerSkipped confirms rows for customers without a
// stripe_customer_id (e.g. free-plan users who never upgraded) are excluded.
func TestRetryPendingBatches_noStripeCustomerSkipped(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	// setupTestCustomer leaves stripe_customer_id NULL.
	custID, apiKeyID := setupTestCustomer(t, pool)

	batchID := uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO usage_events (customer_id, api_key_id, operation, billable_units, request_id, batch_id)
		 VALUES ($1, $2, 'nostripe.op', 5, $3, $4)`,
		custID, apiKeyID, "req-nostripe-"+custID.String()[:8], batchID,
	); err != nil {
		t.Fatalf("insert row: %v", err)
	}

	mock := &mockStripeMeter{}
	f := NewFlusher(pool, mock, 0)

	if err := f.retryPendingBatches(ctx); err != nil {
		t.Fatalf("retryPendingBatches: %v", err)
	}

	// The row's customer has no stripe_customer_id so the query's JOIN excludes it.
	// Verify by checking that no call's idempotency key corresponds to our batch.
	wantKey := "crucible-batch-" + batchID.String()
	for _, c := range mock.calls {
		if c.idempotencyKey == wantKey {
			t.Errorf("got unexpected Stripe call for batch %s (customer has no stripe_customer_id)", batchID)
		}
	}
}

// TestRetryPendingBatches_stripeErrorDoesNotMark confirms that when Stripe returns an error
// during retry, the rows are NOT marked flushed (so the next tick retries with the same key).
func TestRetryPendingBatches_stripeErrorDoesNotMark(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	custID, apiKeyID := setupTestCustomer(t, pool)
	stripeID := "cus_errdm_" + custID.String()[:8]
	if _, err := pool.Exec(ctx,
		`UPDATE customers SET stripe_customer_id=$1 WHERE id=$2`, stripeID, custID,
	); err != nil {
		t.Fatalf("set stripe_customer_id: %v", err)
	}

	batchID := uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO usage_events (customer_id, api_key_id, operation, billable_units, request_id, batch_id)
		 VALUES ($1, $2, 'err.op', 7, $3, $4)`,
		custID, apiKeyID, "req-errdm-"+custID.String()[:8], batchID,
	); err != nil {
		t.Fatalf("insert row: %v", err)
	}

	// Stripe will fail for all calls.
	mock := &mockStripeMeter{err: errStripe}
	f := NewFlusher(pool, mock, 0)

	// Stripe failure propagates: retryPendingBatches returns an error summarising failed batches.
	if err := f.retryPendingBatches(ctx); err == nil {
		t.Errorf("expected error from retryPendingBatches when Stripe fails, got nil")
	}

	// Our customer must have been attempted (Stripe was called for our batch).
	ourCalls := callsForCustomer(mock.calls, stripeID)
	if len(ourCalls) == 0 {
		t.Fatal("expected Stripe call for our customer, got 0")
	}

	// The row must remain unflushed so the next tick re-tries with the SAME batch_id.
	var flushed bool
	if err := pool.QueryRow(ctx,
		`SELECT flushed_to_stripe FROM usage_events WHERE batch_id=$1 LIMIT 1`, batchID,
	).Scan(&flushed); err != nil {
		t.Fatalf("query flushed: %v", err)
	}
	if flushed {
		t.Error("row must stay unflushed when Stripe returns an error")
	}
}

// TestClaimAndEmitNewBatches_freshBatchUUID confirms that phase-B assigns a brand-new UUID
// to each customer's unbatched rows, so different customers never share a batch_id.
func TestClaimAndEmitNewBatches_freshBatchUUID(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	// Two customers, each with stripe_customer_id.
	cust1, key1 := setupTestCustomer(t, pool)
	cust2, key2 := setupTestCustomer(t, pool)

	stripe1 := "cus_fresh1_" + cust1.String()[:8]
	stripe2 := "cus_fresh2_" + cust2.String()[:8]
	for _, pair := range [][2]string{{stripe1, cust1.String()}, {stripe2, cust2.String()}} {
		if _, err := pool.Exec(ctx,
			`UPDATE customers SET stripe_customer_id=$1 WHERE id=$2`, pair[0], pair[1],
		); err != nil {
			t.Fatalf("set stripe_customer_id: %v", err)
		}
	}

	// Insert unbatched rows (batch_id IS NULL).
	for _, row := range []struct {
		cust   uuid.UUID
		apiKey uuid.UUID
		reqID  string
		units  uint64
	}{
		{cust1, key1, "req-claim-c1a-" + cust1.String()[:8], 10},
		{cust1, key1, "req-claim-c1b-" + cust1.String()[:8], 20},
		{cust2, key2, "req-claim-c2a-" + cust2.String()[:8], 5},
	} {
		if _, err := pool.Exec(ctx,
			`INSERT INTO usage_events (customer_id, api_key_id, operation, billable_units, request_id)
			 VALUES ($1, $2, 'claim.op', $3, $4)`,
			row.cust, row.apiKey, row.units, row.reqID,
		); err != nil {
			t.Fatalf("insert row %s: %v", row.reqID, err)
		}
	}

	mock := &mockStripeMeter{}
	f := NewFlusher(pool, mock, 0)

	if err := f.claimAndEmitNewBatches(ctx); err != nil {
		t.Fatalf("claimAndEmitNewBatches: %v", err)
	}

	// Exactly one call per our two customers.
	calls1 := callsForCustomer(mock.calls, stripe1)
	calls2 := callsForCustomer(mock.calls, stripe2)
	if len(calls1) != 1 {
		t.Fatalf("expected 1 Stripe call for %s, got %d", stripe1, len(calls1))
	}
	if len(calls2) != 1 {
		t.Fatalf("expected 1 Stripe call for %s, got %d", stripe2, len(calls2))
	}

	// Each idempotency key must have the "crucible-batch-" prefix.
	for _, call := range []meterCall{calls1[0], calls2[0]} {
		if !strings.HasPrefix(call.idempotencyKey, "crucible-batch-") {
			t.Errorf("idempotencyKey %q missing prefix", call.idempotencyKey)
		}
	}

	// The two idempotency keys must be DIFFERENT (different batch UUIDs per customer).
	if calls1[0].idempotencyKey == calls2[0].idempotencyKey {
		t.Error("two customers must get different batch UUIDs")
	}

	// All rows for our customers should now be marked flushed.
	for _, cust := range []uuid.UUID{cust1, cust2} {
		var unflushed int
		if err := pool.QueryRow(ctx,
			`SELECT COUNT(*) FROM usage_events WHERE customer_id=$1 AND flushed_to_stripe=FALSE`,
			cust,
		).Scan(&unflushed); err != nil {
			t.Fatalf("count unflushed: %v", err)
		}
		if unflushed != 0 {
			t.Errorf("customer %v: expected 0 unflushed rows, got %d", cust, unflushed)
		}
	}

	// The batch_id on cust1's rows must not be nil.
	var nullBatchCount int
	if err := pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM usage_events WHERE customer_id=$1 AND batch_id IS NULL`,
		cust1,
	).Scan(&nullBatchCount); err != nil {
		t.Fatalf("count null batch_id: %v", err)
	}
	if nullBatchCount != 0 {
		t.Errorf("expected all rows to have a batch_id after claim, got %d still NULL", nullBatchCount)
	}
}

// TestClaimAndEmitNewBatches_unitsSummedPerCustomer verifies that all of a customer's
// unbatched rows are summed into a single Stripe call (not one call per row).
func TestClaimAndEmitNewBatches_unitsSummedPerCustomer(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	custID, apiKeyID := setupTestCustomer(t, pool)
	stripeID := "cus_sum_" + custID.String()[:8]
	if _, err := pool.Exec(ctx,
		`UPDATE customers SET stripe_customer_id=$1 WHERE id=$2`, stripeID, custID,
	); err != nil {
		t.Fatalf("set stripe_customer_id: %v", err)
	}

	// Insert three unbatched rows with units 10, 20, 30 → total 60.
	for i, sfx := range []string{"a", "b", "c"} {
		reqID := "req-sum-" + sfx + "-" + custID.String()[:8]
		if _, err := pool.Exec(ctx,
			`INSERT INTO usage_events (customer_id, api_key_id, operation, billable_units, request_id)
			 VALUES ($1, $2, 'sum.op', $3, $4)`,
			custID, apiKeyID, uint64((i+1)*10), reqID,
		); err != nil {
			t.Fatalf("insert row %s: %v", reqID, err)
		}
	}

	mock := &mockStripeMeter{}
	f := NewFlusher(pool, mock, 0)

	if err := f.claimAndEmitNewBatches(ctx); err != nil {
		t.Fatalf("claimAndEmitNewBatches: %v", err)
	}

	ourCalls := callsForCustomer(mock.calls, stripeID)
	if len(ourCalls) != 1 {
		t.Fatalf("expected 1 Stripe call for %s, got %d", stripeID, len(ourCalls))
	}
	if ourCalls[0].units != 60 {
		t.Errorf("units = %d, want 60 (sum of all rows)", ourCalls[0].units)
	}
}

// TestClaimAndEmitNewBatches_noStripeCustomerSkipped confirms rows for customers without
// stripe_customer_id are excluded from phase-B.
func TestClaimAndEmitNewBatches_noStripeCustomerSkipped(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	custID, apiKeyID := setupTestCustomer(t, pool)
	// Do NOT set stripe_customer_id — it stays NULL.

	if _, err := pool.Exec(ctx,
		`INSERT INTO usage_events (customer_id, api_key_id, operation, billable_units, request_id)
		 VALUES ($1, $2, 'noscust.op', 3, $3)`,
		custID, apiKeyID, "req-noscust-"+custID.String()[:8],
	); err != nil {
		t.Fatalf("insert row: %v", err)
	}

	mock := &mockStripeMeter{}
	f := NewFlusher(pool, mock, 0)

	if err := f.claimAndEmitNewBatches(ctx); err != nil {
		t.Fatalf("claimAndEmitNewBatches: %v", err)
	}

	// After phase-B the row must still be unbatched (batch_id IS NULL) because the
	// customer has no stripe_customer_id so the query's GROUP BY excludes them.
	var batchID *uuid.UUID
	if err := pool.QueryRow(ctx,
		`SELECT batch_id FROM usage_events WHERE customer_id=$1 LIMIT 1`, custID,
	).Scan(&batchID); err != nil {
		t.Fatalf("query batch_id: %v", err)
	}
	if batchID != nil {
		t.Errorf("batch_id must remain NULL for customer without stripe_customer_id, got %v", batchID)
	}
}

// TestClaimAndEmitNewBatches_alreadyBatchedSkipped ensures rows that already have a batch_id
// (the crash-recovery scenario) are left to retryPendingBatches and not re-claimed.
func TestClaimAndEmitNewBatches_alreadyBatchedSkipped(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	custID, apiKeyID := setupTestCustomer(t, pool)
	stripeID := "cus_clmd_" + custID.String()[:8]
	if _, err := pool.Exec(ctx,
		`UPDATE customers SET stripe_customer_id=$1 WHERE id=$2`, stripeID, custID,
	); err != nil {
		t.Fatalf("set stripe_customer_id: %v", err)
	}

	existingBatch := uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO usage_events (customer_id, api_key_id, operation, billable_units, request_id, batch_id)
		 VALUES ($1, $2, 'claimed.op', 5, $3, $4)`,
		custID, apiKeyID, "req-already-claimed-"+custID.String()[:8], existingBatch,
	); err != nil {
		t.Fatalf("insert row with batch_id: %v", err)
	}

	mock := &mockStripeMeter{}
	f := NewFlusher(pool, mock, 0)

	if err := f.claimAndEmitNewBatches(ctx); err != nil {
		t.Fatalf("claimAndEmitNewBatches: %v", err)
	}

	// Already-batched rows are not touched by phase-B.
	// Verify the batch_id is still the original — not overwritten.
	var gotBatch uuid.UUID
	if err := pool.QueryRow(ctx,
		`SELECT batch_id FROM usage_events WHERE customer_id=$1 AND batch_id IS NOT NULL LIMIT 1`, custID,
	).Scan(&gotBatch); err != nil {
		t.Fatalf("query batch_id: %v", err)
	}
	if gotBatch != existingBatch {
		t.Errorf("batch_id changed from %v to %v; phase-B must not overwrite existing batch_ids", existingBatch, gotBatch)
	}

	// And the row was not emitted via phase-B (existingBatch idempotency key not seen in claims).
	wantKey := "crucible-batch-" + existingBatch.String()
	for _, c := range mock.calls {
		if c.idempotencyKey == wantKey {
			t.Errorf("phase-B emitted already-batched row with key %s; should have been left for phase-A", wantKey)
		}
	}
}

// TestClaimAndEmitNewBatches_stripeErrorLeavesRowsBatched verifies that when Stripe fails
// during phase-B, the rows still have a batch_id stamped so the next tick's phase-A can
// retry them with the same UUID (Stripe idempotency).
func TestClaimAndEmitNewBatches_stripeErrorLeavesRowsBatched(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	custID, apiKeyID := setupTestCustomer(t, pool)
	stripeID := "cus_errb_" + custID.String()[:8]
	if _, err := pool.Exec(ctx,
		`UPDATE customers SET stripe_customer_id=$1 WHERE id=$2`, stripeID, custID,
	); err != nil {
		t.Fatalf("set stripe_customer_id: %v", err)
	}

	reqID := "req-clmerr-" + custID.String()[:8]
	if _, err := pool.Exec(ctx,
		`INSERT INTO usage_events (customer_id, api_key_id, operation, billable_units, request_id)
		 VALUES ($1, $2, 'clmerr.op', 9, $3)`,
		custID, apiKeyID, reqID,
	); err != nil {
		t.Fatalf("insert row: %v", err)
	}

	mock := &mockStripeMeter{err: errStripe}
	f := NewFlusher(pool, mock, 0)

	// Stripe failure propagates: claimAndEmitNewBatches returns an error summarising failed batches.
	if err := f.claimAndEmitNewBatches(ctx); err == nil {
		t.Errorf("expected error from claimAndEmitNewBatches when Stripe fails, got nil")
	}

	// Our customer must have been attempted.
	ourCalls := callsForCustomer(mock.calls, stripeID)
	if len(ourCalls) == 0 {
		t.Fatal("expected at least 1 Stripe call for our customer, got 0")
	}

	// The row should now have a batch_id (claimed) but flushed_to_stripe must be FALSE.
	var batchID *uuid.UUID
	var flushed bool
	if err := pool.QueryRow(ctx,
		`SELECT batch_id, flushed_to_stripe FROM usage_events WHERE request_id=$1 LIMIT 1`,
		reqID,
	).Scan(&batchID, &flushed); err != nil {
		t.Fatalf("query row: %v", err)
	}
	if batchID == nil {
		t.Fatal("batch_id must be set after claim even when Stripe fails")
	}
	if flushed {
		t.Error("flushed_to_stripe must remain FALSE when Stripe fails")
	}

	// The idempotency key used in the Stripe call for this customer must match the batch_id.
	wantKey := "crucible-batch-" + batchID.String()
	found := false
	for _, call := range ourCalls {
		if call.idempotencyKey == wantKey {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("none of the Stripe calls for %s used key %q (batch_id from the claimed row)", stripeID, wantKey)
	}
}

// TestTwoPhase_noDoubleCount is the end-to-end scenario that demonstrates the fix:
// 1. Insert unbatched rows → claimAndEmitNewBatches stamps a UUID (Stripe call for our customer).
// 2. Verify the row is marked flushed (no double-emit on second tick for our customer).
// 3. Confirm the idempotency key had the "crucible-batch-" prefix.
func TestTwoPhase_noDoubleCount(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	custID, apiKeyID := setupTestCustomer(t, pool)
	stripeID := "cus_2ph_" + custID.String()[:8]
	if _, err := pool.Exec(ctx,
		`UPDATE customers SET stripe_customer_id=$1 WHERE id=$2`, stripeID, custID,
	); err != nil {
		t.Fatalf("set stripe_customer_id: %v", err)
	}

	reqID := "req-twophase-" + custID.String()[:8]
	if _, err := pool.Exec(ctx,
		`INSERT INTO usage_events (customer_id, api_key_id, operation, billable_units, request_id)
		 VALUES ($1, $2, 'twophase.op', 50, $3)`,
		custID, apiKeyID, reqID,
	); err != nil {
		t.Fatalf("insert row: %v", err)
	}

	// Tick 1 — phase B: Stripe succeeds, row is claimed and flushed.
	mock1 := &mockStripeMeter{}
	f1 := NewFlusher(pool, mock1, 0)
	if err := f1.claimAndEmitNewBatches(ctx); err != nil {
		t.Fatalf("tick1 claimAndEmitNewBatches: %v", err)
	}

	// Our customer must have had exactly one Stripe call.
	calls1 := callsForCustomer(mock1.calls, stripeID)
	if len(calls1) != 1 {
		t.Fatalf("tick1: expected 1 Stripe call for %s, got %d", stripeID, len(calls1))
	}
	firstKey := calls1[0].idempotencyKey

	// After tick 1 the row must be flushed_to_stripe=TRUE.
	var flushed bool
	if err := pool.QueryRow(ctx,
		`SELECT flushed_to_stripe FROM usage_events WHERE request_id=$1 LIMIT 1`, reqID,
	).Scan(&flushed); err != nil {
		t.Fatalf("query flushed: %v", err)
	}
	if !flushed {
		t.Error("row must be marked flushed after a successful tick1")
	}

	// Tick 2 — phase A: row is already flushed; no retry call must target our customer.
	mock2 := &mockStripeMeter{}
	f2 := NewFlusher(pool, mock2, 0)
	if err := f2.retryPendingBatches(ctx); err != nil {
		t.Fatalf("tick2 retryPendingBatches: %v", err)
	}
	if got := callsForCustomer(mock2.calls, stripeID); len(got) != 0 {
		t.Errorf("tick2 phase-A: unexpected Stripe call(s) for %s (already flushed)", stripeID)
	}

	// Tick 2 — phase B: no unbatched rows remain for our customer.
	mock3 := &mockStripeMeter{}
	f3 := NewFlusher(pool, mock3, 0)
	if err := f3.claimAndEmitNewBatches(ctx); err != nil {
		t.Fatalf("tick2 claimAndEmitNewBatches: %v", err)
	}
	if got := callsForCustomer(mock3.calls, stripeID); len(got) != 0 {
		t.Errorf("tick2 phase-B: unexpected Stripe call(s) for %s (already flushed)", stripeID)
	}

	// Sanity: the idempotency key from tick 1 had the correct prefix.
	if !strings.HasPrefix(firstKey, "crucible-batch-") {
		t.Errorf("idempotency key %q missing expected prefix", firstKey)
	}
}

// TestRun_cancelStops verifies that Run() returns promptly when the context is canceled.
func TestRun_cancelStops(t *testing.T) {
	mock := &mockStripeMeter{}
	// Use a very long period so the tick never fires during the test.
	f := NewFlusher(nil, mock, 24*time.Hour)

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		f.Run(ctx)
		close(done)
	}()

	cancel()

	select {
	case <-done:
		// good
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after context cancel within 2s")
	}
}

// TestRun_tickCallsPhases verifies that Run delegates to both flusher phases on each tick
// and exits cleanly when the context is canceled.
//
// Behavioral assertions: we plant data that requires both phases to process — a
// pre-claimed pending row (phase A) and an unbatched row (phase B) — then confirm
// both are marked flushed_to_stripe=TRUE after Run exits.
func TestRun_tickCallsPhases(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	custID, apiKeyID := setupTestCustomer(t, pool)
	stripeID := "cus_run_" + custID.String()
	if _, err := pool.Exec(ctx,
		`UPDATE customers SET stripe_customer_id=$1 WHERE id=$2`, stripeID, custID,
	); err != nil {
		t.Fatalf("set stripe_customer_id: %v", err)
	}

	// Phase-A seed: a row with batch_id already stamped but not yet flushed.
	pendingBatchID := uuid.New()
	reqA := "req-run-phaseA-" + custID.String()
	if _, err := pool.Exec(ctx,
		`INSERT INTO usage_events (customer_id, api_key_id, operation, billable_units, request_id, batch_id, flushed_to_stripe)
		 VALUES ($1, $2, 'run.phaseA', 7, $3, $4, FALSE)`,
		custID, apiKeyID, reqA, pendingBatchID,
	); err != nil {
		t.Fatalf("insert phase-A row: %v", err)
	}

	// Phase-B seed: an unbatched row (batch_id IS NULL).
	reqB := "req-run-phaseB-" + custID.String()
	if _, err := pool.Exec(ctx,
		`INSERT INTO usage_events (customer_id, api_key_id, operation, billable_units, request_id)
		 VALUES ($1, $2, 'run.phaseB', 3, $3)`,
		custID, apiKeyID, reqB,
	); err != nil {
		t.Fatalf("insert phase-B row: %v", err)
	}

	// Run with a 1 ms ticker; cancel after it exits.
	runCtx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	defer cancel()

	f := NewFlusher(pool, &mockStripeMeter{}, 1*time.Millisecond)

	done := make(chan struct{})
	go func() {
		f.Run(runCtx)
		close(done)
	}()

	select {
	case <-done:
		// good — Run returned cleanly
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Run did not return after context deadline within 500ms")
	}

	// Both rows must now be flushed_to_stripe=TRUE — proving both phases fired.
	var flushedA, flushedB bool
	if err := pool.QueryRow(ctx,
		`SELECT flushed_to_stripe FROM usage_events WHERE request_id=$1 LIMIT 1`, reqA,
	).Scan(&flushedA); err != nil {
		t.Fatalf("query phase-A row: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`SELECT flushed_to_stripe FROM usage_events WHERE request_id=$1 LIMIT 1`, reqB,
	).Scan(&flushedB); err != nil {
		t.Fatalf("query phase-B row: %v", err)
	}
	if !flushedA {
		t.Error("phase A (retryPendingBatches) did not flush the pre-claimed row")
	}
	if !flushedB {
		t.Error("phase B (claimAndEmitNewBatches) did not flush the unbatched row")
	}
}

// errStripe is a sentinel error used across tests to simulate Stripe failures.
var errStripe = &stripeTestError{msg: "stripe: network timeout"}

type stripeTestError struct{ msg string }

func (e *stripeTestError) Error() string { return e.msg }
