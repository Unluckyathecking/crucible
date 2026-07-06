package respcache_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/redis/go-redis/v9"

	"github.com/Unluckyathecking/crucible/gateway/internal/auth"
	"github.com/Unluckyathecking/crucible/gateway/internal/db"
	"github.com/Unluckyathecking/crucible/gateway/internal/observability"
	"github.com/Unluckyathecking/crucible/gateway/internal/respcache"
	"github.com/Unluckyathecking/crucible/gateway/internal/usage"
)

const testSalt = "respcache-test-salt-32-bytes-pad!!"

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

// setupTestCustomer creates a customer + API key in the DB and returns the
// bearer token, so a request can be routed through the real auth.Middleware.
func setupTestCustomer(t *testing.T, pool *pgxpool.Pool) (customerID uuid.UUID, bearerToken string) {
	t.Helper()
	customerID = uuid.New()
	email := customerID.String() + "@respcachetest.local"

	_, err := pool.Exec(context.Background(), `
		INSERT INTO customers (id, email, plan_id)
		VALUES ($1, $2, 'free')
		ON CONFLICT DO NOTHING
	`, customerID, email)
	if err != nil {
		t.Fatalf("insert customer: %v", err)
	}

	fullKey, prefix, err := auth.Generate("cru_")
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	keyID := uuid.New()
	hash := auth.Hash(testSalt, fullKey)

	_, err = pool.Exec(context.Background(), `
		INSERT INTO api_keys (id, customer_id, prefix, hash, name)
		VALUES ($1, $2, $3, $4, 'test-respcache')
		ON CONFLICT DO NOTHING
	`, keyID, customerID, prefix, hash)
	if err != nil {
		t.Fatalf("insert api_key: %v", err)
	}

	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM usage_events WHERE customer_id = $1`, customerID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM api_keys WHERE id = $1`, keyID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM customers WHERE id = $1`, customerID)
	})

	return customerID, fullKey
}

func usageEventCount(t *testing.T, pool *pgxpool.Pool, customerID uuid.UUID, operation string) int {
	t.Helper()
	var count int
	err := pool.QueryRow(context.Background(), `
		SELECT COUNT(*) FROM usage_events WHERE customer_id = $1 AND operation = $2
	`, customerID, operation).Scan(&count)
	if err != nil {
		t.Fatalf("count usage_events: %v", err)
	}
	return count
}

// === Unit-level tests (no real Redis/Postgres needed) ===

func TestMiddleware_NilStore_Passthrough(t *testing.T) {
	var invoked int
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		invoked++
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	h := respcache.Middleware(nil, nil, "echo", time.Minute, nil)(inner)

	req := httptest.NewRequest(http.MethodPost, "/v1/echo", strings.NewReader(`{}`))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
	if invoked != 1 {
		t.Errorf("expected worker invoked once, got %d", invoked)
	}
}

// === Integration tests (real Redis; some also need real Postgres + auth) ===

func TestMiddleware_ZeroTTL_NeverCaches(t *testing.T) {
	rdb := newTestRedis(t)
	store := respcache.NewStore(rdb)
	operation := "echo"
	body := `{"a":1}`

	var invoked int
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		invoked++
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Billable-Units", "1")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	h := respcache.Middleware(store, nil, operation, 0, nil)(inner)

	for i := 0; i < 2; i++ {
		req := httptest.NewRequest(http.MethodPost, "/v1/echo", strings.NewReader(body))
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("call %d: expected 200, got %d", i, w.Code)
		}
	}
	if invoked != 2 {
		t.Errorf("TTL==0 route must never cache: expected worker invoked twice, got %d", invoked)
	}

	key, err := respcache.Key(operation, []byte(body))
	if err != nil {
		t.Fatalf("Key: %v", err)
	}
	if entry, err := store.Get(context.Background(), key); err != nil || entry != nil {
		t.Errorf("expected nothing stored for a TTL==0 route, got entry=%+v err=%v", entry, err)
	}
}

func TestMiddleware_Miss_CallsNextAndPopulatesCache(t *testing.T) {
	rdb := newTestRedis(t)
	ctx := context.Background()
	store := respcache.NewStore(rdb)
	operation := "echo-miss-" + time.Now().String()
	body := `{"a":1,"b":2}`
	key, err := respcache.Key(operation, []byte(body))
	if err != nil {
		t.Fatalf("Key: %v", err)
	}
	t.Cleanup(func() { rdb.Del(ctx, "respcache:"+key) })

	var invoked int
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		invoked++
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Billable-Units", "2")
		w.Header().Set("X-Units-Label", "lookups")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"result":"fresh"}`))
	})
	h := respcache.Middleware(store, nil, operation, time.Minute, nil)(inner)

	req := httptest.NewRequest(http.MethodPost, "/v1/echo", strings.NewReader(body))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if invoked != 1 {
		t.Fatalf("expected worker invoked once on a miss, got %d", invoked)
	}
	if w.Code != http.StatusOK || w.Body.String() != `{"result":"fresh"}` {
		t.Errorf("response passthrough mismatch: status=%d body=%q", w.Code, w.Body.String())
	}

	entry, err := store.Get(ctx, key)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if entry == nil {
		t.Fatal("expected the miss to populate the cache, got nothing stored")
	}
	if entry.StatusCode != http.StatusOK || string(entry.Body) != `{"result":"fresh"}` ||
		entry.BillableUnits != 2 || entry.UnitsLabel != "lookups" {
		t.Errorf("stored entry = %+v, want status=200 body={\"result\":\"fresh\"} units=2 label=lookups", entry)
	}
}

