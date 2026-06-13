// Package scenarios_test exercises the full gateway middleware pipeline end-to-end
// using real Postgres and Redis via the harness package.
// Requires POSTGRES_DSN and REDIS_URL; tests skip when either is unset.
package scenarios_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Unluckyathecking/crucible/gateway/test/harness"
)

// newTestHTTPClient returns a fresh http.Client for a single test. Create one client
// per test (not per request) to avoid TCP connection churn and to satisfy the stated
// intent of per-test isolation. Each test drives its own httptest.Server, so per-test
// clients avoid cross-test connection-pool interference under t.Parallel(). httptest.NewServer
// uses HTTP/1.1 (non-TLS), so all requests use HTTP/1.1.
func newTestHTTPClient(t *testing.T) *http.Client {
	t.Helper()
	c := &http.Client{
		Timeout: 15 * time.Second,
		Transport: &http.Transport{
			DialContext:         (&net.Dialer{Timeout: 5 * time.Second}).DialContext,
			MaxIdleConns:        10,
			MaxIdleConnsPerHost: 5,
			MaxConnsPerHost:     10,
			IdleConnTimeout:     30 * time.Second,
		},
	}
	t.Cleanup(c.CloseIdleConnections)
	return c
}

// ---- helpers ----------------------------------------------------------------

func postgresDSN(t *testing.T) string {
	t.Helper()
	v := os.Getenv("POSTGRES_DSN")
	if v == "" {
		t.Fatal("POSTGRES_DSN not set; required for integration tests")
	}
	return v
}

func redisURL(t *testing.T) string {
	t.Helper()
	v := os.Getenv("REDIS_URL")
	if v == "" {
		t.Fatal("REDIS_URL not set; required for integration tests")
	}
	return v
}

func baseOpts(t *testing.T, worker http.Handler, mutators ...func(*harness.Options)) harness.Options {
	t.Helper()
	opts := harness.Options{
		WorkerHandler: worker,
		DSN:           postgresDSN(t),
		RedisURL:      redisURL(t),
	}
	for _, fn := range mutators {
		fn(&opts)
	}
	return opts
}

// echoWorker responds to POST /invoke with a fixed billable_units payload.
func echoWorker(billableUnits uint64) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"payload":{},"billable_units":%d}`, billableUnits)
	})
}

// countingWorker wraps echoWorker with an atomic invocation counter.
func countingWorker(billableUnits uint64) (http.Handler, *atomic.Int64) {
	var count atomic.Int64
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count.Add(1)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"payload":{},"billable_units":%d}`, billableUnits)
	})
	return h, &count
}

// varyingWorker embeds the invocation count in the payload so each response is unique.
func varyingWorker() (http.Handler, *atomic.Int64) {
	var count atomic.Int64
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := count.Add(1)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"payload":{"n":%d},"billable_units":1}`, n)
	})
	return h, &count
}

// slowWorker delays the response by delay to trigger the proxy timeout.
func slowWorker(delay time.Duration) (http.Handler, *atomic.Bool) {
	var invoked atomic.Bool
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		invoked.Store(true)
		timer := time.NewTimer(delay)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-r.Context().Done():
			// Return without writing to w. The HTTP server closes the connection
			// with no response, which the gateway proxy maps to 502 WORKER_UNREACHABLE.
			return
		}
		// Guard: if both channels were ready simultaneously Go picks randomly;
		// check context before writing to avoid sending on a cancelled connection.
		if r.Context().Err() != nil {
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"payload":{},"billable_units":1}`)
	})
	return h, &invoked
}

// waitForErrorEvents polls CountErrorEvents until want rows exist or a 5-second
// deadline elapses. The error recorder writes asynchronously, so callers must
// wait rather than asserting immediately after the triggering request returns.
func waitForErrorEvents(t *testing.T, ts *harness.TestServer, customerID uuid.UUID, want int64) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		n := ts.CountErrorEvents(t, customerID)
		if n == want {
			return
		}
		if n > want {
			t.Fatalf("too many error_events for customer %s: got %d, want %d", customerID, n, want)
		}
		select {
		case <-ticker.C:
		case <-ctx.Done():
			t.Fatalf("timeout waiting for %d error_events for customer %s", want, customerID)
		}
	}
}

