package usage

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

func insertUsageEvent(t testing.TB, pool *pgxpool.Pool, customerID, apiKeyID uuid.UUID, operation string, units int64) {
	t.Helper()
	ctx := context.Background()
	reqID := "req-" + uuid.New().String()[:8]
	_, err := pool.Exec(ctx,
		`INSERT INTO usage_events (customer_id, api_key_id, operation, billable_units, request_id)
		 VALUES ($1, $2, $3, $4, $5)`,
		customerID, apiKeyID, operation, units, reqID,
	)
	if err != nil {
		t.Fatalf("insert usage_event: %v", err)
	}
}

func TestQueryByOperation_emptyWindow(t *testing.T) {
	pool := newTestPool(t)
	custID, _ := setupTestCustomer(t, pool)

	from := time.Now().Add(-time.Hour)
	to := time.Now()

	result, err := QueryByOperation(context.Background(), pool, custID, from, to, "")
	if err != nil {
		t.Fatalf("QueryByOperation: %v", err)
	}
	if len(result) != 0 {
		t.Errorf("expected empty result for new customer, got %d rows", len(result))
	}
}

func TestQueryByOperation_singleOperationFilter(t *testing.T) {
	pool := newTestPool(t)
	custID, keyID := setupTestCustomer(t, pool)
	ctx := context.Background()

	insertUsageEvent(t, pool, custID, keyID, "op.a", 5)
	insertUsageEvent(t, pool, custID, keyID, "op.a", 3)
	insertUsageEvent(t, pool, custID, keyID, "op.b", 10)

	from := time.Now().Add(-time.Minute)
	to := time.Now().Add(time.Minute)

	result, err := QueryByOperation(ctx, pool, custID, from, to, "op.a")
	if err != nil {
		t.Fatalf("QueryByOperation: %v", err)
	}
	if len(result) != 1 {
		t.Fatalf("expected 1 row, got %d", len(result))
	}
	if result[0].Operation != "op.a" {
		t.Errorf("operation = %q, want %q", result[0].Operation, "op.a")
	}
	if result[0].TotalBillableUnits != 8 {
		t.Errorf("total_billable_units = %d, want 8", result[0].TotalBillableUnits)
	}
	if result[0].EventCount != 2 {
		t.Errorf("event_count = %d, want 2", result[0].EventCount)
	}
}

func TestQueryByOperation_multiOperation(t *testing.T) {
	pool := newTestPool(t)
	custID, keyID := setupTestCustomer(t, pool)
	ctx := context.Background()

	insertUsageEvent(t, pool, custID, keyID, "op.x", 10)
	insertUsageEvent(t, pool, custID, keyID, "op.y", 20)
	insertUsageEvent(t, pool, custID, keyID, "op.y", 30)

	from := time.Now().Add(-time.Minute)
	to := time.Now().Add(time.Minute)

	result, err := QueryByOperation(ctx, pool, custID, from, to, "")
	if err != nil {
		t.Fatalf("QueryByOperation: %v", err)
	}
	if len(result) != 2 {
		t.Fatalf("expected 2 rows, got %d", len(result))
	}
	// Results are ordered alphabetically by operation.
	if result[0].Operation != "op.x" || result[0].TotalBillableUnits != 10 || result[0].EventCount != 1 {
		t.Errorf("row 0: got %+v, want {op.x 10 1}", result[0])
	}
	if result[1].Operation != "op.y" || result[1].TotalBillableUnits != 50 || result[1].EventCount != 2 {
		t.Errorf("row 1: got %+v, want {op.y 50 2}", result[1])
	}
}

func TestQueryByOperation_unknownCustomer(t *testing.T) {
	pool := newTestPool(t)
	from := time.Now().Add(-time.Minute)
	to := time.Now().Add(time.Minute)

	// Use a fixed non-existent UUID rather than zero so the assertion is not
	// coupled to whether the DB happens to contain a zero-UUID customer.
	nonExistent := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	result, err := QueryByOperation(context.Background(), pool, nonExistent, from, to, "")
	if err != nil {
		t.Fatalf("QueryByOperation with non-existent UUID: %v", err)
	}
	if len(result) != 0 {
		t.Errorf("expected empty result for unknown customer, got %d rows", len(result))
	}
}

