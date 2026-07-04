package usage

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/Unluckyathecking/crucible/gateway/internal/quota"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
	"os"
)

// testDSN returns the Postgres DSN used by all usage package tests.
// Override with PG_TEST_DSN to point at a non-default instance.
// WARNING: must be a dedicated test database; tests create and delete rows.
func testDSN() string {
	if dsn := os.Getenv("PG_TEST_DSN"); dsn != "" {
		return dsn
	}
	return "postgres://crucible@localhost:5432/crucible?sslmode=disable"
}

func newTestPool(t testing.TB) *pgxpool.Pool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	pool, err := pgxpool.New(ctx, testDSN())
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

func setupTestCustomer(t testing.TB, pool *pgxpool.Pool) (customerID, apiKeyID uuid.UUID) {
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

// deleteUsageRows removes usage_events, api_keys, and customer rows for the given customer.
// Called by t.Cleanup so test rows don't accumulate across runs and pollute aggregate assertions.
// Deletion order respects FK constraints: usage_events → api_keys → customers.
// Uses t.Logf (not t.Errorf) so cleanup failures are visible without failing an already-passing test.
func deleteUsageRows(t testing.TB, pool *pgxpool.Pool, custID uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `DELETE FROM usage_events WHERE customer_id=$1`, custID); err != nil {
		t.Logf("cleanup: delete usage_events for %v: %v", custID, err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM api_keys WHERE customer_id=$1`, custID); err != nil {
		t.Logf("cleanup: delete api_keys for %v: %v", custID, err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM customers WHERE id=$1`, custID); err != nil {
		t.Logf("cleanup: delete customers for %v: %v", custID, err)
	}
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
	t.Cleanup(func() { deleteUsageRows(t, pool, custID) })
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
	t.Cleanup(func() { deleteUsageRows(t, pool, custID) })

	r := NewRecorder(pool, nil)
	for i := range 5 {
		reqID := fmt.Sprintf("req-multi-%d", i)
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

func TestRecord_withQuota(t *testing.T) {
	pool := newTestPool(t)
	custID, apiKeyID := setupTestCustomer(t, pool)
	t.Cleanup(func() { deleteUsageRows(t, pool, custID) })

	cache := newTestRedis(t)
	qTracker := quota.New(cache)

	r := NewRecorder(pool, qTracker)

	// Use background context
	ctx := context.Background()

	err := r.Record(ctx, custID, apiKeyID, "test.quota.op", "req-quota-1", 42)
	if err != nil {
		t.Fatalf("Record failed: %v", err)
	}

	// 1. Verify Postgres durable insert
	var units uint64
	err = pool.QueryRow(context.Background(),
		`SELECT billable_units FROM usage_events WHERE request_id = $1`, "req-quota-1",
	).Scan(&units)
	if err != nil {
		t.Fatalf("query inserted row: %v", err)
	}
	if units != 42 {
		t.Errorf("got units %d, want 42", units)
	}

	// 2. Verify Redis quota counter was updated
	// Using Current to fetch the count directly from Redis
	v, err := qTracker.Current(context.Background(), custID)
	if err != nil {
		t.Fatalf("querying redis quota tracker failed: %v", err)
	}
	if v != 42 {
		t.Errorf("expected redis counter to be 42, got %d", v)
	}
}

func TestRecord_quotaRedisError_Tolerated(t *testing.T) {
	pool := newTestPool(t)
	custID, apiKeyID := setupTestCustomer(t, pool)
	t.Cleanup(func() { deleteUsageRows(t, pool, custID) })

	cache := newTestRedis(t)
	qTracker := quota.New(cache)

	// Close the redis connection to force an error on Add()
	cache.Close()

	r := NewRecorder(pool, qTracker)

	// Use background context
	ctx := context.Background()

	// Even though Redis will fail, Record should still succeed because Postgres insert is durable
	err := r.Record(ctx, custID, apiKeyID, "test.quota.err.op", "req-quota-err", 7)
	if err != nil {
		t.Fatalf("Record failed with redis error, should have been tolerated: %v", err)
	}

	// 1. Verify Postgres durable insert still happened
	var units uint64
	err = pool.QueryRow(context.Background(),
		`SELECT billable_units FROM usage_events WHERE request_id = $1`, "req-quota-err",
	).Scan(&units)
	if err != nil {
		t.Fatalf("query inserted row: %v", err)
	}
	if units != 7 {
		t.Errorf("got units %d, want 7", units)
	}
}

func newTestRedis(t testing.TB) *redis.Client {
	t.Helper()
	addr := os.Getenv("REDIS_TEST_ADDR")
	if addr == "" {
		addr = "localhost:6379"
	}
	c := redis.NewClient(&redis.Options{Addr: addr})
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	if err := c.Ping(ctx).Err(); err != nil {
		t.Skipf("redis unavailable on %s, skipping: %v", addr, err)
	}
	return c
}
