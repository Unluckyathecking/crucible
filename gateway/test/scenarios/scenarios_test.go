// Package scenarios_test exercises the full gateway middleware pipeline end-to-end
// using real Postgres and Redis via the harness package.
//
// Each test creates isolated customers (unique UUIDs) so tests are independent
// even when run against a shared CI database. Harness t.Cleanup removes all rows
// and Redis keys belonging to test customers after each test.
//
// Environment variables required:
//
//	POSTGRES_DSN  — real Postgres DSN (same one the CI "Apply gateway migrations" step uses)
//	REDIS_URL     — real Redis URL
//
// Tests skip (not fail) when either variable is unset, so they are safe to run locally
// without infra configured and always green in CI where the vars are always set.
package scenarios_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Unluckyathecking/crucible/gateway/test/harness"
)

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

// baseOpts returns a minimal Options for tests that just need the defaults.
func baseOpts(t *testing.T, worker http.Handler) harness.Options {
	t.Helper()
	return harness.Options{
		WorkerHandler: worker,
		DSN:           postgresDSN(t),
		RedisURL:      redisURL(t),
	}
}

// echoWorker returns a handler that responds to POST /invoke with a fixed billable_units payload.
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

// varyingWorker embeds the invocation count in the payload so each response body is
// unique. Used in idempotency tests: if the second response matches the first, it
// proves the middleware returned the cached response, not a coincidental worker match.
func varyingWorker() (http.Handler, *atomic.Int64) {
	var count atomic.Int64
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := count.Add(1)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"payload":{"n":%d},"billable_units":1}`, n)
	})
	return h, &count
}

// slowWorker sleeps for delay before responding — used to trigger the proxy timeout.
// Returns the handler and an atomic flag that is set true when the handler is invoked,
// so callers can verify the proxy reached the worker before timing out.
func slowWorker(delay time.Duration) (http.Handler, *atomic.Bool) {
	var invoked atomic.Bool
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		invoked.Store(true)
		timer := time.NewTimer(delay)
		select {
		case <-timer.C:
			if r.Context().Err() != nil {
				return
			}
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"payload":{},"billable_units":1}`)
		case <-r.Context().Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			return
		}
	})
	return h, &invoked
}

// invoke sends POST /v1/echo to the gateway server with the given API key and optional
// request mutators. Callers must drain and close the body via drainBody.
func invoke(t *testing.T, ts *harness.TestServer, apiKey string, mutators ...func(*http.Request)) *http.Response {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
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
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	return resp
}

// drainBody reads and closes the response body, returning its bytes.
func drainBody(t *testing.T, r *http.Response) []byte {
	t.Helper()
	if r == nil {
		t.Fatal("drainBody: nil response")
	}
	defer r.Body.Close()
	b, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return b
}

// errorCode extracts the top-level error.code from an apierror envelope.
func errorCode(t *testing.T, body []byte) string {
	t.Helper()
	var env struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("decode apierror envelope: %v\nbody: %s", err, body)
	}
	return env.Error.Code
}

// ---- scenarios --------------------------------------------------------------

// TestHappyPath: authenticated POST /v1/echo → 200, response carries the worker's
// billable_units value, and exactly one usage_events row is written for that customer.
func TestHappyPath(t *testing.T) {
	ts := harness.NewGatewayTestServer(t, baseOpts(t, echoWorker(3)))
	ts.CreatePlan(t, "hp-plan", 100, 10000)
	customerID, apiKey := ts.CreateCustomer(t, "happy-path@example.com", "hp-plan")

	resp := invoke(t, ts, apiKey)
	body := drainBody(t, resp)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", resp.StatusCode, body)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type: got %q, want application/json", ct)
	}

	var inv struct {
		Payload       json.RawMessage `json:"payload"`
		BillableUnits uint64          `json:"billable_units"`
	}
	if err := json.Unmarshal(body, &inv); err != nil {
		t.Fatalf("decode response: %v\nbody: %s", err, body)
	}
	if len(inv.Payload) == 0 {
		t.Errorf("payload: missing or empty in 200 response")
	}
	if inv.BillableUnits != 3 {
		t.Errorf("billable_units: got %d, want 3", inv.BillableUnits)
	}

	// usage.Recorder.Record is synchronous within the request handler, so the row
	// is committed before the HTTP response is written.
	if n := ts.CountUsageEvents(t, customerID); n != 1 {
		t.Errorf("usage_events row count: got %d, want 1", n)
	}
}

