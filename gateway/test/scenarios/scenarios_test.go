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
		t.Skip("POSTGRES_DSN not set; skipping integration test")
	}
	return v
}

func redisURL(t *testing.T) string {
	t.Helper()
	v := os.Getenv("REDIS_URL")
	if v == "" {
		t.Skip("REDIS_URL not set; skipping integration test")
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
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"payload":{},"billable_units":1}`)
		case <-r.Context().Done():
			// Return without writing to w. The HTTP server closes the connection
			// with no response, which the gateway proxy maps to 502 WORKER_UNREACHABLE.
			if !timer.Stop() {
				<-timer.C
			}
			return
		}
	})
	return h, &invoked
}

// waitForErrorEvents polls CountErrorEvents until want rows exist or a 5-second
// deadline elapses. The error recorder writes asynchronously, so callers must
// wait rather than asserting immediately after the triggering request returns.
// Must be called from the main test goroutine (uses t.Fatalf).
func waitForErrorEvents(t *testing.T, ts *harness.TestServer, customerID uuid.UUID, want int64) {
	t.Helper()
	// Check immediately before starting the ticker to return in the common case
	// where the async errorlog has already written the event.
	if n := ts.CountErrorEvents(t, customerID); n == want {
		return
	} else if n > want {
		t.Fatalf("too many error_events for customer %s: got %d, want %d", customerID, n, want)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			t.Fatalf("timeout waiting for %d error_events for customer %s", want, customerID)
		case <-ticker.C:
			n := ts.CountErrorEvents(t, customerID)
			if n == want {
				return
			}
			if n > want {
				t.Fatalf("too many error_events for customer %s: got %d, want %d", customerID, n, want)
			}
		}
	}
}

// invoke sends POST /v1/echo to the gateway. client must be created once per test
// via newTestHTTPClient() and reused across calls. Callers must drain and close the
// response body via drainBody.
func invoke(t *testing.T, client *http.Client, ts *harness.TestServer, apiKey string, mutators ...func(*http.Request)) *http.Response {
	t.Helper()
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
			Code string `json:"code"`
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
	body1 := drainBody(t, r1)
	if r1.StatusCode != http.StatusOK {
		t.Fatalf("first request: want 200, got %d: %s", r1.StatusCode, body1)
	}
	if v := r1.Header.Get("X-Idempotent-Replayed"); v != "" {
		t.Errorf("first request: X-Idempotent-Replayed: got %q, want absent", v)
	}

	// Race guard: CountIdempotencyKey verifies the row is committed before r2 fires.
	// The idempotency middleware writes the row synchronously before sending the
	// HTTP response, so drainBody guarantees commit. Asserting here ensures the
	// second request (r2) always sees the row and returns a replay, not a fresh call.
	if !ts.HasIdempotencyKey(t, customerID, idempKey) {
		t.Fatalf("idempotency_keys: row not found for key %q after first request", idempKey)
	}

	r2 := invoke(t, client, ts, apiKey, withIdemp)
	body2 := drainBody(t, r2)
	if r2.StatusCode != http.StatusOK {
		t.Fatalf("replay request: want 200, got %d: %s", r2.StatusCode, body2)
	}
	if v := r2.Header.Get("X-Idempotent-Replayed"); !strings.EqualFold(v, "true") {
		t.Errorf("replay request: X-Idempotent-Replayed: got %q, want \"true\" (case-insensitive)", v)
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
	if n < 0 || n > 60 {
		t.Errorf("Retry-After: got %d, want in [0,60]", n)
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
	} else if resetUnix < reqTime || resetUnix > reqTime+61 {
		t.Errorf("RateLimit-Reset: got %d, want Unix timestamp within 61s of request time (%d)", resetUnix, reqTime)
	}
	// Only the rateLimit accepted requests must have been billed; the rejected request must not.
	if n := ts.CountUsageEvents(t, customerID); n != int64(rateLimit) {
		t.Errorf("usage_events after rate limit: got %d, want %d (rejected request must not bill)", n, rateLimit)
	}
}

// TestQuotaExceeded: second request exceeds monthly cap of 1 billable unit; returns 429 QUOTA_EXCEEDED.
// The quota middleware calls Allow() which increments the Redis counter (0→1) and admits the first
// request since 1 ≤ cap(1). The usage recorder writes a usage_events row but does not touch the
// quota counter. The second request: Allow() increments (1→2), which exceeds cap(1), so the request
// is denied immediately with no usage_events row written.
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
	// 500 ms worker delay vs 100 ms proxy timeout: large enough ratio to ensure the
	// proxy forwards the request before timing out, testing the "forwarded then timed
	// out" path; small enough to keep the test fast under CI load.
	client := newTestHTTPClient(t)
	worker, invoked := slowWorker(500 * time.Millisecond)
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
