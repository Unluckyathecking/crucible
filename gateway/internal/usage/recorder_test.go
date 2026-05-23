package usage

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

func newTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	pool, err := pgxpool.New(ctx, "postgres://crucible@localhost:5432/crucible?sslmode=disable")
	if err != nil {
		t.Skipf("postgres unavailable: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Skipf("postgres ping failed: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func setupTestCustomer(t *testing.T, pool *pgxpool.Pool) (customerID, apiKeyID uuid.UUID) {
	t.Helper()
	ctx := context.Background()

	customerID = uuid.New()
	apiKeyID = uuid.New()
	email := customerID.String() + "@test.local"
	prefix := fmt.Sprintf("tst_%s", customerID.String()[:8])

	if _, err := pool.Exec(ctx,
		`INSERT INTO customers (id, email, plan_id) VALUES ($1, $2, 'free') ON CONFLICT (id) DO NOTHING`,
		customerID, email,
	); err != nil {
		t.Fatalf("insert customer: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO api_keys (id, customer_id, prefix, hash, name) VALUES ($1, $2, $3, E'\\\\xdeadbeef', 'test-key') ON CONFLICT (id) DO NOTHING`,
		apiKeyID, customerID, prefix,
	); err != nil {
		t.Fatalf("insert api_key: %v", err)
	}
	return customerID, apiKeyID
}

func TestNewRecorder_nilDB(t *testing.T) {
	r := NewRecorder(nil, nil)
	if r == nil {
		t.Fatal("NewRecorder(nil, nil) returned nil")
	}
	if r.db != nil {
		t.Error("expected nil db")
	}
	if r.quota != nil {
		t.Error("expected nil quota")
	}
}

func TestNewRecorder_withDB(t *testing.T) {
	pool := newTestPool(t)
	r := NewRecorder(pool, nil)
	if r == nil {
		t.Fatal("NewRecorder(pool, nil) returned nil")
	}
	if r.db != pool {
		t.Error("db not stored")
	}
	if r.quota != nil {
		t.Error("expected nil quota")
	}
}

func TestRecord_tableDriven(t *testing.T) {
	pool := newTestPool(t)
	custID, apiKeyID := setupTestCustomer(t, pool)
	op := "test.operation"

	tests := []struct {
		name   string
		reqID  string
		units  uint64
		wantOk bool
	}{
		{"single unit", "req-1", 1, true},
		{"many units", "req-many", 1024, true},
		{"max int64 units", "req-max", 9223372036854775807, true},
		{"zero units rejected", "req-zero", 0, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := NewRecorder(pool, nil)
			err := r.Record(context.Background(), custID, apiKeyID, op, tt.reqID, tt.units)
			if tt.wantOk && err != nil {
				t.Fatalf("Record(units=%d): unexpected error: %v", tt.units, err)
			}
			if !tt.wantOk && err == nil {
				t.Fatalf("Record(units=%d): expected error, got nil", tt.units)
			}
			if err != nil {
				return
			}

			var gotUnits uint64
			var gotCustID uuid.UUID
			var gotOp, gotReqID string
			err = pool.QueryRow(context.Background(),
				`SELECT customer_id, operation, billable_units, request_id
				 FROM usage_events WHERE customer_id=$1 AND request_id=$2
				 ORDER BY created_at DESC LIMIT 1`,
				custID, tt.reqID,
			).Scan(&gotCustID, &gotOp, &gotUnits, &gotReqID)
			if err != nil {
				t.Fatalf("query inserted row: %v", err)
			}
			if gotCustID != custID {
				t.Errorf("customer_id = %v, want %v", gotCustID, custID)
			}
			if gotOp != op {
				t.Errorf("operation = %q, want %q", gotOp, op)
			}
			if gotUnits != tt.units {
				t.Errorf("billable_units = %d, want %d", gotUnits, tt.units)
			}
			if gotReqID != tt.reqID {
				t.Errorf("request_id = %q, want %q", gotReqID, tt.reqID)
			}
		})
	}
}

func TestRecord_multipleCalls(t *testing.T) {
	pool := newTestPool(t)
	custID, apiKeyID := setupTestCustomer(t, pool)

	r := NewRecorder(pool, nil)
	for i := range 5 {
		reqID := "req-multi-" + string(rune('a'+i))
		if err := r.Record(context.Background(), custID, apiKeyID, "multi.op", reqID, uint64((i+1)*100)); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}

	var count int
	if err := pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM usage_events WHERE customer_id=$1`, custID,
	).Scan(&count); err != nil {
		t.Fatalf("count query: %v", err)
	}
	if count != 5 {
		t.Errorf("expected 5 rows, got %d", count)
	}
}
