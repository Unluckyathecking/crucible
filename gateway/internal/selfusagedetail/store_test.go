package selfusagedetail_test

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Unluckyathecking/crucible/gateway/internal/selfusagedetail"
)

// newTestPostgres returns a pool for the local test database, or skips the
// test if Postgres is not reachable. Mirrors selferrors.newTestPostgres.
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
		fmt.Sprintf("selfusagedetail-test-%s@example.com", uuid.New().String()[:8]), "free",
	).Scan(&id)
	if err != nil {
		t.Fatalf("seedCustomer: %v", err)
	}
	t.Cleanup(func() {
		ctx := context.Background()
		_, _ = pool.Exec(ctx, `DELETE FROM usage_events WHERE customer_id = $1`, id)
		_, _ = pool.Exec(ctx, `DELETE FROM api_keys WHERE customer_id = $1`, id)
		_, _ = pool.Exec(ctx, `DELETE FROM customers WHERE id = $1`, id)
	})
	return id
}

// seedAPIKey inserts a minimal api_keys row for customerID and returns its id.
// usage_events.api_key_id is NOT NULL, so every seeded usage row needs one.
func seedAPIKey(t *testing.T, pool *pgxpool.Pool, customerID uuid.UUID) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	var id uuid.UUID
	err := pool.QueryRow(ctx, `
		INSERT INTO api_keys (customer_id, prefix, hash)
		VALUES ($1, $2, $3)
		RETURNING id
	`, customerID, "selfusagedetail-test-"+uuid.New().String()[:8], []byte("hash")).Scan(&id)
	if err != nil {
		t.Fatalf("seedAPIKey: %v", err)
	}
	return id
}

// seedUsageEvent inserts one usage_events row for a customer at createdAt.
func seedUsageEvent(t *testing.T, pool *pgxpool.Pool, customerID, apiKeyID uuid.UUID, operation string, billableUnits int64, createdAt time.Time) {
	t.Helper()
	ctx := context.Background()
	_, err := pool.Exec(ctx, `
		INSERT INTO usage_events (customer_id, api_key_id, operation, billable_units, request_id, created_at)
		VALUES ($1, $2, $3, $4, $5, $6)
	`, customerID, apiKeyID, operation, billableUnits, "req-"+uuid.New().String(), createdAt)
	if err != nil {
		t.Fatalf("seedUsageEvent: %v", err)
	}
}

// TestStore_Query_IDOR seeds two distinct customers each with their own usage
// events, then asserts a query scoped to customer A never returns customer
// B's rows (or vice versa).
func TestStore_Query_IDOR(t *testing.T) {
	pool := newTestPostgres(t)
	store := selfusagedetail.NewStore(pool)

	custA := seedCustomer(t, pool)
	custB := seedCustomer(t, pool)
	keyA := seedAPIKey(t, pool, custA)
	keyB := seedAPIKey(t, pool, custB)
	now := time.Now().UTC()
	seedUsageEvent(t, pool, custA, keyA, "/v1/echo-a", 1, now)
	seedUsageEvent(t, pool, custB, keyB, "/v1/echo-b", 1, now)

	from := now.Add(-time.Hour)
	to := now.Add(time.Hour)

	eventsA, _, err := store.Query(context.Background(), custA, from, to, nil, 50, 0)
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

	eventsB, _, err := store.Query(context.Background(), custB, from, to, nil, 50, 0)
	if err != nil {
		t.Fatalf("query customer B: %v", err)
	}
	for _, e := range eventsB {
		if e.Operation == "/v1/echo-a" {
			t.Errorf("customer B query leaked customer A's row: %+v", e)
		}
	}
}

// TestStore_Query_FiltersAndOrdering asserts an operation filter narrows the
// result set and rows come back newest-first.
func TestStore_Query_FiltersAndOrdering(t *testing.T) {
	pool := newTestPostgres(t)
	store := selfusagedetail.NewStore(pool)
	cust := seedCustomer(t, pool)
	key := seedAPIKey(t, pool, cust)

	now := time.Now().UTC()
	seedUsageEvent(t, pool, cust, key, "/v1/echo", 2, now.Add(-2*time.Minute))
	seedUsageEvent(t, pool, cust, key, "/v1/echo", 3, now.Add(-1*time.Minute))
	seedUsageEvent(t, pool, cust, key, "/v1/other", 1, now)

	from := now.Add(-time.Hour)
	to := now.Add(time.Hour)

	all, hasMore, err := store.Query(context.Background(), cust, from, to, nil, 50, 0)
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
	filtered, _, err := store.Query(context.Background(), cust, from, to, &op, 50, 0)
	if err != nil {
		t.Fatalf("query by operation: %v", err)
	}
	if len(filtered) != 2 {
		t.Fatalf("len(filtered by operation) = %d, want 2: %+v", len(filtered), filtered)
	}
}