// invoke sends POST /v1/echo to the gateway. client must be created once per test
// via newTestHTTPClient() and reused across calls. Callers must drain and close the
// response body via drainBody.
func invoke(t *testing.T, client *http.Client, ts *harness.TestServer, apiKey string, mutators ...func(*http.Request)) *http.Response {
	t.Helper()
	if ts == nil || ts.Server == nil {
		t.Fatal("invoke: ts and ts.Server must be non-nil")
	}
	if apiKey == "" {
		t.Fatal("invoke: apiKey must be non-empty")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		ts.Server.URL+"/v1/echo",
		strings.NewReader(`{"input":"scenario-test"}`),
	)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")
	for _, fn := range mutators {
		fn(req)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	// Callers must drain and close the response body via drainBody; drainBody is the sole closer.
	return resp
}

// drainBody reads and closes the response body, returning its bytes.
func drainBody(t *testing.T, r *http.Response) []byte {
	t.Helper()
	defer r.Body.Close()
	b, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return b
}

// errorCode extracts the top-level error.code from an apierror envelope.
// Using a pointer for the Error field lets us distinguish "error key absent from JSON"
// from "error.code is an empty string"; both call t.Fatalf with the raw body.
func errorCode(t *testing.T, body []byte) string {
	t.Helper()
	var env struct {
		Error *struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("decode apierror envelope: %v\nbody: %s", err, body)
	}
	if env.Error == nil || env.Error.Code == "" {
		t.Fatalf("apierror envelope missing error.code\nbody: %s", body)
	}
	if env.Error.Message == "" {
		t.Fatalf("apierror envelope missing error.message\nbody: %s", body)
	}
	return env.Error.Code
}

// ---- scenarios --------------------------------------------------------------

// TestHappyPath: authenticated POST /v1/echo → 200, correct billable_units, one usage row.
func TestHappyPath(t *testing.T) {
	t.Parallel()
	client := newTestHTTPClient(t)
	ts := harness.NewGatewayTestServer(t, baseOpts(t, echoWorker(3)))
	ts.CreatePlan(t, "hp-plan", 100, 10000)
	customerID, apiKey := ts.CreateCustomer(t, "happy-path-"+uuid.New().String()+"@example.com", "hp-plan")

	resp := invoke(t, client, ts, apiKey)
	body := drainBody(t, resp)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", resp.StatusCode, body)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type: got %q, want application/json", ct)
	}
	if v := resp.Header.Get("X-Idempotent-Replayed"); v != "" {
		t.Errorf("X-Idempotent-Replayed: got %q, want absent on non-replay request", v)
	}

	var inv struct {
		Payload       json.RawMessage `json:"payload"`
		BillableUnits uint64          `json:"billable_units"`
	}
	if err := json.Unmarshal(body, &inv); err != nil {
		t.Fatalf("decode response: %v\nbody: %s", err, body)
	}
	if inv.BillableUnits != 3 {
		t.Errorf("billable_units: got %d, want 3", inv.BillableUnits)
	}
	if len(inv.Payload) == 0 || string(inv.Payload) == "null" {
		t.Errorf("payload: got empty or null, want non-empty object from worker")
	}

	// usage.Recorder.Record is synchronous; the row is committed before the HTTP response.
	if n := ts.CountUsageEvents(t, customerID); n != 1 {
		t.Errorf("usage_events row count: got %d, want 1", n)
	}
}

