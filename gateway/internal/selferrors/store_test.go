package selferrors_test

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Unluckyathecking/crucible/gateway/internal/selferrors"
)

// newTestPostgres returns a pool for the local test database, or skips the
// test if Postgres is not reachable. Mirrors selfusage.newTestPostgres.
func newTestPostgres(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	explicit := dsn != ""
	if !explicit {
		if v := os.Getenv("POSTGRES_DSN"); v != "" {
			dsn = v
			explicit = true
		} else {
			dsn = "postgres://crucible@localhost:5432/crucible?sslmode=disable"
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		if explicit {
			t.Fatalf("TEST_DATABASE_URL set but postgres unavailable: %v", err)
		}
		t.Skipf("postgres unavailable, skipping: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		if explicit {
			t.Fatalf("TEST_DATABASE_URL set but postgres ping failed: %v", err)
		}
		t.Skipf("postgres ping failed, skipping: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// seedCustomer inserts a minimal customers row and returns its id.
func seedCustomer(t *testing.T, pool *pgxpool.Pool) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	var id uuid.UUID
	err := pool.QueryRow(ctx,
		`INSERT INTO customers (email, plan_id) VALUES ($1, $2) RETURNING id`,
		fmt.Sprintf("selferrors-test-%s@example.com", uuid.New().String()[:8]), "free",
	).Scan(&id)
	if err != nil {
		t.Fatalf("seedCustomer: %v", err)
	}
	t.Cleanup(func() {
		ctx := context.Background()
		_, _ = pool.Exec(ctx, `DELETE FROM error_events WHERE customer_id = $1`, id)
		_, _ = pool.Exec(ctx, `DELETE FROM api_keys WHERE customer_id = $1`, id)
		_, _ = pool.Exec(ctx, `DELETE FROM customers WHERE id = $1`, id)
	})
	return id
}

// seedErrorEvent inserts one error_events row for a customer at createdAt.
func seedErrorEvent(t *testing.T, pool *pgxpool.Pool, customerID uuid.UUID, operation, code string, status int, createdAt time.Time, payload []byte) {
	t.Helper()
	ctx := context.Background()
	_, err := pool.Exec(ctx, `
		INSERT INTO error_events
		  (customer_id, operation, error_code, http_status, message, request_id, created_at, request_payload)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
	`, customerID, operation, code, status, "boom", "req-"+uuid.New().String(), createdAt, payload)
	if err != nil {
		t.Fatalf("seedErrorEvent: %v", err)
	}
}

// TestStore_Query_IDOR seeds two distinct customers each with their own error
// events, then asserts a query scoped to customer A never returns customer
// B's rows (or vice versa).
func TestStore_Query_IDOR(t *testing.T) {
	pool := newTestPostgres(t)
	store := selferrors.NewStore(pool)

	custA := seedCustomer(t, pool)
	custB := seedCustomer(t, pool)
	now := time.Now().UTC()
	seedErrorEvent(t, pool, custA, "/v1/echo-a", "BAD_REQUEST", 400, now, nil)
	seedErrorEvent(t, pool, custB, "/v1/echo-b", "BAD_REQUEST", 400, now, nil)

	from := now.Add(-time.Hour)
	to := now.Add(time.Hour)

	eventsA, _, err := store.Query(context.Background(), custA, from, to, nil, nil, 50, 0)
	if err != nil {
		t.Fatalf("query customer A: %v", err)
	}
	for _, e := range eventsA {
		if e.Operation == "/v1/echo-b" {
			t.Errorf("customer A query leaked customer B's row: %+v", e)
		}
	}
	foundA := false
	for _, e := range eventsA {
		if e.Operation == "/v1/echo-a" {
			foundA = true
		}
	}
	if !foundA {
		t.Errorf("customer A query missing its own row: %+v", eventsA)
	}

	eventsB, _, err := store.Query(context.Background(), custB, from, to, nil, nil, 50, 0)
	if err != nil {
		t.Fatalf("query customer B: %v", err)
	}
	for _, e := range eventsB {
		if e.Operation == "/v1/echo-a" {
			t.Errorf("customer B query leaked customer A's row: %+v", e)
		}
	}
}

// TestStore_Query_FiltersAndOrdering asserts operation/code filters narrow
// the result set and rows come back newest-first.
func TestStore_Query_FiltersAndOrdering(t *testing.T) {
	pool := newTestPostgres(t)
	store := selferrors.NewStore(pool)
	cust := seedCustomer(t, pool)

	now := time.Now().UTC()
	seedErrorEvent(t, pool, cust, "/v1/echo", "RATE_LIMITED", 429, now.Add(-2*time.Minute), nil)
	seedErrorEvent(t, pool, cust, "/v1/echo", "BAD_REQUEST", 400, now.Add(-1*time.Minute), nil)
	seedErrorEvent(t, pool, cust, "/v1/other", "BAD_REQUEST", 400, now, nil)

	from := now.Add(-time.Hour)
	to := now.Add(time.Hour)

	all, hasMore, err := store.Query(context.Background(), cust, from, to, nil, nil, 50, 0)
	if err != nil {
		t.Fatalf("query all: %v", err)
	}
	if hasMore {
		t.Error("hasMore = true, want false for 3 rows with limit 50")
	}
	if len(all) != 3 {
		t.Fatalf("len(all) = %d, want 3", len(all))
	}
	// Newest-first.
	if all[0].Operation != "/v1/other" || all[2].Operation != "/v1/echo" {
		t.Errorf("unexpected ordering: %+v", all)
	}

	op := "/v1/echo"
	filtered, _, err := store.Query(context.Background(), cust, from, to, &op, nil, 50, 0)
	if err != nil {
		t.Fatalf("query by operation: %v", err)
	}
	if len(filtered) != 2 {
		t.Fatalf("len(filtered by operation) = %d, want 2: %+v", len(filtered), filtered)
	}

	code := "RATE_LIMITED"
	filteredCode, _, err := store.Query(context.Background(), cust, from, to, nil, &code, 50, 0)
	if err != nil {
		t.Fatalf("query by code: %v", err)
	}
	if len(filteredCode) != 1 || filteredCode[0].ErrorCode != "RATE_LIMITED" {
		t.Fatalf("query by code returned %+v, want single RATE_LIMITED row", filteredCode)
	}
}

// TestStore_Query_HasMoreAndPayload asserts the limit+1 probe correctly
// reports has_more and that a non-nil BYTEA payload comes back as a bounded
// UTF-8 string while a nil payload stays nil.
func TestStore_Query_HasMoreAndPayload(t *testing.T) {
	pool := newTestPostgres(t)
	store := selferrors.NewStore(pool)
	cust := seedCustomer(t, pool)

	now := time.Now().UTC()
	seedErrorEvent(t, pool, cust, "/v1/echo", "BAD_REQUEST", 400, now.Add(-2*time.Minute), []byte(`{"a":1}`))
	seedErrorEvent(t, pool, cust, "/v1/echo", "BAD_REQUEST", 400, now.Add(-1*time.Minute), nil)

	from := now.Add(-time.Hour)
	to := now.Add(time.Hour)

	page, hasMore, err := store.Query(context.Background(), cust, from, to, nil, nil, 1, 0)
	if err != nil {
		t.Fatalf("query page 1: %v", err)
	}
	if !hasMore {
		t.Error("hasMore = false, want true (2 rows, limit 1)")
	}
	if len(page) != 1 {
		t.Fatalf("len(page) = %d, want 1", len(page))
	}
	// Newest row (nil payload) comes first.
	if page[0].RequestPayload != nil {
		t.Errorf("RequestPayload = %v, want nil for the row seeded with nil payload", *page[0].RequestPayload)
	}

	page2, hasMore2, err := store.Query(context.Background(), cust, from, to, nil, nil, 1, 1)
	if err != nil {
		t.Fatalf("query page 2: %v", err)
	}
	if hasMore2 {
		t.Error("hasMore = true on last page, want false")
	}
	if len(page2) != 1 {
		t.Fatalf("len(page2) = %d, want 1", len(page2))
	}
	if page2[0].RequestPayload == nil || *page2[0].RequestPayload != `{"a":1}` {
		t.Errorf("RequestPayload = %v, want {\"a\":1}", page2[0].RequestPayload)
	}
}