// TestIdempotentReplay: sending the same Idempotency-Key twice returns the stored
// response on the second call and invokes the worker exactly once. Uses varyingWorker
// so each invocation returns a unique payload; body equality on the second response
// therefore proves the middleware returned the cached copy rather than a coincidentally
// matching worker call.
func TestIdempotentReplay(t *testing.T) {
	worker, invocations := varyingWorker()
	ts := harness.NewGatewayTestServer(t, baseOpts(t, worker))
	ts.CreatePlan(t, "ir-plan", 100, 10000)
	customerID, apiKey := ts.CreateCustomer(t, "idempotent-replay@example.com", "ir-plan")

	idempKey := "scenario-idemp-" + t.Name()
	withIdemp := func(r *http.Request) { r.Header.Set("Idempotency-Key", idempKey) }

	r1 := invoke(t, ts, apiKey, withIdemp)
	body1 := drainBody(t, r1)
	if r1.StatusCode != http.StatusOK {
		t.Fatalf("first request: want 200, got %d: %s", r1.StatusCode, body1)
	}
	// First request is a fresh invocation — must NOT carry the replay header.
	if v := r1.Header.Get("X-Idempotent-Replayed"); v != "" {
		t.Errorf("first request: X-Idempotent-Replayed: got %q, want absent", v)
	}

	// The idempotency middleware writes the record synchronously before sending the
	// response, so by the time drainBody returns the key is committed. Verify this
	// with a direct DB read; if the assertion below fails, the middleware guarantee
	// is broken, not a test timing issue.
	if n := ts.CountIdempotencyKeys(t, customerID, idempKey); n != 1 {
		t.Fatalf("idempotency_keys after first request: got %d rows, want 1 (middleware must write synchronously)", n)
	}

	r2 := invoke(t, ts, apiKey, withIdemp)
	body2 := drainBody(t, r2)
	if r2.StatusCode != http.StatusOK {
		t.Fatalf("replay request: want 200, got %d: %s", r2.StatusCode, body2)
	}

	if string(body1) != string(body2) {
		t.Errorf("replayed body differs:\n  first:  %s\n  second: %s", body1, body2)
	}
	// The replayed body must be the cached first-invocation response.
	// varyingWorker embeds the invocation count; n=1 in the replay proves
	// the cached copy (not a second worker call with n=2) was returned.
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
	// X-Idempotent-Replayed is the middleware's explicit signal that the response
	// came from cache; its absence indicates the middleware did not replay.
	if v := r2.Header.Get("X-Idempotent-Replayed"); v != "true" {
		t.Fatalf("X-Idempotent-Replayed: got %q, want \"true\"", v)
	}
}