// TestIdempotentReplay: same Idempotency-Key twice returns cached response; worker invoked once.
func TestIdempotentReplay(t *testing.T) {
	t.Parallel()
	client := newTestHTTPClient(t)
	worker, invocations := varyingWorker()
	ts := harness.NewGatewayTestServer(t, baseOpts(t, worker))
	ts.CreatePlan(t, "ir-plan", 100, 10000)
	customerID, apiKey := ts.CreateCustomer(t, "idempotent-replay-"+uuid.New().String()+"@example.com", "ir-plan")

	idempKey := "scenario-idemp-" + uuid.New().String()
	withIdemp := func(r *http.Request) { r.Header.Set("Idempotency-Key", idempKey) }

	r1 := invoke(t, client, ts, apiKey, withIdemp)
	if v := r1.Header.Get("X-Idempotent-Replayed"); v != "" {
		t.Errorf("first request: X-Idempotent-Replayed: got %q, want absent", v)
	}
	body1 := drainBody(t, r1)
	if r1.StatusCode != http.StatusOK {
		t.Fatalf("first request: want 200, got %d: %s", r1.StatusCode, body1)
	}

	// Synchrony assertion: the idempotency middleware persists the row before
	// writing the HTTP response, so drainBody guarantees visibility. This
	// catches any regression where the write becomes asynchronous.
	if !ts.HasIdempotencyKey(t, customerID, idempKey) {
		t.Fatalf("idempotency_keys: row not found for key %q after first request", idempKey)
	}

	r2 := invoke(t, client, ts, apiKey, withIdemp)
	if v := r2.Header.Get("X-Idempotent-Replayed"); v != "true" {
		t.Errorf("replay request: X-Idempotent-Replayed: got %q, want \"true\"", v)
	}
	body2 := drainBody(t, r2)
	if r2.StatusCode != http.StatusOK {
		t.Fatalf("replay request: want 200, got %d: %s", r2.StatusCode, body2)
	}

	if string(body1) != string(body2) {
		t.Errorf("replayed body differs:\n  first:  %s\n  second: %s", body1, body2)
	}
	var replayed struct {
		Payload       struct{ N int64 `json:"n"` } `json:"payload"`
		BillableUnits uint64                       `json:"billable_units"`
	}
	if err := json.Unmarshal(body2, &replayed); err != nil {
		t.Fatalf("decode replayed body: %v\nbody: %s", err, body2)
	}
	if replayed.BillableUnits != 1 {
		t.Errorf("replayed billable_units: got %d, want 1", replayed.BillableUnits)
	}
	if replayed.Payload.N != 1 {
		t.Errorf("replayed payload.n: got %d, want 1 (cached first invocation)", replayed.Payload.N)
	}
	if got := invocations.Load(); got != 1 {
		t.Errorf("worker invocations: got %d, want 1", got)
	}
	// Idempotency row must survive the replay (not be deleted after cache hit).
	if !ts.HasIdempotencyKey(t, customerID, idempKey) {
		t.Fatalf("idempotency_keys: row not found for key %q after replay request", idempKey)
	}
	// Two requests, one worker call; idempotent replay must not double-bill.
	if n := ts.CountUsageEvents(t, customerID); n != 1 {
		t.Errorf("usage_events after idempotent replay: got %d, want 1 (replay must not bill again)", n)
	}
}

// TestRateLimit: (limit+1)-th request returns 429 RATE_LIMITED with headers.
// ratelimit.Bucket uses a 60-second sliding window (not a fixed minute boundary),
// so all three requests — issued within milliseconds of each other — always land
// in the same window and the third is reliably rejected.
func TestRateLimit(t *testing.T) {
	t.Parallel()
	client := newTestHTTPClient(t)
	const rateLimit = 2
	ts := harness.NewGatewayTestServer(t, baseOpts(t, echoWorker(1)))
	ts.CreatePlan(t, "rl-2-plan", rateLimit, 10000)
	customerID, apiKey := ts.CreateCustomer(t, "rate-limit-"+uuid.New().String()+"@example.com", "rl-2-plan")

	for i := 0; i < rateLimit; i++ {
		r := invoke(t, client, ts, apiKey)
		b := drainBody(t, r)
		if r.StatusCode != http.StatusOK {
			t.Fatalf("request %d: want 200, got %d: %s", i+1, r.StatusCode, b)
		}
	}

	// Capture time just before the rate-limited request for tighter Retry-After bounds.
	reqTime := time.Now().Unix()
	r := invoke(t, client, ts, apiKey)
	body := drainBody(t, r)
	if r.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("third request: want 429, got %d: %s", r.StatusCode, body)
	}
	if code := errorCode(t, body); code != "RATE_LIMITED" {
		t.Errorf("error.code: got %q, want RATE_LIMITED", code)
	}
	ra := r.Header.Get("Retry-After")
	if ra == "" {
		t.Fatalf("Retry-After header missing on 429 RATE_LIMITED response")
	}
	n, err := strconv.Atoi(ra)
	if err != nil {
		t.Fatalf("Retry-After: got %q, want integer seconds: %v", ra, err)
	}
	if n < 1 || n > 60 {
		t.Errorf("Retry-After: got %d, want in [1,60]", n)
	}
	if v := r.Header.Get("RateLimit-Limit"); v != "2" {
		t.Errorf("RateLimit-Limit: got %q, want 2", v)
	}
	if rrv := r.Header.Get("RateLimit-Remaining"); rrv == "" {
		t.Errorf("RateLimit-Remaining header missing on 429 RATE_LIMITED response")
	} else if rrv != "0" {
		t.Errorf("RateLimit-Remaining: got %q, want 0", rrv)
	}
	raReset := r.Header.Get("RateLimit-Reset")
	if raReset == "" {
		t.Errorf("RateLimit-Reset: missing, want Unix timestamp")
	} else if resetUnix, parseErr := strconv.ParseInt(raReset, 10, 64); parseErr != nil {
		t.Errorf("RateLimit-Reset: got %q, want valid Unix timestamp: %v", raReset, parseErr)
	} else if resetUnix < reqTime-2 || resetUnix > reqTime+61 {
		t.Errorf("RateLimit-Reset: got %d, want Unix timestamp within 61s of request time (%d)", resetUnix, reqTime)
	}
	// Only the rateLimit accepted requests must have been billed; the rejected request must not.
	if n := ts.CountUsageEvents(t, customerID); n != int64(rateLimit) {
		t.Errorf("usage_events after rate limit: got %d, want %d (rejected request must not bill)", n, rateLimit)
	}
}