// TestStore_Query_StableOrderingOnTimestampTie asserts that rows sharing the
// exact same created_at value still come back in a single, stable total
// order — the id DESC tiebreaker — rather than an order Postgres is free to
// vary across otherwise-identical paged queries. Without it, a customer
// paging through more than one page of same-timestamp rows could see
// duplicate or skipped rows across offset pages.
func TestStore_Query_StableOrderingOnTimestampTie(t *testing.T) {
	pool := newTestPostgres(t)
	store := selfusagedetail.NewStore(pool)
	cust := seedCustomer(t, pool)
	key := seedAPIKey(t, pool, cust)

	tied := time.Now().UTC()
	seedUsageEvent(t, pool, cust, key, "op-1", 1, tied)
	seedUsageEvent(t, pool, cust, key, "op-2", 2, tied)
	seedUsageEvent(t, pool, cust, key, "op-3", 3, tied)

	from := tied.Add(-time.Hour)
	to := tied.Add(time.Hour)

	page1, hasMore, err := store.Query(context.Background(), cust, from, to, nil, 2, 0)
	if err != nil {
		t.Fatalf("query page 1: %v", err)
	}
	if !hasMore {
		t.Fatal("hasMore = false, want true (3 rows, limit 2)")
	}
	if len(page1) != 2 {
		t.Fatalf("len(page1) = %d, want 2", len(page1))
	}

	page2, _, err := store.Query(context.Background(), cust, from, to, nil, 2, 2)
	if err != nil {
		t.Fatalf("query page 2: %v", err)
	}
	if len(page2) != 1 {
		t.Fatalf("len(page2) = %d, want 1", len(page2))
	}

	seen := map[string]bool{}
	for _, e := range append(page1, page2...) {
		if seen[e.ID] {
			t.Errorf("row id %s appeared on more than one page: page1=%+v page2=%+v", e.ID, page1, page2)
		}
		seen[e.ID] = true
	}
	if len(seen) != 3 {
		t.Errorf("expected 3 distinct rows across both pages, got %d: page1=%+v page2=%+v", len(seen), page1, page2)
	}
}

// TestStore_Query_HasMoreAndBillableUnits asserts the limit+1 probe correctly
// reports has_more and billable_units round-trips accurately.
func TestStore_Query_HasMoreAndBillableUnits(t *testing.T) {
	pool := newTestPostgres(t)
	store := selfusagedetail.NewStore(pool)
	cust := seedCustomer(t, pool)
	key := seedAPIKey(t, pool, cust)

	now := time.Now().UTC()
	seedUsageEvent(t, pool, cust, key, "/v1/echo", 5, now.Add(-2*time.Minute))
	seedUsageEvent(t, pool, cust, key, "/v1/echo", 7, now.Add(-1*time.Minute))

	from := now.Add(-time.Hour)
	to := now.Add(time.Hour)

	page, hasMore, err := store.Query(context.Background(), cust, from, to, nil, 1, 0)
	if err != nil {
		t.Fatalf("query page 1: %v", err)
	}
	if !hasMore {
		t.Error("hasMore = false, want true (2 rows, limit 1)")
	}
	if len(page) != 1 {
		t.Fatalf("len(page) = %d, want 1", len(page))
	}
	if page[0].BillableUnits != "7" {
		t.Errorf("BillableUnits = %q, want \"7\" (newest row first)", page[0].BillableUnits)
	}

	page2, hasMore2, err := store.Query(context.Background(), cust, from, to, nil, 1, 1)
	if err != nil {
		t.Fatalf("query page 2: %v", err)
	}
	if hasMore2 {
		t.Error("hasMore = true on last page, want false")
	}
	if len(page2) != 1 || page2[0].BillableUnits != "5" {
		t.Errorf("page2 = %+v, want single row with BillableUnits=\"5\"", page2)
	}
}