// TestRateLimit: the (limit+1)-th request inside the window returns 429 with
// Retry-After and/or RateLimit-* headers and the RATE_LIMITED error code.
// The ratelimit package uses a sliding window (not a fixed-minute bucket), so
// requests fired in rapid succession are all counted in the same 60-second window;
// there is no boundary to straddle, and no sleep is needed to make the test deterministic.
func TestRateLimit(t *testing.T) {
	ts := harness.NewGatewayTestServer(t, baseOpts(t, echoWorker(1)))
	ts.CreatePlan(t, "rl-2-plan", 2, 10000)
	_, apiKey := ts.CreateCustomer(t, "rate-limit@example.com", "rl-2-plan")

	// First two requests must succeed (rate=2/min).
	for i := 0; i < 2; i++ {
		r := invoke(t, ts, apiKey)
		b := drainBody(t, r)
		if r.StatusCode != http.StatusOK {
			t.Fatalf("request %d: want 200, got %d: %s", i+1, r.StatusCode, b)
		}
	}

	// Third request must hit the limit.
	r := invoke(t, ts, apiKey)
	body := drainBody(t, r)
	if r.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("third request: want 429, got %d: %s", r.StatusCode, body)
	}
	if code := errorCode(t, body); code != "RATE_LIMITED" {
		t.Errorf("error.code: got %q, want RATE_LIMITED", code)
	}
	// ratelimit.Middleware sets Retry-After, RateLimit-Limit, and RateLimit-Remaining
	// on every 429 response (confirmed via httputil.SetRateLimitHeaders).
	// ratelimit.Middleware sets Retry-After as integer seconds (RFC 6585 §4).
	if ra := r.Header.Get("Retry-After"); ra == "" {
		t.Error("want Retry-After header on 429 RATE_LIMITED response")
	} else {
		n, err := strconv.Atoi(ra)
		if err != nil {
			t.Errorf("Retry-After: got %q, want integer seconds", ra)
		} else if n < 1 || n > 60 {
			t.Errorf("Retry-After: got %d, want in [1,60]", n)
		}
	}
	if v := r.Header.Get("RateLimit-Limit"); v != "2" {
		t.Errorf("RateLimit-Limit: got %q, want 2", v)
	}
	if v := r.Header.Get("RateLimit-Remaining"); v != "0" {
		t.Errorf("RateLimit-Remaining: got %q, want 0", v)
	}
}

// TestQuotaExceeded: a request that would exceed the seeded monthly cap returns
// 429 QUOTA_EXCEEDED and leaves no usage_events row for the rejected call.
func TestQuotaExceeded(t *testing.T) {
	ts := harness.NewGatewayTestServer(t, baseOpts(t, echoWorker(1)))
	// monthlyCap=1: the first request is admitted and writes one usage_events row.
	// The second request is rejected by the quota middleware (cap exhausted) and
	// produces no usage_events row.
	ts.CreatePlan(t, "quota-1-plan", 100, 1)
	customerID, apiKey := ts.CreateCustomer(t, "quota-exceeded@example.com", "quota-1-plan")

	// First request succeeds and consumes the cap.
	r1 := invoke(t, ts, apiKey)
	body1 := drainBody(t, r1)
	if r1.StatusCode != http.StatusOK {
		t.Fatalf("first request: want 200, got %d: %s", r1.StatusCode, body1)
	}
	if n := ts.CountUsageEvents(t, customerID); n != 1 {
		t.Errorf("after first request: usage_events count = %d, want 1", n)
	}

	// Second request must be denied by the quota middleware.
	r2 := invoke(t, ts, apiKey)
	body2 := drainBody(t, r2)
	if r2.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("second request: want 429, got %d: %s", r2.StatusCode, body2)
	}
	if code := errorCode(t, body2); code != "QUOTA_EXCEEDED" {
		t.Errorf("error.code: got %q, want QUOTA_EXCEEDED", code)
	}

	// No additional usage_events row for the denied request.
	if n := ts.CountUsageEvents(t, customerID); n != 1 {
		t.Errorf("after denied request: usage_events count = %d, want 1 (no row for rejected call)", n)
	}
}