// TestQuotaExceeded: second request exceeds monthly cap of 1 billable unit; returns 429 QUOTA_EXCEEDED.
// First request: quota middleware admits it (counter 0 < cap 1), usage recorder writes a usage_events row.
// Second request: quota middleware denies it (counter 1 ≥ cap 1); no usage_events row is written.
func TestQuotaExceeded(t *testing.T) {
	t.Parallel()
	client := newTestHTTPClient(t)
	ts := harness.NewGatewayTestServer(t, baseOpts(t, echoWorker(1)))
	ts.CreatePlan(t, "quota-1-plan", 100, 1)
	customerID, apiKey := ts.CreateCustomer(t, "quota-exceeded-"+uuid.New().String()+"@example.com", "quota-1-plan")

	r1 := invoke(t, client, ts, apiKey)
	body1 := drainBody(t, r1)
	if r1.StatusCode != http.StatusOK {
		t.Fatalf("first request: want 200, got %d: %s", r1.StatusCode, body1)
	}
	if n := ts.CountUsageEvents(t, customerID); n != 1 {
		t.Errorf("after first request: usage_events count = %d, want 1", n)
	}

	r2 := invoke(t, client, ts, apiKey)
	body2 := drainBody(t, r2)
	if r2.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("second request: want 429, got %d: %s", r2.StatusCode, body2)
	}
	if code := errorCode(t, body2); code != "QUOTA_EXCEEDED" {
		t.Errorf("error.code: got %q, want QUOTA_EXCEEDED", code)
	}
	if n := ts.CountUsageEvents(t, customerID); n != 1 {
		t.Errorf("after denied request: usage_events count = %d, want 1 (no row for rejected call)", n)
	}
}

// TestWorkerTimeout: worker that sleeps past proxy deadline returns 502 WORKER_UNREACHABLE.
func TestWorkerTimeout(t *testing.T) {
	t.Parallel()
	// 2 s worker delay vs 100 ms proxy timeout: 20× ratio ensures the proxy timeout
	// fires reliably even under -race and heavy CI parallelism, while keeping total
	// test time bounded.
	client := newTestHTTPClient(t)
	worker, invoked := slowWorker(2 * time.Second)
	ts := harness.NewGatewayTestServer(t, baseOpts(t, worker, func(o *harness.Options) {
		o.WorkerTimeoutMS = 100
	}))
	ts.CreatePlan(t, "timeout-plan", 100, 10000)
	customerID, apiKey := ts.CreateCustomer(t, "worker-timeout-"+uuid.New().String()+"@example.com", "timeout-plan")

	r := invoke(t, client, ts, apiKey)
	body := drainBody(t, r)

	// The proxy maps all transport errors (including client timeout) to 502 WORKER_UNREACHABLE
	// per the contract documented in internal/proxy/client.go. 504 is not returned.
	if r.StatusCode != http.StatusBadGateway {
		t.Fatalf("proxy timeout: want %d, got %d: %s", http.StatusBadGateway, r.StatusCode, body)
	}
	if code := errorCode(t, body); code != "WORKER_UNREACHABLE" {
		t.Errorf("error.code: got %q, want WORKER_UNREACHABLE", code)
	}
	if n := ts.CountUsageEvents(t, customerID); n != 0 {
		t.Errorf("usage_events after timeout: got %d rows, want 0", n)
	}
	// waitForErrorEvents before invoked.Load: the async errorlog goroutine writes
	// the error event only after the handler goroutine has run, so waiting for the
	// event guarantees invoked.Store(true) has already executed.
	waitForErrorEvents(t, ts, customerID, 1)
	if !invoked.Load() {
		t.Error("worker was never invoked; proxy may have short-circuited before forwarding")
	}
}