func TestQueryByOperation_fromEqualTo(t *testing.T) {
	pool := newTestPool(t)
	custID, keyID := setupTestCustomer(t, pool)

	insertUsageEvent(t, pool, custID, keyID, "op.a", 5)

	// from == to is a valid half-open interval [t, t) that returns zero rows; not an error.
	now := time.Now()
	result, err := QueryByOperation(context.Background(), pool, custID, now, now, "")
	if err != nil {
		t.Fatalf("QueryByOperation from==to: %v", err)
	}
	if len(result) != 0 {
		t.Errorf("expected 0 rows for empty interval, got %d", len(result))
	}
}

func TestQueryByOperation_operationTooLong(t *testing.T) {
	pool := newTestPool(t)
	custID, _ := setupTestCustomer(t, pool)

	from := time.Now().Add(-time.Minute)
	to := time.Now().Add(time.Minute)
	longOp := string(make([]byte, 129))

	_, err := QueryByOperation(context.Background(), pool, custID, from, to, longOp)
	if err == nil {
		t.Error("expected error for operation > 128 chars, got nil")
	}
}

func TestQueryByOperation_fromAfterTo(t *testing.T) {
	pool := newTestPool(t)
	custID, _ := setupTestCustomer(t, pool)

	now := time.Now()
	_, err := QueryByOperation(context.Background(), pool, custID, now.Add(time.Minute), now, "")
	if err == nil {
		t.Error("expected error when from > to, got nil")
	}
}

func TestQueryByOperation_halfOpenBoundary(t *testing.T) {
	pool := newTestPool(t)
	custID, keyID := setupTestCustomer(t, pool)
	ctx := context.Background()

	insertUsageEvent(t, pool, custID, keyID, "op.a", 7)

	// Use a 'to' in the past so the event (inserted just now) falls at or after 'to'.
	past := time.Now().Add(-time.Minute)
	from := past.Add(-time.Hour)

	result, err := QueryByOperation(ctx, pool, custID, from, past, "")
	if err != nil {
		t.Fatalf("QueryByOperation: %v", err)
	}
	if len(result) != 0 {
		t.Errorf("expected event at >= to to be excluded, got %d rows", len(result))
	}
}

// TestQueryByOperation_crossCustomerIsolation asserts the query never leaks another
// customer's rows — customerID is always the scope boundary.
func TestQueryByOperation_crossCustomerIsolation(t *testing.T) {
	pool := newTestPool(t)
	custA, keyA := setupTestCustomer(t, pool)
	custB, keyB := setupTestCustomer(t, pool)
	ctx := context.Background()

	insertUsageEvent(t, pool, custA, keyA, "op.shared", 100)
	insertUsageEvent(t, pool, custB, keyB, "op.shared", 999)

	from := time.Now().Add(-time.Minute)
	to := time.Now().Add(time.Minute)

	resultA, err := QueryByOperation(ctx, pool, custA, from, to, "")
	if err != nil {
		t.Fatalf("QueryByOperation custA: %v", err)
	}
	if len(resultA) != 1 {
		t.Fatalf("custA: expected 1 row, got %d", len(resultA))
	}
	if resultA[0].TotalBillableUnits != 100 {
		t.Errorf("custA total = %d, want 100 (must not include custB's 999)", resultA[0].TotalBillableUnits)
	}

	resultB, err := QueryByOperation(ctx, pool, custB, from, to, "")
	if err != nil {
		t.Fatalf("QueryByOperation custB: %v", err)
	}
	if len(resultB) != 1 {
		t.Fatalf("custB: expected 1 row, got %d", len(resultB))
	}
	if resultB[0].TotalBillableUnits != 999 {
		t.Errorf("custB total = %d, want 999 (must not include custA's 100)", resultB[0].TotalBillableUnits)
	}
}
