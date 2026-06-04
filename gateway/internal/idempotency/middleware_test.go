package idempotency_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/Unluckyathecking/crucible/gateway/internal/auth"
	"github.com/Unluckyathecking/crucible/gateway/internal/db"
	"github.com/Unluckyathecking/crucible/gateway/internal/idempotency"
)

// testInfra groups the real Postgres + Redis dependencies needed by integration tests.
// Tests call newTestInfra which skips if either is unavailable.
type testInfra struct {
	pool      *pgxpool.Pool
	redis     *redis.Client
	authStore *auth.Store
}

const testSalt = "idempotency-test-salt-32-bytes!!"

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

func newTestRedis(t *testing.T) *redis.Client {
	t.Helper()
	c := redis.NewClient(&redis.Options{Addr: "localhost:6379"})
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	if err := c.Ping(ctx).Err(); err != nil {
		t.Skipf("redis unavailable: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func newTestInfra(t *testing.T) *testInfra {
	t.Helper()
	pool := newTestPool(t)
	rc := newTestRedis(t)

	if err := db.Apply(context.Background(), pool); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}

	authStore := auth.NewStore(pool, rc, testSalt)
	t.Cleanup(authStore.Close)
	return &testInfra{pool: pool, redis: rc, authStore: authStore}
}

// setupTestCustomer creates a customer + API key in the DB.
// Returns the bearer token and registers cleanup.
func setupTestCustomer(t *testing.T, infra *testInfra) (customerID uuid.UUID, bearerToken string) {
	t.Helper()
	customerID = uuid.New()
	email := customerID.String() + "@idemptest.local"

	_, err := infra.pool.Exec(context.Background(), `
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

	_, err = infra.pool.Exec(context.Background(), `
		INSERT INTO api_keys (id, customer_id, prefix, hash, name)
		VALUES ($1, $2, $3, $4, 'test-idempotency')
		ON CONFLICT DO NOTHING
	`, keyID, customerID, prefix, hash)
	if err != nil {
		t.Fatalf("insert api_key: %v", err)
	}

	t.Cleanup(func() {
		_, _ = infra.pool.Exec(context.Background(),
			`DELETE FROM idempotency_keys WHERE customer_id = $1`, customerID)
		_, _ = infra.pool.Exec(context.Background(),
			`DELETE FROM api_keys WHERE id = $1`, keyID)
		_, _ = infra.pool.Exec(context.Background(),
			`DELETE FROM customers WHERE id = $1`, customerID)
	})

	return customerID, fullKey
}

// buildChain assembles: auth.Middleware → idempotency.Middleware → inner.
func buildChain(infra *testInfra, idempStore *idempotency.Store, inner http.Handler) http.Handler {
	return auth.Middleware(infra.authStore)(idempotency.Middleware(idempStore)(inner))
}

// decodeError decodes the JSON error envelope into code + message.
func decodeError(t *testing.T, body []byte) (code, message string) {
	t.Helper()
	var env struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("decode error envelope: %v (body=%q)", err, body)
	}
	return env.Error.Code, env.Error.Message
}

// === Unit-level tests (no DB/Redis) ===

func TestMiddleware_NilStore_Passthrough(t *testing.T) {
	var invoked int
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		invoked++
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	h := idempotency.Middleware(nil)(inner)

	req := httptest.NewRequest(http.MethodPost, "/v1/echo", strings.NewReader(`{}`))
	req.Header.Set("Idempotency-Key", "any-key")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
	if invoked != 1 {
		t.Errorf("expected worker invoked once, got %d", invoked)
	}
}

func TestMiddleware_NoHeader_Passthrough(t *testing.T) {
	var invoked int
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		invoked++
		w.WriteHeader(http.StatusOK)
	})
	// Use nil store; the no-header path exits before store is checked.
	h := idempotency.Middleware(nil)(inner)

	req := httptest.NewRequest(http.MethodPost, "/v1/echo", strings.NewReader(`{}`))
	// No Idempotency-Key header.
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
	if invoked != 1 {
		t.Errorf("handler must be invoked once, got %d", invoked)
	}
}

func TestMiddleware_KeyTooLong_Returns400(t *testing.T) {
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("inner handler must not be called for oversized key")
	})
	// Key-length check fires after the nil-store guard, so we need a non-nil
	// store. Use a real pool (skip if unavailable); the check never reaches DB.
	pool := newTestPool(t)
	store := idempotency.NewStore(pool)

	h := idempotency.Middleware(store)(inner)

	longKey := strings.Repeat("k", 256)
	req := httptest.NewRequest(http.MethodPost, "/v1/echo", strings.NewReader(`{}`))
	req.Header.Set("Idempotency-Key", longKey)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for oversized key, got %d", w.Code)
	}
	code, _ := decodeError(t, w.Body.Bytes())
	if code != "IDEMPOTENCY_KEY_INVALID" {
		t.Errorf("expected code IDEMPOTENCY_KEY_INVALID, got %q", code)
	}
}

// === Integration tests (real Postgres + Redis) ===

// TestMiddleware_FirstRequest_StoresResponse verifies the first-request path:
// worker is invoked, 2xx response is captured and stored.
func TestMiddleware_FirstRequest_StoresResponse(t *testing.T) {
	infra := newTestInfra(t)
	_, bearer := setupTestCustomer(t, infra)

	var invoked int32
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&invoked, 1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, `{"result":"first","billable_units":1}`)
	})
	store := idempotency.NewStore(infra.pool)
	h := buildChain(infra, store, inner)

	ikey := "first-" + uuid.New().String()
	req := httptest.NewRequest(http.MethodPost, "/v1/echo", strings.NewReader(`{"x":1}`))
	req.Header.Set("Authorization", "Bearer "+bearer)
	req.Header.Set("Idempotency-Key", ikey)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if n := atomic.LoadInt32(&invoked); n != 1 {
		t.Errorf("expected worker invoked 1 time, got %d", n)
	}
	if w.Header().Get("X-Idempotent-Replayed") != "" {
		t.Error("first request must not set X-Idempotent-Replayed")
	}
}

// TestMiddleware_Replay_WorkerNotInvoked verifies acceptance criterion 3:
// on key hit, the stored response is replayed and proxy.Invoke is NOT called.
func TestMiddleware_Replay_WorkerNotInvoked(t *testing.T) {
	infra := newTestInfra(t)
	_, bearer := setupTestCustomer(t, infra)

	var invoked int32
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&invoked, 1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, `{"result":"cached","billable_units":1}`)
	})
	store := idempotency.NewStore(infra.pool)
	h := buildChain(infra, store, inner)

	ikey := "replay-" + uuid.New().String()
	body := `{"x":1}`

	sendRequest := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/v1/echo", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+bearer)
		req.Header.Set("Idempotency-Key", ikey)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w
	}

	// First request: worker must be invoked.
	w1 := sendRequest()
	if w1.Code != http.StatusOK {
		t.Fatalf("first request: expected 200, got %d: %s", w1.Code, w1.Body.String())
	}
	if atomic.LoadInt32(&invoked) != 1 {
		t.Fatalf("first request: expected invoke count 1, got %d", atomic.LoadInt32(&invoked))
	}

	// Second request (same key, same body): worker must NOT be invoked.
	w2 := sendRequest()
	if w2.Code != http.StatusOK {
		t.Fatalf("replay: expected 200, got %d: %s", w2.Code, w2.Body.String())
	}
	if n := atomic.LoadInt32(&invoked); n != 1 {
		t.Errorf("replay: invoke count must stay 1, got %d", n)
	}
	if w2.Header().Get("X-Idempotent-Replayed") != "true" {
		t.Error("replay: expected X-Idempotent-Replayed: true header")
	}

	// Body must match the first response.
	if !bytes.Equal(w1.Body.Bytes(), w2.Body.Bytes()) {
		t.Errorf("replay body mismatch:\nfirst=%q\nreplay=%q", w1.Body.String(), w2.Body.String())
	}
}

// TestMiddleware_DifferentBody_KeyReuse verifies acceptance criterion 6:
// same key + different request body → 422 IDEMPOTENCY_KEY_REUSE.
func TestMiddleware_DifferentBody_KeyReuse(t *testing.T) {
	infra := newTestInfra(t)
	_, bearer := setupTestCustomer(t, infra)

	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, `{"billable_units":1}`)
	})
	store := idempotency.NewStore(infra.pool)
	h := buildChain(infra, store, inner)

	ikey := "reuse-" + uuid.New().String()

	sendWith := func(body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/v1/echo", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+bearer)
		req.Header.Set("Idempotency-Key", ikey)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w
	}

	// First request succeeds and stores.
	if w := sendWith(`{"x":1}`); w.Code != http.StatusOK {
		t.Fatalf("first request: expected 200, got %d", w.Code)
	}

	// Second request with different body → 422.
	w2 := sendWith(`{"x":2}`)
	if w2.Code != http.StatusUnprocessableEntity {
		t.Fatalf("different body: expected 422, got %d: %s", w2.Code, w2.Body.String())
	}
	code, _ := decodeError(t, w2.Body.Bytes())
	if code != "IDEMPOTENCY_KEY_REUSE" {
		t.Errorf("expected IDEMPOTENCY_KEY_REUSE, got %q", code)
	}
}

// TestMiddleware_Non2xx_NotCached verifies acceptance criterion 4:
// non-2xx responses are NOT stored; a subsequent retry can succeed.
func TestMiddleware_Non2xx_NotCached(t *testing.T) {
	infra := newTestInfra(t)
	_, bearer := setupTestCustomer(t, infra)

	var attempt int32
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&attempt, 1)
		w.Header().Set("Content-Type", "application/json")
		if n == 1 {
			// First attempt: simulate a retryable 5xx.
			w.WriteHeader(http.StatusBadGateway)
			_, _ = fmt.Fprint(w, `{"error":{"code":"WORKER_UNREACHABLE","message":"down","retryable":true}}`)
		} else {
			// Subsequent attempts: succeed.
			w.WriteHeader(http.StatusOK)
			_, _ = fmt.Fprint(w, `{"result":"ok","billable_units":1}`)
		}
	})
	store := idempotency.NewStore(infra.pool)
	h := buildChain(infra, store, inner)

	ikey := "retry-" + uuid.New().String()
	body := `{"x":1}`

	send := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/v1/echo", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+bearer)
		req.Header.Set("Idempotency-Key", ikey)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w
	}

	// First attempt → 502 (not cached).
	w1 := send()
	if w1.Code != http.StatusBadGateway {
		t.Fatalf("first attempt: expected 502, got %d", w1.Code)
	}

	// Second attempt (same key, same body): the row was released → worker runs again.
	w2 := send()
	if w2.Code != http.StatusOK {
		t.Fatalf("retry: expected 200, got %d: %s", w2.Code, w2.Body.String())
	}
	if atomic.LoadInt32(&attempt) != 2 {
		t.Errorf("retry: expected 2 worker invocations, got %d", atomic.LoadInt32(&attempt))
	}
}

// TestMiddleware_ConcurrentSameKey_Conflict verifies acceptance criterion 5:
// concurrent requests with the same key → one succeeds, others get 409 IDEMPOTENCY_CONFLICT.
func TestMiddleware_ConcurrentSameKey_Conflict(t *testing.T) {
	infra := newTestInfra(t)
	_, bearer := setupTestCustomer(t, infra)

	// ready is closed to unblock the winning handler after all goroutines have started.
	// handlerEntered is signalled when the winner enters the inner handler.
	ready := make(chan struct{})
	handlerEntered := make(chan struct{}, 1)
	var invoked int32
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&invoked, 1)
		select {
		case handlerEntered <- struct{}{}: // first (and only) entry signals
		default:
		}
		<-ready
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, `{"billable_units":1}`)
	})
	store := idempotency.NewStore(infra.pool)
	h := buildChain(infra, store, inner)

	ikey := "concurrent-" + uuid.New().String()
	const n = 5

	// start is closed to unblock all goroutines simultaneously — avoids the
	// WaitGroup-as-barrier anti-pattern and gives true concurrent Claim racing.
	start := make(chan struct{})

	results := make([]int, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			<-start // wait for simultaneous release
			req := httptest.NewRequest(http.MethodPost, "/v1/echo", strings.NewReader(`{"x":1}`))
			req.Header.Set("Authorization", "Bearer "+bearer)
			req.Header.Set("Idempotency-Key", ikey)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, req)
			results[idx] = w.Code
		}(i)
	}

	close(start) // unblock all goroutines simultaneously
	// Wait for the winner to enter the handler (key claimed), then unblock it.
	select {
	case <-handlerEntered:
	case <-time.After(5 * time.Second):
		close(ready) // unblock goroutines to prevent leak
		t.Fatal("timeout waiting for handler to be entered")
	}
	close(ready)
	wg.Wait()

	successes, conflicts := 0, 0
	for _, code := range results {
		switch code {
		case http.StatusOK:
			successes++
		case http.StatusConflict:
			conflicts++
		default:
			t.Errorf("unexpected status %d", code)
		}
	}
	if successes != 1 {
		t.Errorf("expected exactly 1 success, got %d (results=%v)", successes, results)
	}
	if conflicts != n-1 {
		t.Errorf("expected %d conflicts, got %d (results=%v)", n-1, conflicts, results)
	}
	if invoked != 1 {
		t.Errorf("worker must be invoked exactly once, got %d", invoked)
	}
}

// TestMiddleware_Panic_ReleasesKey verifies the panic-recovery defer:
// a panicking handler must release the claimed key so retries can succeed.
func TestMiddleware_Panic_ReleasesKey(t *testing.T) {
	infra := newTestInfra(t)
	_, bearer := setupTestCustomer(t, infra)

	var attempt int32
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&attempt, 1) == 1 {
			panic("simulated handler panic")
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, `{"result":"ok","billable_units":1}`)
	})
	store := idempotency.NewStore(infra.pool)
	h := buildChain(infra, store, inner)

	ikey := "panic-" + uuid.New().String()
	body := `{"x":1}`

	send := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/v1/echo", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+bearer)
		req.Header.Set("Idempotency-Key", ikey)
		w := httptest.NewRecorder()
		// ServeHTTP propagates panics; recover only the expected simulated panic
		// so we can assert post-panic state; re-panic on anything unexpected.
		func() {
			defer func() {
				v := recover()
				if v != nil && v != "simulated handler panic" {
					panic(v)
				}
			}()
			h.ServeHTTP(w, req)
		}()
		return w
	}

	// First request panics — key should be released by the panic defer.
	send()
	if n := atomic.LoadInt32(&attempt); n != 1 {
		t.Fatalf("first attempt: expected invoked 1 time, got %d", n)
	}

	// Second request with same key: must succeed (key was released, not stuck pending).
	w2 := send()
	if w2.Code != http.StatusOK {
		t.Fatalf("after panic release: expected 200, got %d: %s", w2.Code, w2.Body.String())
	}
	if n := atomic.LoadInt32(&attempt); n != 2 {
		t.Errorf("expected 2 invocations total, got %d", n)
	}
}

// TestMiddleware_ErrorEnvelope verifies acceptance criterion 8:
// error envelopes have the stable shape (code + message + retryable).
func TestMiddleware_ErrorEnvelope(t *testing.T) {
	pool := newTestPool(t)
	if err := db.Apply(context.Background(), pool); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}
	store := idempotency.NewStore(pool)

	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("inner handler must not be called for key-too-long")
	})
	h := idempotency.Middleware(store)(inner)

	longKey := strings.Repeat("k", 256)
	req := httptest.NewRequest(http.MethodPost, "/v1/echo", strings.NewReader(`{}`))
	req.Header.Set("Idempotency-Key", longKey)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	var top map[string]json.RawMessage
	if err := json.Unmarshal(w.Body.Bytes(), &top); err != nil {
		t.Fatalf("parse envelope: %v", err)
	}
	if len(top) != 1 {
		t.Fatalf("envelope must have exactly 1 top-level key ('error'), got %d", len(top))
	}
	errRaw, ok := top["error"]
	if !ok {
		t.Fatal("envelope missing 'error' key")
	}
	var obj struct {
		Code      string `json:"code"`
		Message   string `json:"message"`
		Retryable bool   `json:"retryable"`
	}
	dec := json.NewDecoder(bytes.NewReader(errRaw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&obj); err != nil {
		t.Fatalf("decode error object (unexpected field?): %v", err)
	}
	if obj.Code == "" {
		t.Error("error.code must not be empty")
	}
	if obj.Message == "" {
		t.Error("error.message must not be empty")
	}
	if obj.Retryable {
		t.Error("IDEMPOTENCY_KEY_INVALID must not be retryable")
	}
}