// TestWorkerTimeout: a worker that sleeps past the proxy deadline causes a 502
// BadGateway response in the apierror envelope shape, and no usage_events row is
// recorded. The gateway proxy client returns http.StatusBadGateway (502) on timeout
// via routes.go invoke → apierror.Write(w, rid, http.StatusBadGateway, WORKER_UNREACHABLE, ...).
func TestWorkerTimeout(t *testing.T) {
	worker, invoked := slowWorker(500 * time.Millisecond)
	ts := harness.NewGatewayTestServer(t, harness.Options{
		WorkerHandler:   worker,
		DSN:             postgresDSN(t),
		RedisURL:        redisURL(t),
		WorkerTimeoutMS: 100, // times out well before the 500ms worker sleep
	})
	ts.CreatePlan(t, "timeout-plan", 100, 10000)
	customerID, apiKey := ts.CreateCustomer(t, "worker-timeout@example.com", "timeout-plan")

	r := invoke(t, ts, apiKey)
	body := drainBody(t, r)

	// The proxy client timeout surfaces as 502 BadGateway, not 504.
	// server/routes.go calls apierror.Write(w, rid, http.StatusBadGateway, WORKER_UNREACHABLE, ...)
	// on any proxy error including context deadline exceeded; 502 is confirmed by routes_test.go.
	if r.StatusCode != http.StatusBadGateway {
		t.Fatalf("want 502 BadGateway on proxy timeout, got %d: %s", r.StatusCode, body)
	}

	// The response must be an apierror envelope with WORKER_UNREACHABLE.
	// server/routes.go writes apierror.Write(w, rid, http.StatusBadGateway, WORKER_UNREACHABLE, ...)
	// for any proxy error including context deadline exceeded.
	code := errorCode(t, body)
	if code != "WORKER_UNREACHABLE" {
		t.Errorf("error.code: got %q, want WORKER_UNREACHABLE", code)
	}

	// No usage row: the worker never responded, so Record was never called.
	if n := ts.CountUsageEvents(t, customerID); n != 0 {
		t.Errorf("usage_events after timeout: got %d rows, want 0", n)
	}
	// The proxy must have reached the worker before timing out.
	if !invoked.Load() {
		t.Error("worker was never invoked; proxy may have short-circuited before forwarding")
	}
}

// TestCrossCustomerIsolation: requests from customer A never appear in customer B's
// usage_events or error_events history, and vice versa.
func TestCrossCustomerIsolation(t *testing.T) {
	worker, invocations := countingWorker(1)
	ts := harness.NewGatewayTestServer(t, baseOpts(t, worker))
	ts.CreatePlan(t, "iso-plan", 100, 10000)

	custA, keyA := ts.CreateCustomer(t, "isolation-A@example.com", "iso-plan")
	custB, keyB := ts.CreateCustomer(t, "isolation-B@example.com", "iso-plan")

	// Customer A makes a request.
	rA := invoke(t, ts, keyA)
	drainBody(t, rA)
	if rA.StatusCode != http.StatusOK {
		t.Fatalf("customer A: want 200, got %d", rA.StatusCode)
	}

	// Customer B's usage_events and error_events are unaffected by A's request.
	if n := ts.CountUsageEvents(t, custB); n != 0 {
		t.Errorf("after A's request: customer B usage_events = %d, want 0", n)
	}
	if n := ts.CountErrorEvents(t, custB); n != 0 {
		t.Errorf("after A's request: customer B error_events = %d, want 0", n)
	}

	// Customer B makes a request.
	rB := invoke(t, ts, keyB)
	drainBody(t, rB)
	if rB.StatusCode != http.StatusOK {
		t.Fatalf("customer B: want 200, got %d", rB.StatusCode)
	}

	// Worker must have been invoked exactly once per customer.
	if got := invocations.Load(); got != 2 {
		t.Errorf("worker invocations: got %d, want 2", got)
	}
	// Each customer has exactly one usage row; neither bleeds into the other.
	if n := ts.CountUsageEvents(t, custA); n != 1 {
		t.Errorf("customer A usage_events = %d, want 1", n)
	}
	if n := ts.CountUsageEvents(t, custB); n != 1 {
		t.Errorf("customer B usage_events = %d, want 1", n)
	}
	// Both customers' requests succeeded (200), so no error_events for either.
	if n := ts.CountErrorEvents(t, custA); n != 0 {
		t.Errorf("customer A error_events = %d, want 0", n)
	}
	if n := ts.CountErrorEvents(t, custB); n != 0 {
		t.Errorf("customer B error_events = %d, want 0", n)
	}
}
