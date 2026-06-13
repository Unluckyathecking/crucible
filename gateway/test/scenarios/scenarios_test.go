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

// testHTTPClient is shared across all scenario tests; http.Client is safe for concurrent use.
// Keep-alives are enabled so parallel tests reuse connections rather than opening new TCP
// connections on every request, which avoids ephemeral port exhaustion under -race.
// httptest.Server uses plain HTTP (no TLS), so HTTP/2 is never negotiated regardless.
var testHTTPClient = &http.Client{
	Timeout: 15 * time.Second,
	Transport: &http.Transport{
		DialContext:         (&net.Dialer{Timeout: 5 * time.Second}).DialContext,
		TLSHandshakeTimeout: 5 * time.Second,
		MaxIdleConns:        10,
		MaxIdleConnsPerHost: 5,
		MaxConnsPerHost:     10,
		IdleConnTimeout:     30 * time.Second,
	},
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

func baseOpts(t *testing.T, worker http.Handler) harness.Options {
	t.Helper()
	return harness.Options{
		WorkerHandler: worker,
		DSN:           postgresDSN(t),
		RedisURL:      redisURL(t),
	}
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

// slowWorker waits for delay before responding — triggers the proxy timeout.
// The handler selects on r.Context().Done() so it releases promptly when cancelled.
func slowWorker(delay time.Duration) (http.Handler, *atomic.Bool) {
	var invoked atomic.Bool
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		invoked.Store(true)
		timer := time.NewTimer(delay)
		defer func() {
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
		}()
		select {
		case <-timer.C:
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"payload":{},"billable_units":1}`)
		case <-r.Context().Done():
			return
		}
	})
	return h, &invoked
}

// invoke sends POST /v1/echo to the gateway. Callers must drain and close via drainBody.
// testHTTPClient.Timeout (15 s) bounds each request; no per-call context needed.
func invoke(t *testing.T, ts *harness.TestServer, apiKey string, mutators ...func(*http.Request)) *http.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(
		context.Background(),
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
	resp, err := testHTTPClient.Do(req)
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

// TestHappyPath: authenticated POST /v1/echo → 200, correct billable_units, one usage row.
func TestHappyPath(t *testing.T) {
	t.Parallel()
	ts := harness.NewGatewayTestServer(t, baseOpts(t, echoWorker(3)))
	ts.CreatePlan(t, "hp-plan", 100, 10000)
	customerID, apiKey := ts.CreateCustomer(t, "happy-path-"+uuid.New().String()+"@example.com", "hp-plan")

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
	if inv.BillableUnits != 3 {
		t.Errorf("billable_units: got %d, want 3", inv.BillableUnits)
	}

	// usage.Recorder.Record is synchronous; the row is committed before the HTTP response.
	if n := ts.CountUsageEvents(t, customerID); n != 1 {
		t.Errorf("usage_events row count: got %d, want 1", n)
	}
}

// TestIdempotentReplay: same Idempotency-Key twice returns cached response; worker invoked once.
func TestIdempotentReplay(t *testing.T) {
	t.Parallel()
	worker, invocations := varyingWorker()
	ts := harness.NewGatewayTestServer(t, baseOpts(t, worker))
	ts.CreatePlan(t, "ir-plan", 100, 10000)
	customerID, apiKey := ts.CreateCustomer(t, "idempotent-replay-"+uuid.New().String()+"@example.com", "ir-plan")

	idempKey := "scenario-idemp-" + uuid.New().String()
	withIdemp := func(r *http.Request) { r.Header.Set("Idempotency-Key", idempKey) }

	r1 := invoke(t, ts, apiKey, withIdemp)
	body1 := drainBody(t, r1)
	if r1.StatusCode != http.StatusOK {
		t.Fatalf("first request: want 200, got %d: %s", r1.StatusCode, body1)
	}
	if v := r1.Header.Get("X-Idempotent-Replayed"); v != "" {
		t.Errorf("first request: X-Idempotent-Replayed: got %q, want absent", v)
	}

	// The idempotency key row must exist after the first request completes.
	// Assertion failure here means the middleware did not call store.Finalize,
	// which would also break the replay test below (r2 would re-invoke the worker).
	if n := ts.CountIdempotencyKeys(t, customerID, idempKey); n != 1 {
		t.Fatalf("idempotency_keys after first request: got %d rows, want 1", n)
	}

	r2 := invoke(t, ts, apiKey, withIdemp)
	body2 := drainBody(t, r2)
	if r2.StatusCode != http.StatusOK {
		t.Fatalf("replay request: want 200, got %d: %s", r2.StatusCode, body2)
	}
	if v := r2.Header.Get("X-Idempotent-Replayed"); v != "true" {
		t.Fatalf("replay request: X-Idempotent-Replayed: got %q, want \"true\"", v)
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
	if n := ts.CountIdempotencyKeys(t, customerID, idempKey); n != 1 {
		t.Fatalf("idempotency_keys after replay request: got %d rows, want 1", n)
	}
}

// TestRateLimit: (limit+1)-th request returns 429 RATE_LIMITED with headers.
// All three requests complete within milliseconds so they reliably land in the same
// 60-second sliding window — no minute-boundary race is possible at this timescale.
func TestRateLimit(t *testing.T) {
	t.Parallel()
	ts := harness.NewGatewayTestServer(t, baseOpts(t, echoWorker(1)))
	ts.CreatePlan(t, "rl-2-plan", 2, 10000)
	_, apiKey := ts.CreateCustomer(t, "rate-limit-"+uuid.New().String()+"@example.com", "rl-2-plan")

	for i := 0; i < 2; i++ {
		r := invoke(t, ts, apiKey)
		b := drainBody(t, r)
		if r.StatusCode != http.StatusOK {
			t.Fatalf("request %d: want 200, got %d: %s", i+1, r.StatusCode, b)
		}
	}

	r := invoke(t, ts, apiKey)
	body := drainBody(t, r)
	if r.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("third request: want 429, got %d: %s", r.StatusCode, body)
	}
	if code := errorCode(t, body); code != "RATE_LIMITED" {
		t.Errorf("error.code: got %q, want RATE_LIMITED", code)
	}
	if ra := r.Header.Get("Retry-After"); ra == "" {
		t.Error("want Retry-After header on 429 RATE_LIMITED response")
	} else {
		n, err := strconv.Atoi(ra)
		if err != nil {
			t.Errorf("Retry-After: got %q, want integer seconds", ra)
		} else if n < 0 || n > 60 {
			t.Errorf("Retry-After: got %d, want in [0,60]", n)
		}
	}
	if v := r.Header.Get("RateLimit-Limit"); v != "2" {
		t.Errorf("RateLimit-Limit: got %q, want 2", v)
	}
	if v := r.Header.Get("RateLimit-Remaining"); v != "0" {
		t.Errorf("RateLimit-Remaining: got %q, want 0", v)
	}
}

// TestQuotaExceeded: second request exceeds cap of 1; returns 429 QUOTA_EXCEEDED; no additional usage row written.
func TestQuotaExceeded(t *testing.T) {
	t.Parallel()
	ts := harness.NewGatewayTestServer(t, baseOpts(t, echoWorker(1)))
	ts.CreatePlan(t, "quota-1-plan", 100, 1)
	customerID, apiKey := ts.CreateCustomer(t, "quota-exceeded-"+uuid.New().String()+"@example.com", "quota-1-plan")

	r1 := invoke(t, ts, apiKey)
	body1 := drainBody(t, r1)
	if r1.StatusCode != http.StatusOK {
		t.Fatalf("first request: want 200, got %d: %s", r1.StatusCode, body1)
	}
	if n := ts.CountUsageEvents(t, customerID); n != 1 {
		t.Errorf("after first request: usage_events count = %d, want 1", n)
	}

	r2 := invoke(t, ts, apiKey)
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
	worker, invoked := slowWorker(3500 * time.Millisecond)
	ts := harness.NewGatewayTestServer(t, harness.Options{
		WorkerHandler:   worker,
		DSN:             postgresDSN(t),
		RedisURL:        redisURL(t),
		WorkerTimeoutMS: 500, // 7:1 ratio; reliable on loaded CI runners under -race
	})
	ts.CreatePlan(t, "timeout-plan", 100, 10000)
	customerID, apiKey := ts.CreateCustomer(t, "worker-timeout-"+uuid.New().String()+"@example.com", "timeout-plan")

	r := invoke(t, ts, apiKey)
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
	if !invoked.Load() {
		t.Error("worker was never invoked; proxy may have short-circuited before forwarding")
	}
}

// TestCrossCustomerIsolation: requests from A never appear in B's rows, and vice versa.
func TestCrossCustomerIsolation(t *testing.T) {
	t.Parallel()
	worker, invocations := countingWorker(1)
	ts := harness.NewGatewayTestServer(t, baseOpts(t, worker))
	ts.CreatePlan(t, "iso-plan", 100, 10000)

	custA, keyA := ts.CreateCustomer(t, "isolation-A-"+uuid.New().String()+"@example.com", "iso-plan")
	custB, keyB := ts.CreateCustomer(t, "isolation-B-"+uuid.New().String()+"@example.com", "iso-plan")

	rA := invoke(t, ts, keyA)
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

	rB := invoke(t, ts, keyB)
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