func TestMiddleware_Hit_SkipsNextServesFromCache(t *testing.T) {
	rdb := newTestRedis(t)
	ctx := context.Background()
	store := respcache.NewStore(rdb)
	operation := "echo-hit-" + time.Now().String()
	body := `{"a":1}`
	key, err := respcache.Key(operation, []byte(body))
	if err != nil {
		t.Fatalf("Key: %v", err)
	}
	t.Cleanup(func() { rdb.Del(ctx, "respcache:"+key) })

	cached := &respcache.Entry{
		StatusCode:    http.StatusOK,
		Body:          []byte(`{"result":"cached"}`),
		ContentType:   "application/json",
		BillableUnits: 5,
		UnitsLabel:    "lookups",
	}
	if err := store.Set(ctx, key, cached, time.Minute); err != nil {
		t.Fatalf("Set: %v", err)
	}

	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("worker must not be invoked on a cache hit")
	})
	h := respcache.Middleware(store, nil, operation, time.Minute, nil)(inner)

	req := httptest.NewRequest(http.MethodPost, "/v1/echo", strings.NewReader(body))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", w.Code)
	}
	if w.Body.String() != `{"result":"cached"}` {
		t.Errorf("body = %q, want cached body", w.Body.String())
	}
	if got := w.Header().Get("X-Billable-Units"); got != "5" {
		t.Errorf("X-Billable-Units = %q, want 5", got)
	}
	if got := w.Header().Get("X-Units-Label"); got != "lookups" {
		t.Errorf("X-Units-Label = %q, want lookups", got)
	}
	if got := w.Header().Get("X-Respcache"); got != "hit" {
		t.Errorf("X-Respcache = %q, want hit", got)
	}
}

func TestMiddleware_ErrorResponse_NotCached(t *testing.T) {
	rdb := newTestRedis(t)
	ctx := context.Background()
	store := respcache.NewStore(rdb)
	operation := "echo-error-" + time.Now().String()
	body := `{"a":1}`
	key, err := respcache.Key(operation, []byte(body))
	if err != nil {
		t.Fatalf("Key: %v", err)
	}
	t.Cleanup(func() { rdb.Del(ctx, "respcache:"+key) })

	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(`{"error":{"code":"WORKER_UNREACHABLE"}}`))
	})
	h := respcache.Middleware(store, nil, operation, time.Minute, nil)(inner)

	req := httptest.NewRequest(http.MethodPost, "/v1/echo", strings.NewReader(body))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502 (passthrough)", w.Code)
	}
	if entry, err := store.Get(ctx, key); err != nil || entry != nil {
		t.Errorf("a non-2xx response must never be cached, got entry=%+v err=%v", entry, err)
	}
}

