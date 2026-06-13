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

const (
	// defaultTestRatePerMin and defaultTestMonthlyCap are used for plans in tests
	// that do not specifically exercise rate-limiting or quota; the values are
	// deliberately high so they cannot interfere with other test assertions.
	defaultTestRatePerMin = 100
	defaultTestMonthlyCap = 10_000

	// HTTP client constants for newTestHTTPClient.
	testClientTimeout       = 25 * time.Second // must exceed testDialTimeout + testRequestTimeout + body-drain margin
	testDialTimeout         = 5 * time.Second
	testIdleConnTimeout     = 30 * time.Second
	testMaxIdleConns        = 10
	testMaxIdleConnsPerHost = 5
	testMaxConnsPerHost     = 10

	// Request context and polling timeouts.
	testRequestTimeout = 10 * time.Second
	errorPollTimeout   = 5 * time.Second
	errorPollInterval  = 100 * time.Millisecond
)

// newTestHTTPClient returns a fresh http.Client for a single test. Create one client
// per test (not per request) to avoid TCP connection churn and to satisfy the stated
// intent of per-test isolation. Each test drives its own httptest.Server, so per-test
// clients avoid cross-test connection-pool interference under t.Parallel(). httptest.NewServer
// uses HTTP/1.1 (non-TLS), so all requests use HTTP/1.1.
func newTestHTTPClient(t *testing.T) *http.Client {
	t.Helper()
	c := &http.Client{
		Timeout: testClientTimeout,
		Transport: &http.Transport{
			DialContext:         (&net.Dialer{Timeout: testDialTimeout}).DialContext,
			MaxIdleConns:        testMaxIdleConns,
			MaxIdleConnsPerHost: testMaxIdleConnsPerHost,
			MaxConnsPerHost:     testMaxConnsPerHost,
			IdleConnTimeout:     testIdleConnTimeout,
			ForceAttemptHTTP2:   false,
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
		_, _ = fmt.Fprintf(w, `{"payload":{},"billable_units":%d}`, billableUnits)
	})
}

// countingWorker wraps echoWorker with an atomic invocation counter.
func countingWorker(billableUnits uint64) (http.Handler, *atomic.Int64) {
	var count atomic.Int64
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"payload":{},"billable_units":%d}`, billableUnits)
	})
	return h, &count
}

// varyingWorker embeds the invocation count in the payload so each response is unique.
func varyingWorker() (http.Handler, *atomic.Int64) {
	var count atomic.Int64
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := count.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"payload":{"n":%d},"billable_units":1}`, n)
	})
	return h, &count
}

// slowWorker delays the response by delay to trigger the proxy timeout.
// The proxy cancels the request context on timeout; the handler returns
// immediately on r.Context().Done() to avoid writing to a closed response.
func slowWorker(delay time.Duration) (http.Handler, *atomic.Bool) {
	var invoked atomic.Bool
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		invoked.Store(true)
		select {
		case <-time.After(delay):
			if r.Context().Err() != nil {
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprint(w, `{"payload":{},"billable_units":1}`)
		case <-r.Context().Done():
		}
	})
	return h, &invoked
}

// waitForErrorEvents polls CountErrorEvents until want rows exist or a 5-second
// deadline elapses. The error recorder writes asynchronously, so callers must
// wait rather than asserting immediately after the triggering request returns.
// The condition is checked immediately on each iteration before sleeping, so if
// the condition is already met there is no 100ms delay before returning.
func waitForErrorEvents(t *testing.T, ts *harness.TestServer, customerID uuid.UUID, want int64) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), errorPollTimeout)
	defer cancel()
	for {
		n := ts.CountErrorEvents(t, customerID)
		if n == want {
			return
		}
		if n > want {
			t.Fatalf("too many error_events for customer %s: got %d, want %d", customerID, n, want)
		}
		select {
		case <-time.After(errorPollInterval):
		case <-ctx.Done():
			t.Fatalf("timeout waiting for %d error_events for customer %s", want, customerID)
		}
	}
}

