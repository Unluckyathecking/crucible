// Package scenarios exercises the full gateway middleware pipeline end-to-end
// using real Postgres and Redis via the harness package.
package scenarios

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
	"github.com/redis/go-redis/v9"

	"github.com/Unluckyathecking/crucible/gateway/internal/auth"
	"github.com/Unluckyathecking/crucible/gateway/internal/testdb"
	"github.com/Unluckyathecking/crucible/gateway/test/harness"
)

const (
	// defaultTestRatePerMin and defaultTestMonthlyCap are used for plans in tests
	// that do not specifically exercise rate-limiting or quota; the values are
	// deliberately high so they cannot interfere with other test assertions.
	defaultTestRatePerMin = 100
	defaultTestMonthlyCap = 10_000

	// HTTP client constants for newTestHTTPClient.
	// testClientTimeout is a generous ceiling that is never the bottleneck;
	// individual requests use testRequestTimeout (10s) or proxy-level timeouts.
	testClientTimeout       = 25 * time.Second
	testDialTimeout         = 5 * time.Second
	testIdleConnTimeout     = 10 * time.Second
	testMaxIdleConns        = 10
	testMaxIdleConnsPerHost = 5
	testMaxConnsPerHost     = 10

	// Request context and polling timeouts.
	testRequestTimeout = 10 * time.Second
	errorPollTimeout   = 5 * time.Second
	errorPollInterval  = 100 * time.Millisecond

	// hungWorkerFallback is the maximum time hungWorker blocks before exiting.
	// It prevents httptest.Server.Close() deadlocks when the request context is
	// not cancelled first (e.g. if the proxy timeout fires after Close starts).
	// hungWorkerFallback must exceed WorkerTimeoutMS (500 ms in TestWorkerTimeout)
	// but need not be large. 5 s gives adequate margin under -race scheduling
	// variance without imposing a long wait when the proxy timeout itself fires.
	hungWorkerFallback = 5 * time.Second
)

// newTestHTTPClient returns an http.Client for a single test (one per test, not per request).
func newTestHTTPClient(t *testing.T) *http.Client {
	t.Helper()
	c := &http.Client{
		Timeout: testClientTimeout,
		Transport: &http.Transport{
			DialContext:           (&net.Dialer{Timeout: testDialTimeout}).DialContext,
			ResponseHeaderTimeout: testRequestTimeout,
			MaxIdleConns:          testMaxIdleConns,
			MaxIdleConnsPerHost:   testMaxIdleConnsPerHost,
			MaxConnsPerHost:       testMaxConnsPerHost,
			IdleConnTimeout:       testIdleConnTimeout,
		},
	}
	t.Cleanup(c.CloseIdleConnections)
	return c
}

// ---- helpers ----------------------------------------------------------------

func postgresDSN(t *testing.T) string {
	t.Helper()
	if v := os.Getenv("POSTGRES_DSN"); v != "" {
		return v
	}
	return testdb.DSN(t)
}