func TestMiddleware_BillableUnitsBelowOne_NotCached(t *testing.T) {
	rdb := newTestRedis(t)
	ctx := context.Background()
	store := respcache.NewStore(rdb)
	operation := "echo-badunits-" + time.Now().String()
	body := `{"a":1}`
	key, err := respcache.Key(operation, []byte(body))
	if err != nil {
		t.Fatalf("Key: %v", err)
	}
	t.Cleanup(func() { rdb.Del(ctx, "respcache:"+key) })

	// Defense-in-depth: routes.go already rejects billable_units<1 with a 502
	// before invoke() ever writes 2xx, so this response shape shouldn't reach
	// Middleware in production. Verify it doesn't get cached even so.
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	h := respcache.Middleware(store, nil, operation, time.Minute, nil)(inner)

	req := httptest.NewRequest(http.MethodPost, "/v1/echo", strings.NewReader(body))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if entry, err := store.Get(ctx, key); err != nil || entry != nil {
		t.Errorf("a 2xx response without a valid X-Billable-Units header must never be cached, got entry=%+v err=%v", entry, err)
	}
}

func TestMiddleware_TTLExpiry_ReInvokesWorker(t *testing.T) {
	rdb := newTestRedis(t)
	ctx := context.Background()
	store := respcache.NewStore(rdb)
	operation := "echo-expiry-" + time.Now().String()
	body := `{"a":1}`
	key, err := respcache.Key(operation, []byte(body))
	if err != nil {
		t.Fatalf("Key: %v", err)
	}
	t.Cleanup(func() { rdb.Del(ctx, "respcache:"+key) })

	var invoked int
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		invoked++
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Billable-Units", "1")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	h := respcache.Middleware(store, nil, operation, 200*time.Millisecond, nil)(inner)

	req1 := httptest.NewRequest(http.MethodPost, "/v1/echo", strings.NewReader(body))
	h.ServeHTTP(httptest.NewRecorder(), req1)
	if invoked != 1 {
		t.Fatalf("expected worker invoked once on the first (miss) call, got %d", invoked)
	}

	req2 := httptest.NewRequest(http.MethodPost, "/v1/echo", strings.NewReader(body))
	h.ServeHTTP(httptest.NewRecorder(), req2)
	if invoked != 1 {
		t.Fatalf("expected worker NOT invoked on the second (hit) call, got %d total invocations", invoked)
	}

	time.Sleep(400 * time.Millisecond)

	req3 := httptest.NewRequest(http.MethodPost, "/v1/echo", strings.NewReader(body))
	h.ServeHTTP(httptest.NewRecorder(), req3)
	if invoked != 2 {
		t.Errorf("expected worker invoked again after TTL expiry, got %d total invocations", invoked)
	}
}

// TestMiddleware_HitStillMeters is the "hit still records usage" acceptance
// criterion: a cache hit must insert a usage_events row via recorder.Record,
// even though the worker (and invoke()'s own recorder.Record call) never runs.
func TestMiddleware_HitStillMeters(t *testing.T) {
	rdb := newTestRedis(t)
	pool := newTestPool(t)
	if err := db.Apply(context.Background(), pool); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}

	authStore := auth.NewStore(pool, rdb, testSalt)
	t.Cleanup(authStore.Close)
	customerID, bearerToken := setupTestCustomer(t, pool)
	recorder := usage.NewRecorder(pool, nil)

	store := respcache.NewStore(rdb)
	operation := "echo-meters-" + time.Now().String()
	body := `{"a":1}`
	key, err := respcache.Key(operation, []byte(body))
	if err != nil {
		t.Fatalf("Key: %v", err)
	}
	t.Cleanup(func() { rdb.Del(context.Background(), "respcache:"+key) })

	cached := &respcache.Entry{
		StatusCode:    http.StatusOK,
		Body:          []byte(`{"result":"cached"}`),
		ContentType:   "application/json",
		BillableUnits: 1,
	}
	if err := store.Set(context.Background(), key, cached, time.Minute); err != nil {
		t.Fatalf("Set: %v", err)
	}

	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("worker must not be invoked on a cache hit")
	})
	h := auth.Middleware(authStore)(respcache.Middleware(store, recorder, operation, time.Minute, nil)(inner))

	before := usageEventCount(t, pool, customerID, operation)

	req := httptest.NewRequest(http.MethodPost, "/v1/echo", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+bearerToken)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}

	after := usageEventCount(t, pool, customerID, operation)
	if after != before+1 {
		t.Errorf("usage_events count = %d, want %d (a cache hit must still meter usage)", after, before+1)
	}
}

