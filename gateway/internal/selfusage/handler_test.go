package selfusage_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/Unluckyathecking/crucible/gateway/internal/auth"
	"github.com/Unluckyathecking/crucible/gateway/internal/billing"
	"github.com/Unluckyathecking/crucible/gateway/internal/quota"
	"github.com/Unluckyathecking/crucible/gateway/internal/selfusage"
	"github.com/Unluckyathecking/crucible/gateway/internal/testdb"
)

// newTestPostgres returns a pool for the local test database, or skips the test
// if Postgres is not reachable. Mirrors operator.newTestPostgres.
func newTestPostgres(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	explicit := dsn != ""
	if !explicit {
		if v := os.Getenv("POSTGRES_DSN"); v != "" {
			dsn = v
			explicit = true
		} else {
			dsn = testdb.DSN(t)
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

func newTestRedis(t *testing.T) *redis.Client {
	t.Helper()
	c := redis.NewClient(&redis.Options{Addr: "localhost:6379"})
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	if err := c.Ping(ctx).Err(); err != nil {
		t.Skipf("redis unavailable on localhost:6379, skipping: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// seedCustomer inserts a minimal customers row on the given plan and returns its id.
func seedCustomer(t *testing.T, pool *pgxpool.Pool, planID string) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	var id uuid.UUID
	err := pool.QueryRow(ctx,
		`INSERT INTO customers (email, plan_id) VALUES ($1, $2) RETURNING id`,
		fmt.Sprintf("selfusage-test-%s@example.com", uuid.New().String()[:8]), planID,
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

// seedUsageEvent inserts one usage_events row for a customer, creating a throwaway
// api_keys row to satisfy the FK.
func seedUsageEvent(t *testing.T, pool *pgxpool.Pool, customerID uuid.UUID, operation string, units int64) {
	t.Helper()
	ctx := context.Background()
	var keyID uuid.UUID
	_ = pool.QueryRow(ctx,
		`INSERT INTO api_keys (customer_id, prefix, hash) VALUES ($1, $2, $3) RETURNING id`,
		customerID, "su_test_pfx_"+uuid.New().String()[:8], []byte("testhash"),
	).Scan(&keyID)

	_, err := pool.Exec(ctx,
		`INSERT INTO usage_events (customer_id, api_key_id, operation, billable_units, request_id)
		 VALUES ($1, $2, $3, $4, $5)`,
		customerID, keyID, operation, units, "req-"+uuid.New().String(),
	)
	if err != nil {
		t.Fatalf("seedUsageEvent: %v", err)
	}
}

// cleanupQuotaKey deletes the Redis quota counter for customerID in the current
// UTC month. Mirrors the "quota:<customer>:<YYYY-MM>" key format documented on
// quota.Tracker (the field is unexported, so tests reconstruct it rather than
// leaking package-internal state across module boundaries).
func cleanupQuotaKey(t *testing.T, rdb *redis.Client, customerID uuid.UUID) {
	t.Helper()
	key := fmt.Sprintf("quota:%s:%s", customerID, time.Now().UTC().Format("2006-01"))
	t.Cleanup(func() { rdb.Del(context.Background(), key) })
}

// testKey builds an auth.Key for customerID on planID, wired via auth.WithTestKey.
func testKeyContext(customerID uuid.UUID, planID string) context.Context {
	key := &auth.Key{
		ID: uuid.New(),
		Customer: auth.Customer{
			ID:    customerID,
			Email: "selfusage-test@example.com",
			Plan:  planID,
		},
	}
	return auth.WithTestKey(context.Background(), key)
}

func newRouter(db *pgxpool.Pool, tracker *quota.Tracker, plans *billing.PlanCache) http.Handler {
	r := chi.NewRouter()
	r.Get("/v1/usage", selfusage.Handler(db, tracker, plans))
	return r
}

func decodeResponse(t *testing.T, rec *httptest.ResponseRecorder) selfusage.Response {
	t.Helper()
	var resp selfusage.Response
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v — body: %s", err, rec.Body.String())
	}
	return resp
}

// TestHandler_NoAuth verifies the endpoint 401s when auth.FromContext has no key
// (the auth.Middleware chain always populates this in production; this exercises
// the handler's own defense-in-depth check).
func TestHandler_NoAuth(t *testing.T) {
	r := newRouter(nil, nil, nil)
	req := httptest.NewRequest(http.MethodGet, "/v1/usage", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d — body: %s", rec.Code, rec.Body.String())
	}
}

// TestHandler_IDOR seeds two distinct customers each with their own usage_events,
// then asserts a request authenticated as customer A never sees customer B's
// breakdown (or vice versa) — there is no customer_id parameter, so the only way
// to leak cross-customer data would be a handler bug that ignores auth context.
func TestHandler_IDOR(t *testing.T) {
	pool := newTestPostgres(t)

	custA := seedCustomer(t, pool, "free")
	custB := seedCustomer(t, pool, "free")
	seedUsageEvent(t, pool, custA, "op-a", 5)
	seedUsageEvent(t, pool, custB, "op-b", 7)

	r := newRouter(pool, nil, nil)

	reqA := httptest.NewRequest(http.MethodGet, "/v1/usage", nil).WithContext(testKeyContext(custA, "free"))
	recA := httptest.NewRecorder()
	r.ServeHTTP(recA, reqA)
	if recA.Code != http.StatusOK {
		t.Fatalf("customer A: expected 200, got %d — body: %s", recA.Code, recA.Body.String())
	}
	respA := decodeResponse(t, recA)

	reqB := httptest.NewRequest(http.MethodGet, "/v1/usage", nil).WithContext(testKeyContext(custB, "free"))
	recB := httptest.NewRecorder()
	r.ServeHTTP(recB, reqB)
	if recB.Code != http.StatusOK {
		t.Fatalf("customer B: expected 200, got %d — body: %s", recB.Code, recB.Body.String())
	}
	respB := decodeResponse(t, recB)

	for _, op := range respA.Breakdown {
		if op.Operation == "op-b" {
			t.Errorf("customer A response leaked customer B's operation %q", op.Operation)
		}
	}
	for _, op := range respB.Breakdown {
		if op.Operation == "op-a" {
			t.Errorf("customer B response leaked customer A's operation %q", op.Operation)
		}
	}

	foundA := false
	for _, op := range respA.Breakdown {
		if op.Operation == "op-a" && op.TotalUnits == 5 {
			foundA = true
		}
	}
	if !foundA {
		t.Errorf("customer A response missing its own op-a usage: %+v", respA.Breakdown)
	}

	foundB := false
	for _, op := range respB.Breakdown {
		if op.Operation == "op-b" && op.TotalUnits == 7 {
			foundB = true
		}
	}
	if !foundB {
		t.Errorf("customer B response missing its own op-b usage: %+v", respB.Breakdown)
	}
}

// TestHandler_UsedMatchesTrackerCurrent asserts Used is exactly quota.Tracker.Current's
// value — the same signal the quota middleware gates on — not some independently
// computed figure that could drift from what actually throttles the customer.
func TestHandler_UsedMatchesTrackerCurrent(t *testing.T) {
	rdb := newTestRedis(t)
	cust := uuid.New()
	cleanupQuotaKey(t, rdb, cust)
	tracker := quota.New(rdb)

	for i := 0; i < 3; i++ {
		if _, _, _, _, err := tracker.Reserve(context.Background(), cust, 100); err != nil {
			t.Fatalf("reserve %d: %v", i, err)
		}
	}
	wantUsed, err := tracker.Current(context.Background(), cust)
	if err != nil {
		t.Fatalf("tracker.Current: %v", err)
	}
	if wantUsed == 0 {
		t.Fatal("expected non-zero counter after 3 reserves")
	}

	r := newRouter(nil, tracker, nil)
	req := httptest.NewRequest(http.MethodGet, "/v1/usage", nil).WithContext(testKeyContext(cust, "free"))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d — body: %s", rec.Code, rec.Body.String())
	}
	resp := decodeResponse(t, rec)
	if resp.Used != wantUsed {
		t.Errorf("Used = %d, want %d (tracker.Current parity)", resp.Used, wantUsed)
	}
}

// TestHandler_CapAndRemaining_FromPlanCache seeds a real plans-backed PlanCache and
// asserts Cap/Remaining derive from billing.PlanCache.MonthlyCap for the customer's
// plan, with Used subtracted correctly.
func TestHandler_CapAndRemaining_FromPlanCache(t *testing.T) {
	pool := newTestPostgres(t)
	rdb := newTestRedis(t)
	plans := billing.NewPlanCache(pool)
	tracker := quota.New(rdb)

	wantCap := plans.MonthlyCap(context.Background(), "free")
	if wantCap <= 0 {
		t.Skip("free plan is unlimited in this environment; skipping cap/remaining test")
	}

	cust := uuid.New()
	cleanupQuotaKey(t, rdb, cust)
	if _, _, _, _, err := tracker.Reserve(context.Background(), cust, wantCap); err != nil {
		t.Fatalf("reserve: %v", err)
	}
	used, err := tracker.Current(context.Background(), cust)
	if err != nil {
		t.Fatalf("tracker.Current: %v", err)
	}

	r := newRouter(nil, tracker, plans)
	req := httptest.NewRequest(http.MethodGet, "/v1/usage", nil).WithContext(testKeyContext(cust, "free"))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d — body: %s", rec.Code, rec.Body.String())
	}
	resp := decodeResponse(t, rec)
	if resp.Cap != wantCap {
		t.Errorf("Cap = %d, want %d", resp.Cap, wantCap)
	}
	wantRemaining := wantCap - int64(used)
	if wantRemaining < 0 {
		wantRemaining = 0
	}
	if resp.Remaining != wantRemaining {
		t.Errorf("Remaining = %d, want %d", resp.Remaining, wantRemaining)
	}
	if resp.PlanID != "free" {
		t.Errorf("PlanID = %q, want free", resp.PlanID)
	}
}

// TestHandler_Breakdown seeds multiple operations for one customer and asserts the
// per-operation breakdown totals and descending total_units ordering.
func TestHandler_Breakdown(t *testing.T) {
	pool := newTestPostgres(t)
	cust := seedCustomer(t, pool, "free")

	seedUsageEvent(t, pool, cust, "op-small", 2)
	seedUsageEvent(t, pool, cust, "op-big", 9)
	seedUsageEvent(t, pool, cust, "op-big", 6) // second call, same operation: aggregates to 15

	r := newRouter(pool, nil, nil)
	req := httptest.NewRequest(http.MethodGet, "/v1/usage", nil).WithContext(testKeyContext(cust, "free"))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d — body: %s", rec.Code, rec.Body.String())
	}
	resp := decodeResponse(t, rec)

	if resp.TotalUnits != 17 {
		t.Errorf("TotalUnits = %d, want 17", resp.TotalUnits)
	}
	if resp.TotalCalls != 3 {
		t.Errorf("TotalCalls = %d, want 3", resp.TotalCalls)
	}
	if len(resp.Breakdown) != 2 {
		t.Fatalf("Breakdown has %d entries, want 2: %+v", len(resp.Breakdown), resp.Breakdown)
	}
	// Descending total_units ordering: op-big (15) before op-small (2).
	if resp.Breakdown[0].Operation != "op-big" || resp.Breakdown[0].TotalUnits != 15 || resp.Breakdown[0].Calls != 2 {
		t.Errorf("Breakdown[0] = %+v, want op-big/15/2", resp.Breakdown[0])
	}
	if resp.Breakdown[1].Operation != "op-small" || resp.Breakdown[1].TotalUnits != 2 || resp.Breakdown[1].Calls != 1 {
		t.Errorf("Breakdown[1] = %+v, want op-small/2/1", resp.Breakdown[1])
	}
}

// TestHandler_NilDB_ZeroedBreakdown asserts a nil DB (Deps.DB unset, mirroring
// idempotency.NewStore's nil-safety) returns a 200 with an empty breakdown rather
// than panicking or erroring — used/cap/remaining still populate from the
// (non-nil) tracker/plans.
func TestHandler_NilDB_ZeroedBreakdown(t *testing.T) {
	rdb := newTestRedis(t)
	tracker := quota.New(rdb)
	cust := uuid.New()

	r := newRouter(nil, tracker, nil)
	req := httptest.NewRequest(http.MethodGet, "/v1/usage", nil).WithContext(testKeyContext(cust, "free"))
	rec := httptest.NewRecorder()

	// Must not panic.
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d — body: %s", rec.Code, rec.Body.String())
	}
	resp := decodeResponse(t, rec)
	if len(resp.Breakdown) != 0 {
		t.Errorf("Breakdown = %+v, want empty for nil DB", resp.Breakdown)
	}
	if resp.TotalUnits != 0 || resp.TotalCalls != 0 {
		t.Errorf("TotalUnits/TotalCalls = %d/%d, want 0/0 for nil DB", resp.TotalUnits, resp.TotalCalls)
	}
}

// TestHandler_NilTrackerAndPlans_ZeroedCounters asserts nil quota.Tracker and nil
// billing.PlanCache (both "nil-Redis"-shaped: the tracker wraps a Redis client)
// degrade to zeroed used/cap rather than panicking.
func TestHandler_NilTrackerAndPlans_ZeroedCounters(t *testing.T) {
	pool := newTestPostgres(t)
	cust := seedCustomer(t, pool, "free")

	r := newRouter(pool, nil, nil)
	req := httptest.NewRequest(http.MethodGet, "/v1/usage", nil).WithContext(testKeyContext(cust, "free"))
	rec := httptest.NewRecorder()

	// Must not panic.
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d — body: %s", rec.Code, rec.Body.String())
	}
	resp := decodeResponse(t, rec)
	if resp.Used != 0 {
		t.Errorf("Used = %d, want 0 for nil tracker", resp.Used)
	}
	if resp.Cap != 0 {
		t.Errorf("Cap = %d, want 0 for nil plans", resp.Cap)
	}
	if resp.Remaining != -1 {
		t.Errorf("Remaining = %d, want -1 (unlimited) for nil plans", resp.Remaining)
	}
}

// TestHandler_AllNilDeps exercises the fully-degraded path (nil DB, nil tracker,
// nil plans) end to end — the endpoint must still respond 200 with an
// authenticated caller, never panic.
func TestHandler_AllNilDeps(t *testing.T) {
	cust := uuid.New()
	r := newRouter(nil, nil, nil)
	req := httptest.NewRequest(http.MethodGet, "/v1/usage", nil).WithContext(testKeyContext(cust, "free"))
	rec := httptest.NewRecorder()

	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d — body: %s", rec.Code, rec.Body.String())
	}
	resp := decodeResponse(t, rec)
	if resp.Used != 0 || resp.Cap != 0 || resp.Remaining != -1 || len(resp.Breakdown) != 0 {
		t.Errorf("unexpected fully-degraded response: %+v", resp)
	}
}