func redisURL(t *testing.T) string {
	t.Helper()
	if v := os.Getenv("REDIS_URL"); v != "" {
		return v
	}
	// Match the gateway/internal convention: fall back to the local default and
	// skip (not fail) when Redis is genuinely unreachable.
	const addr = "localhost:6379"
	c := redis.NewClient(&redis.Options{Addr: addr})
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := c.Ping(ctx).Err(); err != nil {
		t.Skipf("redis unavailable on %s, skipping: %v", addr, err)
	}
	return "redis://" + addr
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

// varyingWorker embeds an incrementing counter in payload.n so each response is unique.
func varyingWorker() (http.Handler, *atomic.Int64) {
	var count atomic.Int64
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := count.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"payload":{"n":%d},"billable_units":1}`, n)
	})
	return h, &count
}

// hungWorker blocks without writing any response, simulating a worker that
// never finishes. The gateway proxy timeout fires, cancelling r.Context();
// context.WithTimeout wraps the request context so the handler also exits
// after hungWorkerFallback if the proxy does not cancel first. Using
// context.WithTimeout avoids the timer-channel drain race: defer cancel()
// stops the underlying timer correctly regardless of which deadline fires.
// WriteHeader(503) is called after ctx.Done() so the handler never returns
// with Go's implicit 200; by this point the gateway has already closed its
// outgoing connection, so the write is a no-op on the gateway side.
func hungWorker() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), hungWorkerFallback)
		defer cancel()
		<-ctx.Done()
		w.WriteHeader(http.StatusServiceUnavailable)
	})
}

// waitForErrorEvents polls until want error_events rows exist or the deadline elapses.
// All logic runs in the calling goroutine; there are no spawned goroutines here.
func waitForErrorEvents(t *testing.T, ts *harness.TestServer, customerID uuid.UUID, want int64) {
	t.Helper()
	deadline := time.Now().Add(errorPollTimeout)
	var last int64
	for {
		last = ts.CountErrorEvents(t, customerID)
		if last == want {
			return
		}
		if last > want {
			t.Fatalf("too many error_events for customer %s: got %d, want %d", customerID, last, want)
		}
		if time.Now().After(deadline) {
			t.Fatalf("timeout waiting for %d error_events for customer %s; last count: %d", want, customerID, last)
		}
		time.Sleep(errorPollInterval)
	}
}

// invoke sends POST /v1/echo and returns the response. The caller MUST call
// drainBody (or otherwise read and close r.Body) before the next request to
// avoid leaking the underlying connection.
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
	req.Header.Set("Accept", "application/json")
	for _, fn := range mutators {
		fn(req)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("send request: %v", err)
	}
	return resp
}

// drainBody reads and closes the response body, returning its bytes.
func drainBody(t *testing.T, r *http.Response) []byte {
	t.Helper()
	defer r.Body.Close()
	b, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatalf("drainBody: read body: %v", err)
	}
	return b
}

// errorCode extracts error.code from an apierror envelope; fatals if absent.
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

// invocationResponse is the JSON shape returned by every worker in this suite.
type invocationResponse struct {
	Payload       json.RawMessage `json:"payload"`
	BillableUnits uint64          `json:"billable_units"`
}

// ---- scenarios --------------------------------------------------------------

// TestHappyPath: authenticated POST → 200, correct payload/billable_units, one usage row,
// X-Request-ID present, and OWASP security headers set by middleware.
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
		t.Errorf("X-Idempotent-Replayed: got %q, want absent", v)
	}

	var inv invocationResponse
	if err := json.Unmarshal(body, &inv); err != nil {
		t.Fatalf("decode response: %v\nbody: %s", err, body)
	}
	if inv.BillableUnits != 3 {
		t.Errorf("billable_units: got %d, want 3", inv.BillableUnits)
	}
	// echoWorker returns {} for its payload; verify the proxy forwards it unmodified.
	if got := string(inv.Payload); got != "{}" {
		t.Errorf("payload: got %s, want {}", got)
	}
	if n := ts.CountUsageEvents(t, customerID); n != 1 {
		t.Errorf("usage_events: got %d, want 1", n)
	}

	// X-Request-ID must be a valid UUID on every response and unique across requests.
	// Use Fatalf so a missing or malformed rid1 stops the test before the rid2==rid1
	// uniqueness check, which would silently pass if both headers were empty.
	rid1 := resp.Header.Get("X-Request-ID")
	if rid1 == "" {
		t.Fatalf("X-Request-ID absent on first response")
	}
	if _, err := uuid.Parse(rid1); err != nil {
		t.Fatalf("X-Request-ID %q is not a valid UUID: %v", rid1, err)
	}

	// Security headers set by the SecurityHeaders middleware; verify presence only so
	// the test remains valid if specific values evolve (e.g. Permissions-Policy directives).
	if got := resp.Header.Get("X-Content-Type-Options"); got == "" {
		t.Errorf("X-Content-Type-Options header missing")
	}
	if got := resp.Header.Get("X-Frame-Options"); got == "" {
		t.Errorf("X-Frame-Options header missing")
	}
	if got := resp.Header.Get("Permissions-Policy"); got == "" {
		t.Errorf("Permissions-Policy header missing")
	}

	resp2 := invoke(t, client, ts, apiKey)
	body2 := drainBody(t, resp2)
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("second request: want 200, got %d: %s", resp2.StatusCode, body2)
	}
	if v := resp2.Header.Get("X-Idempotent-Replayed"); v != "" {
		t.Errorf("second request: X-Idempotent-Replayed: got %q, want absent", v)
	}
	rid2 := resp2.Header.Get("X-Request-ID")
	if rid2 == "" {
		t.Fatalf("X-Request-ID absent on second response")
	}
	if _, err := uuid.Parse(rid2); err != nil {
		t.Fatalf("second response X-Request-ID %q is not a valid UUID: %v", rid2, err)
	}
	if rid2 == rid1 {
		t.Errorf("X-Request-ID not unique across requests: both got %q", rid1)
	}
	var inv2 invocationResponse
	if err := json.Unmarshal(body2, &inv2); err != nil {
		t.Fatalf("second request: decode response: %v\nbody: %s", err, body2)
	}
	if inv2.BillableUnits != 3 {
		t.Errorf("second request: billable_units: got %d, want 3", inv2.BillableUnits)
	}
	if got := string(inv2.Payload); got != "{}" {
		t.Errorf("second request: payload: got %s, want {}", got)
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
	if r1.StatusCode != http.StatusOK {
		t.Fatalf("first request: want 200, got %d: %s", r1.StatusCode, drainBody(t, r1))
	}
	body1 := drainBody(t, r1)

	if !ts.HasIdempotencyKey(t, customerID, idempKey) {
		t.Fatalf("idempotency_keys: row not found for key %q after first request", idempKey)
	}

	r2 := invoke(t, client, ts, apiKey, withIdemp)
	if v := r2.Header.Get("X-Idempotent-Replayed"); v != "true" {
		t.Errorf("replay request: X-Idempotent-Replayed: got %q, want \"true\"", v)
	}
	if r2.StatusCode != http.StatusOK {
		t.Fatalf("replay request: want 200, got %d: %s", r2.StatusCode, drainBody(t, r2))
	}
	body2 := drainBody(t, r2)

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
	if got, want := replayed.Payload.N, int64(1); got != want {
		t.Errorf("replayed payload.n: got %d, want %d", got, want)
	}
	if got := invocations.Load(); got != 1 {
		t.Errorf("worker invocations: got %d, want 1", got)
	}
	if !ts.HasIdempotencyKey(t, customerID, idempKey) {
		t.Fatalf("idempotency_keys: row not found for key %q after replay request", idempKey)
	}
	if n := ts.CountUsageEvents(t, customerID); n != 1 {
		t.Errorf("usage_events after idempotent replay: got %d, want 1 (replay must not bill again)", n)
	}
}

// TestRateLimit: (limit+1)-th request returns 429 RATE_LIMITED with rate-limit headers.
// All requests land in the same 60-second window so the overflow is reliably rejected.
func TestRateLimit(t *testing.T) {
	t.Parallel()
	client := newTestHTTPClient(t)
	const rateLimit = 2
	ts := harness.NewGatewayTestServer(t, baseOpts(t, echoWorker(1)))
	ts.CreatePlan(t, "rl-2-plan", rateLimit, defaultTestMonthlyCap)
	customerID, apiKey := ts.CreateCustomer(t, "rate-limit-"+uuid.New().String()+"@example.com", "rl-2-plan")

	// Guard against the 60-second window resetting mid-test: if the first request
	// lands near the end of a window and the overflow request arrives in the next
	// window, the counter resets and the overflow request succeeds unexpectedly.
	// Require at least 15 s remaining in the window to ensure all three requests
	// complete in the same 60-second span, even under -race slowdown.
	// Unix-modulo is used (not time.Now().Second()) so the boundary aligns with
	// the rate limiter's Unix-second window, not the local clock minute.
	const (
		maxSyncAttempts    = 3
		windowSafetyMargin = 45 // sleep if >= 45 s into the 60-second window
	)
	for attempt := 1; attempt <= maxSyncAttempts; attempt++ {
		now := time.Now().Unix()
		if int(now%60) < windowSafetyMargin {
			break
		} else if attempt == maxSyncAttempts {
			t.Fatalf("could not align to rate-limit window after %d attempts", maxSyncAttempts)
		} else {
			next := (now/60 + 1) * 60
			time.Sleep(time.Until(time.Unix(next, 0)) + 200*time.Millisecond)
		}
	}
	// Capture windowStart immediately before the first request so the elapsed time
	// between the reference timestamp and the rejected request is minimised.
	// The rate limiter uses a 60-second sliding window (resetAt = time.Now().Add(time.Minute)),
	// so RateLimit-Reset ≈ windowStart+60; the +62 s upper bound absorbs test overhead.
	windowStart := time.Now().Unix()

	for i := 0; i < rateLimit; i++ {
		r := invoke(t, client, ts, apiKey)
		b := drainBody(t, r)
		if r.StatusCode != http.StatusOK {
			t.Fatalf("request %d: want 200, got %d: %s", i+1, r.StatusCode, b)
		}
	}

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
		// Retry-After must be ≥1: a value of 0 would imply the window already reset,
		// inconsistent with the 429 we just received.
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
	if v := r.Header.Get("RateLimit-Reset"); v == "" {
		t.Errorf("RateLimit-Reset: missing, want Unix timestamp")
	} else if resetTS, err := strconv.ParseInt(v, 10, 64); err != nil {
		t.Errorf("RateLimit-Reset: got %q, want Unix timestamp: %v", v, err)
	} else {
		// The sliding window sets resetAt = time.Now().Add(time.Minute) at the
		// moment of rejection. Between the alignment checkpoint and the rejected
		// request, up to two allowed requests execute plus scheduling overhead,
		// so allow 2 s of slack on the upper bound.
		if resetTS < windowStart+58 || resetTS > windowStart+62 {
			t.Errorf("RateLimit-Reset: got %d, want in [%d, %d] (±2s for test overhead)", resetTS, windowStart+58, windowStart+62)
		}
	}
	// Only the rateLimit accepted requests must have been billed; the rejected request must not.
	if n := ts.CountUsageEvents(t, customerID); n != int64(rateLimit) {
		t.Errorf("usage_events after rate limit: got %d, want %d (rejected request must not bill)", n, rateLimit)
	}
}

// TestQuotaExceeded: second request exceeds monthly cap of 1 billable unit; returns 429 QUOTA_EXCEEDED.
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

// TestWorkerTimeout: hungWorker blocks indefinitely; gateway proxy timeout fires and returns 502 WORKER_UNREACHABLE.
func TestWorkerTimeout(t *testing.T) {
	t.Parallel()
	// WorkerTimeoutMS=500 is the bottleneck; hungWorker never responds so the proxy
	// cancels r.Context() and the client sees 502 well within any reasonable deadline.
	client := newTestHTTPClient(t)
	ts := harness.NewGatewayTestServer(t, baseOpts(t, hungWorker(), func(o *harness.Options) {
		o.WorkerTimeoutMS = 500
	}))
	ts.CreatePlan(t, "timeout-plan", defaultTestRatePerMin, defaultTestMonthlyCap)
	customerID, apiKey := ts.CreateCustomer(t, "worker-timeout-"+uuid.New().String()+"@example.com", "timeout-plan")

	r := invoke(t, client, ts, apiKey)
	body := drainBody(t, r)

	if r.StatusCode != http.StatusBadGateway {
		t.Fatalf("proxy timeout: want %d, got %d: %s", http.StatusBadGateway, r.StatusCode, body)
	}
	if code := errorCode(t, body); code != "WORKER_UNREACHABLE" {
		t.Errorf("error.code: got %q, want WORKER_UNREACHABLE", code)
	}
	if n := ts.CountUsageEvents(t, customerID); n != 0 {
		t.Errorf("usage_events after timeout: got %d rows, want 0", n)
	}
	// waitForErrorEvents success proves the proxy forwarded the request (error_events are
	// only inserted on proxy-forwarded failures, not on pre-forward rejections).
	waitForErrorEvents(t, ts, customerID, 1)
}

// TestAuthFailure: a key not registered in the database returns 401 UNAUTHORIZED.
// No plan or customer is created intentionally: the gateway auth middleware rejects
// unknown keys before any plan/customer lookup, so the rejection is independent of
// database state beyond the api_keys table being empty for this key.
// countingWorker is used instead of echoWorker so we can assert the proxy layer was
// never reached — auth must reject before the request is forwarded to the worker.
func TestAuthFailure(t *testing.T) {
	t.Parallel()
	client := newTestHTTPClient(t)
	worker, invocations := countingWorker(1)
	ts := harness.NewGatewayTestServer(t, baseOpts(t, worker))

	// Generate a structurally valid key that is guaranteed absent from the database:
	// auth.Generate produces the canonical format the middleware expects, but we
	// never call CreateCustomer so no matching hash row exists. The gateway looks
	// up by prefix, finds no row, and returns 401 — exercising the DB-absent path.
	absentKey, _, authErr := auth.Generate(harness.TestAPIKeyPrefix)
	if authErr != nil {
		t.Fatalf("generate absent key: %v", authErr)
	}
	r := invoke(t, client, ts, absentKey)
	body := drainBody(t, r)
	if r.StatusCode != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d: %s", r.StatusCode, body)
	}
	if code := errorCode(t, body); code != "UNAUTHORIZED" {
		t.Errorf("error.code: got %q, want UNAUTHORIZED", code)
	}
	if got := invocations.Load(); got != 0 {
		t.Errorf("worker invocations: got %d, want 0 (auth must reject before proxy)", got)
	}
}

// TestWorkerBadResponse: worker with billable_units=0 gets 502 WORKER_BAD_RESPONSE.
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
	body1 := drainBody(t, r1)
	if r1.StatusCode != http.StatusOK {
		t.Fatalf("customer A first request: want 200, got %d: %s", r1.StatusCode, body1)
	}
	if v := r1.Header.Get("X-Idempotent-Replayed"); v != "" {
		t.Errorf("customer A first request: X-Idempotent-Replayed = %q, want absent", v)
	}

	r2 := invoke(t, client, ts, keyB, withIdemp)
	body2 := drainBody(t, r2)
	if r2.StatusCode != http.StatusOK {
		t.Fatalf("customer B first request: want 200, got %d: %s", r2.StatusCode, body2)
	}
	if v := r2.Header.Get("X-Idempotent-Replayed"); v != "" {
		t.Errorf("customer B first request: X-Idempotent-Replayed = %q, want absent (different customer)", v)
	}
	// varyingWorker embeds an incrementing counter; body1 (A) and body2 (B) must differ
	// because each customer triggers an independent worker invocation.
	// Equal bodies would mean B was incorrectly served from A's idempotency cache.
	if string(body1) == string(body2) {
		t.Errorf("idempotency isolation failure: customers A and B received identical worker responses\n  A: %s\n  B: %s", body1, body2)
	}
	if got := invocations.Load(); got != 2 {
		t.Errorf("worker invocations: got %d, want 2 (one per customer)", got)
	}

	// A replays with the same key: must be served from A's cache, not forwarded to the worker.
	r1Replay := invoke(t, client, ts, keyA, withIdemp)
	body1Replay := drainBody(t, r1Replay)
	if r1Replay.StatusCode != http.StatusOK {
		t.Fatalf("customer A replay: want 200, got %d: %s", r1Replay.StatusCode, body1Replay)
	}
	if v := r1Replay.Header.Get("X-Idempotent-Replayed"); v != "true" {
		t.Errorf("customer A replay: X-Idempotent-Replayed = %q, want \"true\"", v)
	}
	if string(body1) != string(body1Replay) {
		t.Errorf("customer A replay body mismatch:\n  first:  %s\n  replay: %s", body1, body1Replay)
	}
	if got := invocations.Load(); got != 2 {
		t.Errorf("worker invocations after A replay: got %d, want 2 (replay must not call worker)", got)
	}

	// B replays with the same key: must be served from B's cache, not forwarded.
	r2Replay := invoke(t, client, ts, keyB, withIdemp)
	body2Replay := drainBody(t, r2Replay)
	if r2Replay.StatusCode != http.StatusOK {
		t.Fatalf("customer B replay: want 200, got %d: %s", r2Replay.StatusCode, body2Replay)
	}
	if v := r2Replay.Header.Get("X-Idempotent-Replayed"); v != "true" {
		t.Errorf("customer B replay: X-Idempotent-Replayed = %q, want \"true\"", v)
	}
	if string(body2) != string(body2Replay) {
		t.Errorf("customer B replay body mismatch:\n  first:  %s\n  replay: %s", body2, body2Replay)
	}
	if got := invocations.Load(); got != 2 {
		t.Errorf("worker invocations after B replay: got %d, want 2 (replay must not call worker)", got)
	}
}

// TestWebhookDeliveriesIDORIsolation verifies that GET /v1/webhooks/deliveries is
// tenant-scoped: customer A sees only their own delivery rows and never B's
// event_id, endpoint URL, or response code. The handler's sole isolation guard
// is WHERE we.customer_id = $1 in the deliveries JOIN; this test catches a
// regression if that clause is removed or mis-parameterised.
func TestWebhookDeliveriesIDORIsolation(t *testing.T) {
	t.Parallel()
	client := newTestHTTPClient(t)
	ts := harness.NewGatewayTestServer(t, baseOpts(t, echoWorker(1)))
	ts.CreatePlan(t, "wd-idor-plan", defaultTestRatePerMin, defaultTestMonthlyCap)

	customerIDA, keyA := ts.CreateCustomer(t, "wd-idor-A-"+uuid.New().String()+"@example.com", "wd-idor-plan")
	customerIDB, _ := ts.CreateCustomer(t, "wd-idor-B-"+uuid.New().String()+"@example.com", "wd-idor-plan")

	endpointIDA := uuid.New()
	endpointIDB := uuid.New()
	eventIDA := "evt_A_" + uuid.New().String()
	eventIDB := "evt_B_" + uuid.New().String()
	urlA := "https://a.example.com/webhook"
	urlB := "https://b.example.com/webhook"

	seedCtx, seedCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer seedCancel()

	if _, err := ts.DB.Exec(seedCtx,
		`INSERT INTO webhook_endpoints (id, customer_id, url, secret, active) VALUES ($1, $2, $3, $4, true)`,
		endpointIDA, customerIDA, urlA, []byte("secret-a"),
	); err != nil {
		t.Fatalf("seed endpoint A: %v", err)
	}
	if _, err := ts.DB.Exec(seedCtx,
		`INSERT INTO webhook_endpoints (id, customer_id, url, secret, active) VALUES ($1, $2, $3, $4, true)`,
		endpointIDB, customerIDB, urlB, []byte("secret-b"),
	); err != nil {
		t.Fatalf("seed endpoint B: %v", err)
	}
	if _, err := ts.DB.Exec(seedCtx,
		`INSERT INTO webhook_deliveries (event_id, endpoint_id, payload, status, attempts, last_response_code) VALUES ($1, $2, '{}', 'delivered', 1, 200)`,
		eventIDA, endpointIDA,
	); err != nil {
		t.Fatalf("seed delivery A: %v", err)
	}
	if _, err := ts.DB.Exec(seedCtx,
		`INSERT INTO webhook_deliveries (event_id, endpoint_id, payload, status, attempts, last_response_code) VALUES ($1, $2, '{}', 'delivered', 1, 201)`,
		eventIDB, endpointIDB,
	); err != nil {
		t.Fatalf("seed delivery B: %v", err)
	}

	type deliveryItem struct {
		EventID          string `json:"event_id"`
		EndpointURL      string `json:"endpoint_url"`
		LastResponseCode *int   `json:"last_response_code"`
	}

	// customer A sees only their own rows.
	reqA, err := http.NewRequestWithContext(
		context.Background(), http.MethodGet, ts.Server.URL+"/v1/webhooks/deliveries", nil)
	if err != nil {
		t.Fatalf("build request A: %v", err)
	}
	reqA.Header.Set("Authorization", "Bearer "+keyA)
	respA, err := client.Do(reqA)
	if err != nil {
		t.Fatalf("send request A: %v", err)
	}
	bodyA := drainBody(t, respA)
	if respA.StatusCode != http.StatusOK {
		t.Fatalf("customer A: want 200, got %d: %s", respA.StatusCode, bodyA)
	}
	var pageA struct {
		Items []deliveryItem `json:"items"`
		Total int64          `json:"total"`
	}
	if err := json.Unmarshal(bodyA, &pageA); err != nil {
		t.Fatalf("customer A: decode response: %v\nbody: %s", err, bodyA)
	}
	foundA := false
	for _, d := range pageA.Items {
		if d.EventID == eventIDA {
			foundA = true
		}
		if d.EventID == eventIDB {
			t.Errorf("IDOR: customer A sees customer B event_id %q", eventIDB)
		}
		if d.EndpointURL == urlB {
			t.Errorf("IDOR: customer A sees customer B endpoint_url %q", urlB)
		}
		if d.LastResponseCode != nil && *d.LastResponseCode == 201 {
			t.Errorf("IDOR: customer A sees customer B last_response_code 201")
		}
	}
	if !foundA {
		t.Errorf("customer A: own event_id %q not found in response", eventIDA)
	}

	// no Authorization header → 401.
	reqNoAuth, err := http.NewRequestWithContext(
		context.Background(), http.MethodGet, ts.Server.URL+"/v1/webhooks/deliveries", nil)
	if err != nil {
		t.Fatalf("build no-auth request: %v", err)
	}
	respNoAuth, err := client.Do(reqNoAuth)
	if err != nil {
		t.Fatalf("send no-auth request: %v", err)
	}
	bodyNoAuth := drainBody(t, respNoAuth)
	if respNoAuth.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no auth: want 401, got %d: %s", respNoAuth.StatusCode, bodyNoAuth)
	}

	// structurally valid but unregistered key → 401.
	absentKey, _, genErr := auth.Generate(harness.TestAPIKeyPrefix)
	if genErr != nil {
		t.Fatalf("generate absent key: %v", genErr)
	}
	reqInvalid, err := http.NewRequestWithContext(
		context.Background(), http.MethodGet, ts.Server.URL+"/v1/webhooks/deliveries", nil)
	if err != nil {
		t.Fatalf("build invalid-key request: %v", err)
	}
	reqInvalid.Header.Set("Authorization", "Bearer "+absentKey)
	respInvalid, err := client.Do(reqInvalid)
	if err != nil {
		t.Fatalf("send invalid-key request: %v", err)
	}
	bodyInvalid := drainBody(t, respInvalid)
	if respInvalid.StatusCode != http.StatusUnauthorized {
		t.Fatalf("invalid key: want 401, got %d: %s", respInvalid.StatusCode, bodyInvalid)
	}
	if code := errorCode(t, bodyInvalid); code != "UNAUTHORIZED" {
		t.Errorf("invalid key: error.code: got %q, want UNAUTHORIZED", code)
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