// invoke sends POST /v1/echo to the gateway. client must be created once per test
// via newTestHTTPClient() and reused across calls. drainBody is the sole closer of
// the response body; no t.Cleanup is registered here for body close.
func invoke(t *testing.T, client *http.Client, ts *harness.TestServer, apiKey string, mutators ...func(*http.Request)) *http.Response {
	t.Helper()
	if ts == nil || ts.Server == nil {
		t.Fatal("invoke: ts and ts.Server must be non-nil")
	}
	if apiKey == "" {
		t.Fatal("invoke: apiKey must be non-empty")
	}
	ctx, cancel := context.WithTimeout(context.Background(), testRequestTimeout)
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
		t.Fatalf("send request: %v", err)
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

// invokeNoAuth sends POST /v1/echo without an Authorization header. Use for
// tests that exercise the missing-auth path without needing a registered key.
// Callers must drain and close the response body via drainBody; drainBody is the sole closer.
func invokeNoAuth(t *testing.T, client *http.Client, ts *harness.TestServer, mutators ...func(*http.Request)) *http.Response {
	t.Helper()
	if ts == nil || ts.Server == nil {
		t.Fatal("invokeNoAuth: ts and ts.Server must be non-nil")
	}
	ctx, cancel := context.WithTimeout(context.Background(), testRequestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		ts.Server.URL+"/v1/echo",
		strings.NewReader(`{"input":"scenario-test"}`),
	)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	for _, fn := range mutators {
		fn(req)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("send request: %v", err)
	}
	return resp
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
	return env.Error.Code
}

// ---- scenarios --------------------------------------------------------------

// TestHappyPath: authenticated POST /v1/echo → 200, correct billable_units, one usage row.
func TestHappyPath(t *testing.T) {
	t.Parallel()
	client := newTestHTTPClient(t)
	ts := harness.NewGatewayTestServer(t, baseOpts(t, echoWorker(3)))
	ts.CreatePlan(t, "hp-plan", defaultTestRatePerMin, defaultTestMonthlyCap)
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
	ts.CreatePlan(t, "ir-plan", defaultTestRatePerMin, defaultTestMonthlyCap)
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
	if got, want := replayed.BillableUnits, uint64(1); got != want {
		t.Errorf("replayed billable_units: got %d, want %d", got, want)
	}
	// N must equal 1: the replay must return the first invocation's cached payload,
	// not invoke varyingWorker again (which would return N=2).
	if got, want := replayed.Payload.N, int64(1); got != want {
		t.Errorf("replayed payload.n: got %d, want %d", got, want)
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
	ts.CreatePlan(t, "rl-2-plan", rateLimit, defaultTestMonthlyCap)
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
		t.Errorf("Retry-After header missing on 429 RATE_LIMITED response")
	} else if n, err := strconv.Atoi(ra); err != nil {
		t.Fatalf("Retry-After: got %q, want integer seconds: %v", ra, err)
	} else if n < 1 || n > 60 {
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
	} else if resetUnix < reqTime-1 || resetUnix > reqTime+60 {
		t.Errorf("RateLimit-Reset: got %d, want Unix timestamp in [%d, %d]", resetUnix, reqTime-1, reqTime+60)
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
	ts.CreatePlan(t, "quota-1-plan", defaultTestRatePerMin, 1)
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
	ts.CreatePlan(t, "timeout-plan", defaultTestRatePerMin, defaultTestMonthlyCap)
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
	ts.CreatePlan(t, "bad-resp-plan", defaultTestRatePerMin, defaultTestMonthlyCap)
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
	waitForErrorEvents(t, ts, customerID, 1)
}

// TestNoAuthorizationHeader: a request with no Authorization header returns 401 UNAUTHORIZED.
// This exercises the "missing header" branch of the auth middleware, distinct from
// TestAuthFailure which sends a key not present in the database.
func TestNoAuthorizationHeader(t *testing.T) {
	t.Parallel()
	client := newTestHTTPClient(t)
	ts := harness.NewGatewayTestServer(t, baseOpts(t, echoWorker(1)))

	r := invokeNoAuth(t, client, ts)
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
	ts.CreatePlan(t, "reqid-plan", defaultTestRatePerMin, defaultTestMonthlyCap)
	customerID, apiKey := ts.CreateCustomer(t, "reqid-"+uuid.New().String()+"@example.com", "reqid-plan")

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
	if n := ts.CountUsageEvents(t, customerID); n != 1 {
		t.Errorf("usage_events: got %d, want 1", n)
	}
}

// TestIdempotencyKeyIsolation: the same Idempotency-Key string shared by two different
// customers must not cross-contaminate the cache — customer B's first request must reach
// the worker, not be served from customer A's cached entry (scoped by customer_id).
func TestIdempotencyKeyIsolation(t *testing.T) {
	t.Parallel()
	client := newTestHTTPClient(t)
	worker, invocations := varyingWorker()
	ts := harness.NewGatewayTestServer(t, baseOpts(t, worker))
	ts.CreatePlan(t, "idemp-iso-plan", defaultTestRatePerMin, defaultTestMonthlyCap)
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
	if got := invocations.Load(); got != 2 {
		t.Errorf("worker invocations: got %d, want 2 (one per customer)", got)
	}
}

// TestRateLimitHeadersOnSuccess verifies that the rate-limit middleware injects
// RateLimit-Limit and RateLimit-Remaining headers on every 200 response, not
// only on 429 rejections.
func TestRateLimitHeadersOnSuccess(t *testing.T) {
	t.Parallel()
	// rlHdrLimit is the plan's rate-per-minute; it appears in the RateLimit-Limit
	// header and is asserted below, so the value is intentionally different from
	// defaultTestRatePerMin.
	const rlHdrLimit = 10
	client := newTestHTTPClient(t)
	ts := harness.NewGatewayTestServer(t, baseOpts(t, echoWorker(1)))
	ts.CreatePlan(t, "rl-hdr-plan", rlHdrLimit, defaultTestMonthlyCap)
	_, apiKey := ts.CreateCustomer(t, "rl-hdr-"+uuid.New().String()+"@example.com", "rl-hdr-plan")

	r := invoke(t, client, ts, apiKey)
	body := drainBody(t, r)
	if r.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", r.StatusCode, body)
	}
	if v := r.Header.Get("RateLimit-Limit"); v == "" {
		t.Errorf("RateLimit-Limit header absent on 200 response")
	} else if got, want := v, strconv.Itoa(rlHdrLimit); got != want {
		t.Errorf("RateLimit-Limit: got %q, want %q", got, want)
	}
	if v := r.Header.Get("RateLimit-Remaining"); v == "" {
		t.Errorf("RateLimit-Remaining header absent on 200 response")
	} else if got, want := v, strconv.Itoa(rlHdrLimit-1); got != want {
		t.Errorf("RateLimit-Remaining: got %q, want %q (limit minus 1 consumed)", got, want)
	}
}

// TestSecurityHeadersPresent: the SecurityHeaders middleware injects OWASP-recommended
// headers on every successful response.
func TestSecurityHeadersPresent(t *testing.T) {
	t.Parallel()
	client := newTestHTTPClient(t)
	ts := harness.NewGatewayTestServer(t, baseOpts(t, echoWorker(1)))
	ts.CreatePlan(t, "sec-hdr-plan", defaultTestRatePerMin, defaultTestMonthlyCap)
	_, apiKey := ts.CreateCustomer(t, "sec-hdr-"+uuid.New().String()+"@example.com", "sec-hdr-plan")

	r := invoke(t, client, ts, apiKey)
	body := drainBody(t, r)
	if r.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", r.StatusCode, body)
	}
	if got := r.Header.Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options: got %q, want nosniff", got)
	}
	if got := r.Header.Get("X-Frame-Options"); got != "DENY" {
		t.Errorf("X-Frame-Options: got %q, want DENY", got)
	}
	if got := r.Header.Get("X-XSS-Protection"); got != "0" {
		t.Errorf("X-XSS-Protection: got %q, want 0", got)
	}
	// SecurityHeaders sets HSTS unconditionally (not gated on r.TLS), so it is
	// present even on the HTTP-only httptest.Server used by this test.
	if got, want := r.Header.Get("Strict-Transport-Security"), "max-age=63072000; includeSubDomains"; got != want {
		t.Errorf("Strict-Transport-Security: got %q, want %q", got, want)
	}
	if got := r.Header.Get("Referrer-Policy"); got != "strict-origin-when-cross-origin" {
		t.Errorf("Referrer-Policy: got %q, want strict-origin-when-cross-origin", got)
	}
	if got := r.Header.Get("Permissions-Policy"); got == "" {
		t.Errorf("Permissions-Policy: header absent")
	}
}

// TestCrossCustomerIsolation: requests from A never appear in B's rows, and vice versa.
func TestCrossCustomerIsolation(t *testing.T) {
	t.Parallel()
	client := newTestHTTPClient(t)
	worker, invocations := countingWorker(1)
	ts := harness.NewGatewayTestServer(t, baseOpts(t, worker))
	ts.CreatePlan(t, "iso-plan", defaultTestRatePerMin, defaultTestMonthlyCap)

	custA, keyA := ts.CreateCustomer(t, "isolation-A-"+uuid.New().String()+"@example.com", "iso-plan")
	custB, keyB := ts.CreateCustomer(t, "isolation-B-"+uuid.New().String()+"@example.com", "iso-plan")

	rA := invoke(t, client, ts, keyA)
	drainBody(t, rA)
	if rA.StatusCode != http.StatusOK {
		t.Fatalf("customer A: want 200, got %d", rA.StatusCode)
	}

	if n := ts.CountUsageEvents(t, custA); n != 1 {
		t.Errorf("after A's request: customer A usage_events = %d, want 1", n)
	}
	if n := ts.CountErrorEvents(t, custA); n != 0 {
		t.Errorf("after A's request: customer A error_events = %d, want 0", n)
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