// TestAuthFailure: a key not registered in the database returns 401 UNAUTHORIZED.
func TestAuthFailure(t *testing.T) {
	t.Parallel()
	client := newTestHTTPClient(t)
	ts := harness.NewGatewayTestServer(t, baseOpts(t, echoWorker(1)))

	// Synthesise a key that matches the production prefix format but is guaranteed
	// absent from the database: prefix + 32 uppercase zeros (not base32-random) +
	// UUID suffix ensures uniqueness without depending on key generation internals.
	r := invoke(t, client, ts, harness.TestAPIKeyPrefix+strings.Repeat("A", 32)+uuid.New().String())
	body := drainBody(t, r)
	if r.StatusCode != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d: %s", r.StatusCode, body)
	}
	if code := errorCode(t, body); code != "UNAUTHORIZED" {
		t.Errorf("error.code: got %q, want UNAUTHORIZED", code)
	}
}

// TestWorkerBadResponse: a worker that returns billable_units=0 gets 502 WORKER_BAD_RESPONSE.
// The gateway enforces billable_units >= 1 at the trust boundary (internal/server/routes.go)
// to prevent a buggy or non-SDK worker from granting free usage.
func TestWorkerBadResponse(t *testing.T) {
	t.Parallel()
	client := newTestHTTPClient(t)
	ts := harness.NewGatewayTestServer(t, baseOpts(t, echoWorker(0)))
	ts.CreatePlan(t, "bad-resp-plan", 10, 1000)
	customerID, apiKey := ts.CreateCustomer(t, "bad-resp-"+uuid.New().String()+"@example.com", "bad-resp-plan")

	r := invoke(t, client, ts, apiKey)
	body := drainBody(t, r)
	if r.StatusCode != http.StatusBadGateway {
		t.Fatalf("want 502, got %d: %s", r.StatusCode, body)
	}
	if code := errorCode(t, body); code != "WORKER_BAD_RESPONSE" {
		t.Errorf("error.code: got %q, want WORKER_BAD_RESPONSE", code)
	}
	if n := ts.CountUsageEvents(t, customerID); n != 0 {
		t.Errorf("usage_events after WORKER_BAD_RESPONSE: got %d, want 0", n)
	}
}

// TestNoAuthorizationHeader: a request with no Authorization header returns 401 UNAUTHORIZED.
// This exercises the "missing header" branch of the auth middleware, distinct from
// TestAuthFailure which sends a key not present in the database.
func TestNoAuthorizationHeader(t *testing.T) {
	t.Parallel()
	client := newTestHTTPClient(t)
	ts := harness.NewGatewayTestServer(t, baseOpts(t, echoWorker(1)))

	// Pass a non-empty placeholder to satisfy invoke's empty-key guard, then remove
	// the Authorization header via a mutator before the request is sent.
	r := invoke(t, client, ts, "placeholder-key", func(req *http.Request) {
		req.Header.Del("Authorization")
	})
	body := drainBody(t, r)
	if r.StatusCode != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d: %s", r.StatusCode, body)
	}
	if code := errorCode(t, body); code != "UNAUTHORIZED" {
		t.Errorf("error.code: got %q, want UNAUTHORIZED", code)
	}
}

// TestRequestIDPresent: every response carries an X-Request-ID header set by the
// RequestID middleware. The value is a valid UUID; clients use it for support-ticket correlation.
func TestRequestIDPresent(t *testing.T) {
	t.Parallel()
	client := newTestHTTPClient(t)
	ts := harness.NewGatewayTestServer(t, baseOpts(t, echoWorker(1)))
	ts.CreatePlan(t, "reqid-plan", 100, 10000)
	_, apiKey := ts.CreateCustomer(t, "reqid-"+uuid.New().String()+"@example.com", "reqid-plan")

	r := invoke(t, client, ts, apiKey)
	body := drainBody(t, r)
	if r.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", r.StatusCode, body)
	}
	rid := r.Header.Get("X-Request-ID")
	if rid == "" {
		t.Fatalf("X-Request-ID header absent on 200 response")
	}
	if _, err := uuid.Parse(rid); err != nil {
		t.Errorf("X-Request-ID %q is not a valid UUID: %v", rid, err)
	}
}

