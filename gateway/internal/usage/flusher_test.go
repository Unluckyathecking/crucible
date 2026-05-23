package usage

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

type mockStripeMeter struct {
	calls []meterCall
	err   error
}

type meterCall struct {
	stripeCustomerID string
	units            uint64
	idempotencyKey   string
}

func (m *mockStripeMeter) EmitMeterEvent(_ context.Context, stripeCustomerID string, units uint64, idempotencyKey string) error {
	m.calls = append(m.calls, meterCall{
		stripeCustomerID: stripeCustomerID,
		units:            units,
		idempotencyKey:   idempotencyKey,
	})
	return m.err
}

func TestStripeMeter_implementsInterface(t *testing.T) {
	var _ StripeMeter = (*mockStripeMeter)(nil)
}

func TestNewFlusher(t *testing.T) {
	mock := &mockStripeMeter{}
	period := 10 * time.Second

	f := NewFlusher(nil, mock, period)
	if f == nil {
		t.Fatal("NewFlusher returned nil")
	}
	if f.stripe != mock {
		t.Error("stripe not stored")
	}
	if f.period != period {
		t.Errorf("period = %v, want %v", f.period, period)
	}
}

func TestEmitAndMark_idempotencyKeyFormat(t *testing.T) {
	tests := []struct {
		name    string
		batchID uuid.UUID
		custID  string
		units   uint64
	}{
		{"standard batch", uuid.New(), "cus_test123", 42},
		{"zero units", uuid.New(), "cus_zero", 0},
		{"large units", uuid.New(), "cus_large", 1 << 40},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mock := &mockStripeMeter{err: errors.New("stripe err")}
			f := NewFlusher(nil, mock, 0)
			f.emitAndMark(context.Background(), tt.batchID, tt.custID, tt.units)

			if len(mock.calls) != 1 {
				t.Fatalf("expected 1 call, got %d", len(mock.calls))
			}
			call := mock.calls[0]

			wantKey := "crucible-batch-" + tt.batchID.String()
			if call.idempotencyKey != wantKey {
				t.Errorf("idempotencyKey = %q, want %q", call.idempotencyKey, wantKey)
			}
			if call.stripeCustomerID != tt.custID {
				t.Errorf("stripeCustomerID = %q, want %q", call.stripeCustomerID, tt.custID)
			}
			if call.units != tt.units {
				t.Errorf("units = %d, want %d", call.units, tt.units)
			}
		})
	}
}

func TestEmitAndMark_sameBatchSameIdempotencyKey(t *testing.T) {
	batchID := uuid.New()
	wantKey := "crucible-batch-" + batchID.String()

	mock1 := &mockStripeMeter{err: errors.New("err")}
	f1 := NewFlusher(nil, mock1, 0)
	f1.emitAndMark(context.Background(), batchID, "cus_a", 100)

	mock2 := &mockStripeMeter{err: errors.New("err")}
	f2 := NewFlusher(nil, mock2, 0)
	f2.emitAndMark(context.Background(), batchID, "cus_b", 200)

	if mock1.calls[0].idempotencyKey != wantKey {
		t.Errorf("first key = %q, want %q", mock1.calls[0].idempotencyKey, wantKey)
	}
	if mock2.calls[0].idempotencyKey != wantKey {
		t.Errorf("second key = %q, want %q", mock2.calls[0].idempotencyKey, wantKey)
	}
	if mock1.calls[0].idempotencyKey != mock2.calls[0].idempotencyKey {
		t.Error("same batch_id must produce identical idempotency keys")
	}
}

func TestEmitAndMark_differentBatchesDifferentKeys(t *testing.T) {
	mock := &mockStripeMeter{err: errors.New("err")}
	f := NewFlusher(nil, mock, 0)

	batch1 := uuid.New()
	batch2 := uuid.New()

	f.emitAndMark(context.Background(), batch1, "cus_1", 10)
	f.emitAndMark(context.Background(), batch2, "cus_2", 20)

	if len(mock.calls) != 2 {
		t.Fatalf("expected 2 calls, got %d", len(mock.calls))
	}
	if mock.calls[0].idempotencyKey == mock.calls[1].idempotencyKey {
		t.Error("different batch_ids must produce different idempotency keys")
	}
}

func TestEmitAndMark_stripeErrorDoesNotPanic(t *testing.T) {
	mock := &mockStripeMeter{err: errors.New("stripe network error")}
	f := NewFlusher(nil, mock, 0)
	f.emitAndMark(context.Background(), uuid.New(), "cus_err", 1)

	if len(mock.calls) != 1 {
		t.Error("stripe error should still record the call")
	}
}

func TestEmitAndMark_successMarksFlushed(t *testing.T) {
	pool := newTestPool(t)
	custID, apiKeyID := setupTestCustomer(t, pool)

	batchID := uuid.New()

	rec := NewRecorder(pool, nil)
	if err := rec.Record(context.Background(), custID, apiKeyID, "test.op", "req-em", 15); err != nil {
		t.Fatalf("setup Record: %v", err)
	}
	if _, err := pool.Exec(context.Background(),
		`UPDATE usage_events SET batch_id=$1 WHERE customer_id=$2 AND batch_id IS NULL`,
		batchID, custID,
	); err != nil {
		t.Fatalf("stamp batch_id: %v", err)
	}

	mock := &mockStripeMeter{}
	f := NewFlusher(pool, mock, 0)
	f.emitAndMark(context.Background(), batchID, "cus_stripe_ok", 15)

	if len(mock.calls) != 1 {
		t.Fatalf("expected 1 Stripe call, got %d", len(mock.calls))
	}

	var flushed bool
	if err := pool.QueryRow(context.Background(),
		`SELECT flushed_to_stripe FROM usage_events WHERE batch_id=$1 LIMIT 1`,
		batchID,
	).Scan(&flushed); err != nil {
		t.Fatalf("query flushed: %v", err)
	}
	if !flushed {
		t.Error("expected flushed_to_stripe=true after successful emit")
	}
}

