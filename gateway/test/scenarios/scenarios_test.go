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

// slowWorker sleeps for delay before responding. The proxy times out before
// delay elapses, so the 200 response is never received by the caller; the
// gateway proxy is responsible for returning 502 BadGateway.
// Selecting on r.Context().Done() lets the goroutine exit promptly when the
// gateway closes the connection rather than sleeping for the full delay.
func slowWorker(delay time.Duration) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tmr := time.NewTimer(delay)
		defer tmr.Stop()
		select {
		case <-tmr.C:
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprint(w, `{"payload":{},"billable_units":1}`)
		case <-r.Context().Done():
			// Non-blocking drain: if Stop lost the race with the timer firing,
			// drain the channel so the internal goroutine is not held for the
			// remainder of the delay.
			if !tmr.Stop() {
				select {
				case <-tmr.C:
				default:
				}
			}
			return
		}
	})
}

// waitForErrorEvents polls until want error_events rows exist or the deadline elapses.
// Checks immediately on entry, then at errorPollInterval, so fast paths complete
// without waiting for the first ticker fire.
func waitForErrorEvents(t *testing.T, ts *harness.TestServer, customerID uuid.UUID, want int64) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), errorPollTimeout)
	defer cancel()
	check := func() bool {
		n := ts.CountErrorEvents(t, customerID)
		if n == want {
			return true
		}
		if n > want {
			t.Fatalf("too many error_events for customer %s: got %d, want %d", customerID, n, want)
		}
		return false
	}
	if check() {
		return
	}
	ticker := time.NewTicker(errorPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			t.Fatalf("timeout waiting for %d error_events for customer %s", want, customerID)
		case <-ticker.C:
			if check() {
				return
			}
		}
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
	// echoWorker returns {} for its payload; verify the proxy forwards it unmodified.
	if got := string(inv.Payload); got != "{}" {
		t.Errorf("payload: got %s, want {}", got)
	}
	if n := ts.CountUsageEvents(t, customerID); n != 1 {
		t.Errorf("usage_events: got %d, want 1", n)
	}

	// X-Request-ID must be a valid UUID on every response and unique across requests.
	rid1 := resp.Header.Get("X-Request-ID")
	if rid1 == "" {
		t.Errorf("X-Request-ID absent")
	} else if _, err := uuid.Parse(rid1); err != nil {
		t.Errorf("X-Request-ID %q is not a valid UUID: %v", rid1, err)
	}

	// Security headers set by the SecurityHeaders middleware; check against resp (first response).
	if got := resp.Header.Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options: got %q, want nosniff", got)
	}
	if got := resp.Header.Get("X-Frame-Options"); got != "DENY" {
		t.Errorf("X-Frame-Options: got %q, want DENY", got)
	}
	const wantPermissionsPolicy = "camera=(), microphone=(), geolocation=(), interest-cohort=()"
	if got := resp.Header.Get("Permissions-Policy"); got != wantPermissionsPolicy {
		t.Errorf("Permissions-Policy: got %q, want %q", got, wantPermissionsPolicy)
	}

	resp2 := invoke(t, client, ts, apiKey)
	body2 := drainBody(t, resp2)
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("second request: want 200, got %d: %s", resp2.StatusCode, body2)
	}
	if v := resp2.Header.Get("X-Idempotent-Replayed"); v != "" {
		t.Errorf("second request: X-Idempotent-Replayed: got %q, want absent", v)
	}
	if rid2 := resp2.Header.Get("X-Request-ID"); rid2 == rid1 {
		t.Errorf("X-Request-ID not unique across requests: both got %q", rid1)
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

	// Guard against straddling a rate-limit window boundary (1-minute fixed windows).
	// If we're within 2 s of the next minute, sleep until we're safely into the new
	// minute (plus 100 ms buffer). Uses sub-second precision to avoid under-sleeping.
	if now := time.Now(); now.Second() >= 58 {
		nextMinute := now.Truncate(time.Minute).Add(time.Minute)
		time.Sleep(time.Until(nextMinute) + 100*time.Millisecond)
	}
	// Snapshot the window start once; RateLimit-Reset must fall in [windowStart, windowStart+60].
	// Capturing before requests avoids a spurious failure if time advances past windowStart+60
	// between the request and the assertion.
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
		if resetTS < windowStart || resetTS > windowStart+60 {
			t.Errorf("RateLimit-Reset: got %d, want in [%d, %d] (current window boundary)", resetTS, windowStart, windowStart+60)
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

// TestWorkerTimeout: worker that sleeps past proxy deadline returns 502 WORKER_UNREACHABLE.
func TestWorkerTimeout(t *testing.T) {
	t.Parallel()
	// 5 s delay vs 500 ms proxy timeout: 10× ratio, reliable under -race with CPU contention.
	// The proxy timeout (WorkerTimeoutMS=500) is the bottleneck; client sees 502, not client-side expiry.
	client := newTestHTTPClient(t)
	ts := harness.NewGatewayTestServer(t, baseOpts(t, slowWorker(5*time.Second), func(o *harness.Options) {
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
	// varyingWorker embeds an incrementing counter; equal bodies would mean B was served A's cached payload.
	if string(body1) == string(body2) {
		t.Errorf("idempotency isolation failure: customers A and B received identical worker responses\nbody: %s", body1)
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
	r3 := invoke(t, client, ts, keyB, withIdemp)
	body3 := drainBody(t, r3)
	if r3.StatusCode != http.StatusOK {
		t.Fatalf("customer B replay: want 200, got %d: %s", r3.StatusCode, body3)
	}
	if v := r3.Header.Get("X-Idempotent-Replayed"); v != "true" {
		t.Errorf("customer B replay: X-Idempotent-Replayed = %q, want \"true\"", v)
	}
	if string(body2) != string(body3) {
		t.Errorf("customer B replay body mismatch:\n  first:  %s\n  replay: %s", body2, body3)
	}
	if got := invocations.Load(); got != 2 {
		t.Errorf("worker invocations after B replay: got %d, want 2 (replay must not call worker)", got)
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