// TestIdempotencyKeyIsolation: the same Idempotency-Key string shared by two different
// customers must not cross-contaminate the cache — customer B's first request must reach
// the worker, not be served from customer A's cached entry (scoped by customer_id).
func TestIdempotencyKeyIsolation(t *testing.T) {
	t.Parallel()
	client := newTestHTTPClient(t)
	worker, _ := varyingWorker()
	ts := harness.NewGatewayTestServer(t, baseOpts(t, worker))
	ts.CreatePlan(t, "idemp-iso-plan", 100, 10000)
	_, keyA := ts.CreateCustomer(t, "idemp-iso-A-"+uuid.New().String()+"@example.com", "idemp-iso-plan")
	_, keyB := ts.CreateCustomer(t, "idemp-iso-B-"+uuid.New().String()+"@example.com", "idemp-iso-plan")

	sharedKey := "shared-idemp-" + uuid.New().String()
	withIdemp := func(r *http.Request) { r.Header.Set("Idempotency-Key", sharedKey) }

	r1 := invoke(t, client, ts, keyA, withIdemp)
	replayA := r1.Header.Get("X-Idempotent-Replayed")
	body1 := drainBody(t, r1)
	if r1.StatusCode != http.StatusOK {
		t.Fatalf("customer A first request: want 200, got %d: %s", r1.StatusCode, body1)
	}
	if replayA != "" {
		t.Errorf("customer A first request: X-Idempotent-Replayed = %q, want absent", replayA)
	}

	r2 := invoke(t, client, ts, keyB, withIdemp)
	replayB := r2.Header.Get("X-Idempotent-Replayed")
	body2 := drainBody(t, r2)
	if r2.StatusCode != http.StatusOK {
		t.Fatalf("customer B first request: want 200, got %d: %s", r2.StatusCode, body2)
	}
	if replayB != "" {
		t.Errorf("customer B first request: X-Idempotent-Replayed = %q, want absent (different customer)", replayB)
	}
	// varyingWorker embeds an incrementing counter; equal bodies would mean B was served A's cached payload.
	if string(body1) == string(body2) {
		t.Errorf("idempotency isolation failure: customers A and B received identical worker responses\nbody: %s", body1)
	}
}

// TestCrossCustomerIsolation: requests from A never appear in B's rows, and vice versa.
func TestCrossCustomerIsolation(t *testing.T) {
	t.Parallel()
	client := newTestHTTPClient(t)
	worker, invocations := countingWorker(1)
	ts := harness.NewGatewayTestServer(t, baseOpts(t, worker))
	ts.CreatePlan(t, "iso-plan", 100, 10000)

	custA, keyA := ts.CreateCustomer(t, "isolation-A-"+uuid.New().String()+"@example.com", "iso-plan")
	custB, keyB := ts.CreateCustomer(t, "isolation-B-"+uuid.New().String()+"@example.com", "iso-plan")

	rA := invoke(t, client, ts, keyA)
	drainBody(t, rA)
	if rA.StatusCode != http.StatusOK {
		t.Fatalf("customer A: want 200, got %d", rA.StatusCode)
	}

	if n := ts.CountUsageEvents(t, custB); n != 0 {
		t.Errorf("after A's request: customer B usage_events = %d, want 0", n)
	}
	if n := ts.CountErrorEvents(t, custB); n != 0 {
		t.Errorf("after A's request: customer B error_events = %d, want 0", n)
	}

	rB := invoke(t, client, ts, keyB)
	drainBody(t, rB)
	if rB.StatusCode != http.StatusOK {
		t.Fatalf("customer B: want 200, got %d", rB.StatusCode)
	}

	if got := invocations.Load(); got != 2 {
		t.Errorf("worker invocations: got %d, want 2", got)
	}
	if n := ts.CountUsageEvents(t, custA); n != 1 {
		t.Errorf("customer A usage_events = %d, want 1", n)
	}
	if n := ts.CountUsageEvents(t, custB); n != 1 {
		t.Errorf("customer B usage_events = %d, want 1", n)
	}
	if n := ts.CountErrorEvents(t, custA); n != 0 {
		t.Errorf("customer A error_events = %d, want 0", n)
	}
	if n := ts.CountErrorEvents(t, custB); n != 0 {
		t.Errorf("customer B error_events = %d, want 0", n)
	}
}