func TestFlusher_retryPendingBatches(t *testing.T) {
	pool := newTestPool(t)
	custID, apiKeyID := setupTestCustomer(t, pool)

	// Update the test customer to have a stripe ID so the query picks it up
	stripeID := "cus_pending_test_" + custID.String()[:8]
	if _, err := pool.Exec(context.Background(),
		`UPDATE customers SET stripe_customer_id = $1 WHERE id = $2`, stripeID, custID); err != nil {
		t.Fatalf("setup customer stripe ID: %v", err)
	}

	batchID := uuid.New()

	rec := NewRecorder(pool, nil)
	if err := rec.Record(context.Background(), custID, apiKeyID, "test.op", "req-pending", 25); err != nil {
		t.Fatalf("setup Record: %v", err)
	}

	// manually set batch_id but leave flushed_to_stripe = false
	if _, err := pool.Exec(context.Background(),
		`UPDATE usage_events SET batch_id=$1 WHERE customer_id=$2 AND batch_id IS NULL`,
		batchID, custID,
	); err != nil {
		t.Fatalf("stamp batch_id: %v", err)
	}

	mock := &mockStripeMeter{}
	f := NewFlusher(pool, mock, 0)

	if err := f.retryPendingBatches(context.Background()); err != nil {
		t.Fatalf("retryPendingBatches failed: %v", err)
	}

	if len(mock.calls) != 1 {
		t.Fatalf("expected 1 Stripe call, got %d", len(mock.calls))
	}

	if mock.calls[0].units != 25 {
		t.Errorf("expected 25 units, got %d", mock.calls[0].units)
	}

	if mock.calls[0].idempotencyKey != "crucible-batch-"+batchID.String() {
		t.Errorf("expected idempotency key crucible-batch-%s, got %s", batchID.String(), mock.calls[0].idempotencyKey)
	}

	var flushed bool
	if err := pool.QueryRow(context.Background(),
		`SELECT flushed_to_stripe FROM usage_events WHERE batch_id=$1 LIMIT 1`,
		batchID,
	).Scan(&flushed); err != nil {
		t.Fatalf("query flushed: %v", err)
	}
	if !flushed {
		t.Error("expected flushed_to_stripe=true after successful emit")
	}
}

func TestFlusher_claimAndEmitNewBatches(t *testing.T) {
	pool := newTestPool(t)
	custID, apiKeyID := setupTestCustomer(t, pool)

	// Update the test customer to have a stripe ID
	stripeID := "cus_claim_test_" + custID.String()[:8]
	if _, err := pool.Exec(context.Background(),
		`UPDATE customers SET stripe_customer_id = $1 WHERE id = $2`, stripeID, custID); err != nil {
		t.Fatalf("setup customer stripe ID: %v", err)
	}

	rec := NewRecorder(pool, nil)
	// Create two records
	if err := rec.Record(context.Background(), custID, apiKeyID, "test.op", "req-new1", 10); err != nil {
		t.Fatalf("setup Record: %v", err)
	}
	if err := rec.Record(context.Background(), custID, apiKeyID, "test.op", "req-new2", 20); err != nil {
		t.Fatalf("setup Record: %v", err)
	}

	mock := &mockStripeMeter{}
	f := NewFlusher(pool, mock, 0)

	if err := f.claimAndEmitNewBatches(context.Background()); err != nil {
		t.Fatalf("claimAndEmitNewBatches failed: %v", err)
	}

	if len(mock.calls) != 1 {
		t.Fatalf("expected 1 Stripe call, got %d", len(mock.calls))
	}

	if mock.calls[0].units != 30 {
		t.Errorf("expected 30 units, got %d", mock.calls[0].units)
	}

	// Verify both rows were stamped with same batch id and flushed
	var count int
	var flushed bool
	if err := pool.QueryRow(context.Background(),
		`SELECT COUNT(*), bool_and(flushed_to_stripe) FROM usage_events WHERE customer_id=$1 AND batch_id IS NOT NULL`,
		custID,
	).Scan(&count, &flushed); err != nil {
		t.Fatalf("query events: %v", err)
	}

	if count != 2 {
		t.Errorf("expected 2 rows updated, got %d", count)
	}
	if !flushed {
		t.Errorf("expected rows to be flushed_to_stripe=true")
	}
}

func TestFlusher_Run_ContextCancel(t *testing.T) {
	mock := &mockStripeMeter{}
	f := NewFlusher(nil, mock, 10*time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	// Should return immediately and not panic
	done := make(chan struct{})
	go func() {
		f.Run(ctx)
		close(done)
	}()

	select {
	case <-done:
		// passed
	case <-time.After(1 * time.Second):
		t.Fatal("Run() did not respect context cancellation")
	}
}