// TestMiddleware_HitsCounter asserts that a seeded-Redis cache hit increments
// crucible_respcache_hits_total and does not increment the miss counter.
func TestMiddleware_HitsCounter(t *testing.T) {
	rdb := newTestRedis(t)
	ctx := context.Background()
	store := respcache.NewStore(rdb)
	metrics := observability.NewMetricsForTest(prometheus.NewRegistry())
	operation := "echo-hits-counter-" + time.Now().String()
	body := `{"a":1}`
	key, err := respcache.Key(operation, []byte(body))
	if err != nil {
		t.Fatalf("Key: %v", err)
	}
	t.Cleanup(func() { rdb.Del(ctx, "respcache:"+key) })

	cached := &respcache.Entry{
		StatusCode:    http.StatusOK,
		Body:          []byte(`{"result":"cached"}`),
		ContentType:   "application/json",
		BillableUnits: 1,
	}
	if err := store.Set(ctx, key, cached, time.Minute); err != nil {
		t.Fatalf("Set: %v", err)
	}

	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("worker must not be invoked on a cache hit")
	})
	h := respcache.Middleware(store, nil, operation, time.Minute, metrics)(inner)

	req := httptest.NewRequest(http.MethodPost, "/v1/echo", strings.NewReader(body))
	h.ServeHTTP(httptest.NewRecorder(), req)

	if got := testutil.ToFloat64(metrics.RespCacheHitsTotal.WithLabelValues(operation)); got != 1 {
		t.Errorf("hits counter = %v, want 1", got)
	}
	if got := testutil.ToFloat64(metrics.RespCacheMissesTotal.WithLabelValues(operation)); got != 0 {
		t.Errorf("misses counter = %v, want 0 on a hit", got)
	}
}

// TestMiddleware_MissesCounter asserts that a cache miss increments
// crucible_respcache_misses_total and does not increment the hit counter.
func TestMiddleware_MissesCounter(t *testing.T) {
	rdb := newTestRedis(t)
	store := respcache.NewStore(rdb)
	metrics := observability.NewMetricsForTest(prometheus.NewRegistry())
	operation := "echo-misses-counter-" + time.Now().String()
	body := `{"a":1}`
	key, err := respcache.Key(operation, []byte(body))
	if err != nil {
		t.Fatalf("Key: %v", err)
	}
	t.Cleanup(func() { rdb.Del(context.Background(), "respcache:"+key) })

	var invoked int
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		invoked++
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Billable-Units", "1")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"result":"fresh"}`))
	})
	h := respcache.Middleware(store, nil, operation, time.Minute, metrics)(inner)

	req := httptest.NewRequest(http.MethodPost, "/v1/echo", strings.NewReader(body))
	h.ServeHTTP(httptest.NewRecorder(), req)

	if invoked != 1 {
		t.Fatalf("expected worker invoked once on a miss, got %d", invoked)
	}
	if got := testutil.ToFloat64(metrics.RespCacheMissesTotal.WithLabelValues(operation)); got != 1 {
		t.Errorf("misses counter = %v, want 1", got)
	}
	if got := testutil.ToFloat64(metrics.RespCacheHitsTotal.WithLabelValues(operation)); got != 0 {
		t.Errorf("hits counter = %v, want 0 on a miss", got)
	}
}

// TestMiddleware_Counter_FailOpenIncrements asserts that a Redis store.Get error
// increments crucible_respcache_failopen_total and the request is still served.
func TestMiddleware_Counter_FailOpenIncrements(t *testing.T) {
	// A client pointing at a port with nothing listening returns an error
	// immediately, triggering the fail-open path without needing to kill real Redis.
	badRDB := redis.NewClient(&redis.Options{
		Addr:        "localhost:19999",
		DialTimeout: 100 * time.Millisecond,
	})
	t.Cleanup(func() { _ = badRDB.Close() })
	store := respcache.NewStore(badRDB)

	operation := "echo-counter-failopen"
	body := `{"a":1}`

	var invoked int
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		invoked++
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	h := respcache.Middleware(store, nil, operation, time.Minute, nil)(inner)

	before := testutil.ToFloat64(observability.RespCacheFailOpenTotal.WithLabelValues(operation))

	req := httptest.NewRequest(http.MethodPost, "/v1/echo", strings.NewReader(body))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("fail-open must still serve the response: status = %d, want 200", w.Code)
	}
	if invoked != 1 {
		t.Errorf("fail-open must invoke the worker: invoked = %d, want 1", invoked)
	}

	after := testutil.ToFloat64(observability.RespCacheFailOpenTotal.WithLabelValues(operation))
	if after != before+1 {
		t.Errorf("crucible_respcache_failopen_total{operation=%q} = %v, want %v", operation, after, before+1)
	}
}
