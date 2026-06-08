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
	t.Cleanup(func() { deleteUsageRows(t, pool, custID) })

	const stripeID = "cus_em_ok"
	if _, err := pool.Exec(context.Background(),
		`UPDATE customers SET stripe_customer_id=$1 WHERE id=$2`,
		stripeID, custID,
	); err != nil {
		t.Fatalf("set stripe_customer_id: %v", err)
	}

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
	f.emitAndMark(context.Background(), batchID, stripeID, 15)

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
