package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Unluckyathecking/crucible/gateway/internal/resilience"
)

func TestInvoke_Success(t *testing.T) {
	worker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/invoke" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"payload":{"ok":true},"billable_units":7,"units_label":"pages"}`))
	}))
	defer worker.Close()

	c := New(worker.URL, 5*time.Second, 0)
	resp, err := c.Invoke(context.Background(), &InvokeRequest{
		RequestID: "req_x",
		Operation: "echo",
		Payload:   json.RawMessage(`{"in":1}`),
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if resp.BillableUnits != 7 {
		t.Errorf("billable_units = %d, want 7", resp.BillableUnits)
	}
	if resp.UnitsLabel != "pages" {
		t.Errorf("units_label = %q, want pages", resp.UnitsLabel)
	}
	if resp.Error != nil {
		t.Errorf("unexpected error: %+v", resp.Error)
	}
}

func TestInvoke_WorkerError(t *testing.T) {
	worker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"error":{"code":"BAD_INPUT","message":"bad","retryable":false},"billable_units":1}`))
	}))
	defer worker.Close()

	c := New(worker.URL, 5*time.Second, 0)
	resp, err := c.Invoke(context.Background(), &InvokeRequest{Operation: "x"})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if resp.Error == nil {
		t.Fatal("expected structured error")
	}
	if resp.Error.Code != "BAD_INPUT" {
		t.Errorf("code = %q, want BAD_INPUT", resp.Error.Code)
	}
}

func TestInvoke_Non200(t *testing.T) {
	worker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`worker exploded`))
	}))
	defer worker.Close()

	c := New(worker.URL, 5*time.Second, 0)
	_, err := c.Invoke(context.Background(), &InvokeRequest{Operation: "x"})
	if err == nil {
		t.Fatal("expected error for non-200, got nil")
	}
	// The body should surface in the error message so operators can triage worker failures
	// without having to attach a debugger.
	if !strings.Contains(err.Error(), "worker exploded") {
		t.Errorf("error %q did not include worker response body", err.Error())
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("error %q did not include status code", err.Error())
	}
}

func TestInvoke_Non200_BodyTruncated(t *testing.T) {
	bigBody := strings.Repeat("x", 10000)
	worker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(bigBody))
	}))
	defer worker.Close()

	c := New(worker.URL, 5*time.Second, 0)
	_, err := c.Invoke(context.Background(), &InvokeRequest{Operation: "x"})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	// The body peek caps at 512 bytes so a chatty worker can't blow up log lines.
	if len(err.Error()) > 700 {
		t.Errorf("error too long (%d bytes); body should be truncated to ~512", len(err.Error()))
	}
}

func TestInvoke_MalformedShape(t *testing.T) {
	worker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"billable_units":1}`)) // neither payload nor error
	}))
	defer worker.Close()

	c := New(worker.URL, 5*time.Second, 0)
	_, err := c.Invoke(context.Background(), &InvokeRequest{Operation: "x"})
	if err == nil {
		t.Fatal("expected error for malformed response, got nil")
	}
}

func TestInvoke_Timeout(t *testing.T) {
	worker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(5 * time.Second)
	}))
	defer worker.Close()

	c := New(worker.URL, 50*time.Millisecond, 0)
	_, err := c.Invoke(context.Background(), &InvokeRequest{Operation: "slow"})
	if err == nil {
		t.Fatal("expected timeout error, got nil")
	}
	if !strings.Contains(err.Error(), "worker call") {
		t.Errorf("error %q should wrap the transport error", err.Error())
	}
}

func TestInvoke_FallbackTimeout(t *testing.T) {
	c := New("http://fake", 0, 0)
	if c.http.Timeout != defaultTimeout {
		t.Errorf("timeout = %v, want %v fallback", c.http.Timeout, defaultTimeout)
	}

	cNegative := New("http://fake", -5*time.Second, 0)
	if cNegative.http.Timeout != defaultTimeout {
		t.Errorf("negative timeout = %v, want %v fallback", cNegative.http.Timeout, defaultTimeout)
	}
}

func TestInvoke_ContextDeadlineHonored(t *testing.T) {
	// Handler blocks until request context cancels or a long fallback elapses.
	// The client HTTP timeout (5s) and handler sleep (2s) both outlast the 100ms caller context.
	worker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(2 * time.Second):
		}
	}))
	t.Cleanup(worker.Close)

	c := New(worker.URL, 5*time.Second, 0)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := c.Invoke(ctx, &InvokeRequest{Operation: "slow"})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected error from context deadline, got nil")
	}
	if elapsed > 1*time.Second {
		t.Errorf("Invoke took %v; should have returned promptly on context deadline", elapsed)
	}
	var urlErr *url.Error
	if !errors.Is(err, context.DeadlineExceeded) &&
		!errors.Is(err, context.Canceled) &&
		!(errors.As(err, &urlErr) && urlErr.Timeout()) {
		t.Errorf("expected context deadline/cancellation error, got: %v", err)
	}
}

func TestNew_TransportCeilingAndTimeouts(t *testing.T) {
	// A slow worker must not be able to pin gateway sockets/goroutines without
	// bound: the transport caps connections per host and bounds the header wait.
	c := New("http://worker", 5*time.Second, 32)

	tr, ok := c.http.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport is %T, want *http.Transport", c.http.Transport)
	}
	if tr.MaxConnsPerHost != 32 {
		t.Errorf("MaxConnsPerHost = %d, want 32 from config knob", tr.MaxConnsPerHost)
	}
	// No ResponseHeaderTimeout assertion: a fixed header-wait ceiling would cap
	// legitimate workers (which write the response only after their handler
	// returns) below WORKER_TIMEOUT_MS. Total time is bounded by the per-request
	// context deadline; the real DoS fix is the connection ceiling + connect timeout.
	if tr.DialContext == nil {
		t.Error("DialContext is nil; want an explicit net.Dialer with connect timeout")
	}
}

func TestNew_DefaultMaxConns(t *testing.T) {
	// maxConns <= 0 must fall back to a sane ceiling rather than unlimited (0).
	c := New("http://worker", 5*time.Second, 0)
	tr := c.http.Transport.(*http.Transport)
	if tr.MaxConnsPerHost != defaultMaxConns {
		t.Errorf("MaxConnsPerHost = %d, want default %d", tr.MaxConnsPerHost, defaultMaxConns)
	}
}

func TestInvoke_StalledConnection(t *testing.T) {
	// Start a raw TCP listener that accepts connections but never writes a response.
	// This simulates a worker that hangs after TCP handshake.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen failed: %v", err)
	}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		var held []net.Conn
		defer func() {
			for _, c := range held {
				_ = c.Close()
			}
		}()
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			held = append(held, conn)
		}
	}()
	// Close the listener first (unblocks Accept → goroutine exits), then wait.
	t.Cleanup(func() {
		l.Close()
		wg.Wait()
	})

	workerURL := "http://" + l.Addr().String()
	// Set a very short timeout so the test runs fast.
	c := New(workerURL, 50*time.Millisecond, 0)

	start := time.Now()
	_, err = c.Invoke(context.Background(), &InvokeRequest{Operation: "slow"})
	duration := time.Since(start)

	if err == nil {
		t.Fatal("expected timeout error, got nil")
	}
	if duration > 1*time.Second {
		t.Errorf("timeout took too long: %v", duration)
	}
	if !strings.Contains(err.Error(), "worker call") {
		t.Errorf("error %q should wrap the transport error", err.Error())
	}
}

// TestInvoke_MarshalError exercises the json.Marshal failure path (line 77-79).
// json.RawMessage containing invalid JSON causes Marshal to return an error.
func TestInvoke_MarshalError(t *testing.T) {
	c := New("http://unused", 5*time.Second, 0)
	_, err := c.Invoke(context.Background(), &InvokeRequest{
		Operation: "x",
		Payload:   json.RawMessage(`not-valid-json`),
	})
	if err == nil {
		t.Fatal("expected marshal error, got nil")
	}
	if !strings.Contains(err.Error(), "marshal request") {
		t.Errorf("error %q should wrap as marshal request", err.Error())
	}
}

// TestInvoke_BadWorkerURL exercises the http.NewRequestWithContext failure path (line 82-84).
// A URL with a control character is rejected by net/url at request-build time.
func TestInvoke_BadWorkerURL(t *testing.T) {
	c := New("http://\x00bad", 5*time.Second, 0)
	_, err := c.Invoke(context.Background(), &InvokeRequest{Operation: "x"})
	if err == nil {
		t.Fatal("expected request-build error, got nil")
	}
	if !strings.Contains(err.Error(), "build request") {
		t.Errorf("error %q should wrap as build request", err.Error())
	}
}

// TestInvoke_DecodeError exercises the json.Decode failure path (line 110-112).
// The worker returns HTTP 200 but a body that is not valid JSON.
func TestInvoke_DecodeError(t *testing.T) {
	worker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`this is not json at all`))
	}))
	defer worker.Close()

	c := New(worker.URL, 5*time.Second, 0)
	_, err := c.Invoke(context.Background(), &InvokeRequest{Operation: "x"})
	if err == nil {
		t.Fatal("expected decode error, got nil")
	}
	if !strings.Contains(err.Error(), "decode worker response") {
		t.Errorf("error %q should wrap as decode worker response", err.Error())
	}
}

// TestInvoke_ContextCanceled verifies that an explicit context.Cancel unblocks Invoke promptly.
// Complements TestInvoke_ContextDeadlineHonored which uses a deadline rather than explicit cancel.
func TestInvoke_ContextCanceled(t *testing.T) {
	// Handler blocks until request context is done or a long fallback elapses.
	worker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(2 * time.Second):
		}
	}))
	t.Cleanup(worker.Close)

	c := New(worker.URL, 5*time.Second, 0)

	ctx, cancel := context.WithCancel(context.Background())
	// Cancel after a short delay to let the request reach the handler.
	time.AfterFunc(80*time.Millisecond, cancel)

	start := time.Now()
	_, err := c.Invoke(ctx, &InvokeRequest{Operation: "slow"})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected error from context cancel, got nil")
	}
	if elapsed > 1*time.Second {
		t.Errorf("Invoke took %v; should have returned promptly on context cancel", elapsed)
	}
	var urlErr *url.Error
	if !errors.Is(err, context.Canceled) &&
		!errors.Is(err, context.DeadlineExceeded) &&
		!(errors.As(err, &urlErr) && urlErr.Timeout()) {
		t.Errorf("expected context cancellation error, got: %v", err)
	}
}

// TestInvoke_LargeSuccessBody verifies Invoke correctly decodes a large valid response body.
// This ensures the response body reader is not arbitrarily limited for successful 200 responses.
func TestInvoke_LargeSuccessBody(t *testing.T) {
	// Build a payload field with 64 KB of data to confirm no silent truncation.
	largeData := strings.Repeat("a", 64*1024)
	worker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"payload":{"data":"` + largeData + `"},"billable_units":1}`))
	}))
	defer worker.Close()

	c := New(worker.URL, 5*time.Second, 0)
	resp, err := c.Invoke(context.Background(), &InvokeRequest{Operation: "x"})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if resp.BillableUnits != 1 {
		t.Errorf("billable_units = %d, want 1", resp.BillableUnits)
	}
	// Payload should decode the full body without truncation.
	if len(resp.Payload) < 64*1024 {
		t.Errorf("payload length %d, want >= 64 KB", len(resp.Payload))
	}
}

// ── Resilience tests ──────────────────────────────────────────────────────────

// TestInvoke_RetryOn5xx verifies that 5xx responses are retried and the final
// success is returned without error.
func TestInvoke_RetryOn5xx(t *testing.T) {
	var callCount atomic.Int32
	worker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := callCount.Add(1)
		if n < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"payload":{"ok":true},"billable_units":1}`))
	}))
	defer worker.Close()

	pol := ResiliencePolicy{
		Retry: resilience.Policy{MaxAttempts: 3, BaseBackoff: 1 * time.Millisecond, MaxBackoff: 5 * time.Millisecond},
	}
	c := New(worker.URL, 5*time.Second, 0, pol)

	resp, err := c.Invoke(context.Background(), &InvokeRequest{Operation: "x"})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if resp.BillableUnits != 1 {
		t.Errorf("billable_units = %d, want 1", resp.BillableUnits)
	}
	if n := callCount.Load(); n != 3 {
		t.Errorf("call count = %d, want 3 (2 failures + 1 success)", n)
	}
}

// TestInvoke_RetryRespectsMaxAttempts verifies that a permanently-failing worker is
// called exactly MaxAttempts times and no more.
func TestInvoke_RetryRespectsMaxAttempts(t *testing.T) {
	var callCount atomic.Int32
	worker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		callCount.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer worker.Close()

	pol := ResiliencePolicy{
		Retry: resilience.Policy{MaxAttempts: 3, BaseBackoff: 1 * time.Millisecond, MaxBackoff: 5 * time.Millisecond},
	}
	c := New(worker.URL, 5*time.Second, 0, pol)

	_, err := c.Invoke(context.Background(), &InvokeRequest{Operation: "x"})
	if err == nil {
		t.Fatal("Invoke: expected error for always-503 worker, got nil")
	}
	if n := callCount.Load(); n != 3 {
		t.Errorf("call count = %d, want 3 (MaxAttempts exhausted)", n)
	}
}

// TestInvoke_NoRetryOn200WorkerError asserts that a worker error envelope (HTTP 200
// with error field) is returned immediately without any retry — the worker already
// did billable work.
func TestInvoke_NoRetryOn200WorkerError(t *testing.T) {
	var callCount atomic.Int32
	worker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		callCount.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"error":{"code":"BAD_INPUT","message":"bad","retryable":false},"billable_units":1}`))
	}))
	defer worker.Close()

	pol := ResiliencePolicy{
		Retry: resilience.Policy{MaxAttempts: 5, BaseBackoff: 1 * time.Millisecond, MaxBackoff: 10 * time.Millisecond},
	}
	c := New(worker.URL, 5*time.Second, 0, pol)

	resp, err := c.Invoke(context.Background(), &InvokeRequest{Operation: "x"})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if resp.Error == nil || resp.Error.Code != "BAD_INPUT" {
		t.Fatalf("expected BAD_INPUT worker error, got %+v", resp.Error)
	}
	if n := callCount.Load(); n != 1 {
		t.Errorf("call count = %d, want 1 (no retry on HTTP 200 worker error)", n)
	}
}

// TestInvoke_NoRetryOn200Success asserts that a successful HTTP 200 response is
// not retried even when MaxAttempts > 1.
func TestInvoke_NoRetryOn200Success(t *testing.T) {
	var callCount atomic.Int32
	worker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		callCount.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"payload":{"ok":true},"billable_units":2}`))
	}))
	defer worker.Close()

	pol := ResiliencePolicy{
		Retry: resilience.Policy{MaxAttempts: 5, BaseBackoff: 1 * time.Millisecond, MaxBackoff: 10 * time.Millisecond},
	}
	c := New(worker.URL, 5*time.Second, 0, pol)

	_, err := c.Invoke(context.Background(), &InvokeRequest{Operation: "x"})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if n := callCount.Load(); n != 1 {
		t.Errorf("call count = %d, want 1 (no retry on HTTP 200 success)", n)
	}
}

// TestInvoke_RetriesStopOnCtxExpired extends TestInvoke_ContextDeadlineHonored:
// with MaxAttempts > 1 and a retryable 5xx, retries stop when the ctx deadline
// passes rather than spinning all MaxAttempts.
func TestInvoke_RetriesStopOnCtxExpired(t *testing.T) {
	var callCount atomic.Int32
	worker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		callCount.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer worker.Close()

	pol := ResiliencePolicy{
		// 20 attempts with 30ms base. Without ctx stopping retries, this would
		// make >= 20 calls over many seconds. Context expires at 80ms.
		Retry: resilience.Policy{MaxAttempts: 20, BaseBackoff: 30 * time.Millisecond, MaxBackoff: 1 * time.Second},
	}
	c := New(worker.URL, 5*time.Second, 0, pol)

	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()

	_, err := c.Invoke(ctx, &InvokeRequest{Operation: "x"})
	if err == nil {
		t.Fatal("expected error")
	}
	// The context expires well before MaxAttempts (20) would be exhausted.
	// We verify ctx — not exact timing — stops retries by asserting we never
	// reached MaxAttempts.
	if n := callCount.Load(); n >= 20 {
		t.Errorf("call count = %d, want < 20 (ctx expiry should stop retries before MaxAttempts)", n)
	}
}

// TestInvoke_BreakerFastFailWhileOpen asserts that zero HTTP calls reach the test
// server once the circuit breaker has opened.
func TestInvoke_BreakerFastFailWhileOpen(t *testing.T) {
	var callCount atomic.Int32
	worker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		callCount.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer worker.Close()

	pol := ResiliencePolicy{
		Breaker: resilience.BreakerConfig{Threshold: 2, Cooldown: time.Hour},
	}
	c := New(worker.URL, 5*time.Second, 0, pol)

	// Drive the breaker open with exactly Threshold failures.
	for i := 0; i < 2; i++ {
		_, err := c.Invoke(context.Background(), &InvokeRequest{Operation: "x"})
		if err == nil {
			t.Fatalf("expected error on attempt %d to open breaker, got nil", i+1)
		}
	}
	if c.breaker.CurrentState() != resilience.StateOpen {
		t.Fatalf("breaker not open after %d failures", 2)
	}

	callsBefore := callCount.Load()

	// These calls must fast-fail — no HTTP calls should reach the server.
	for i := 0; i < 5; i++ {
		_, err := c.Invoke(context.Background(), &InvokeRequest{Operation: "x"})
		if err == nil {
			t.Fatal("expected error while breaker is open")
		}
		if !errors.Is(err, resilience.ErrBreakerOpen) {
			t.Errorf("error %q should wrap ErrBreakerOpen", err.Error())
		}
	}

	if after := callCount.Load(); after != callsBefore {
		t.Errorf("got %d HTTP calls while breaker was open, want 0", after-callsBefore)
	}
}

// TestInvoke_BreakerClosesOnSuccessfulProbe verifies the full breaker lifecycle:
// open → half-open (after cooldown) → closed (after successful probe).
// Uses WithNow to advance the clock deterministically instead of time.Sleep.
func TestInvoke_BreakerClosesOnSuccessfulProbe(t *testing.T) {
	var callCount atomic.Int32
	// Server returns 5xx until the 3rd call, then succeeds.
	worker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := callCount.Add(1)
		if n <= 2 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"payload":{"ok":true},"billable_units":1}`))
	}))
	defer worker.Close()

	pol := ResiliencePolicy{
		// Long cooldown so real time never accidentally expires; we advance via WithNow.
		Breaker: resilience.BreakerConfig{Threshold: 2, Cooldown: time.Hour},
		// No retry — this test exercises breaker lifecycle, not retry behaviour.
		Retry: resilience.Policy{MaxAttempts: 1},
	}
	c := New(worker.URL, 5*time.Second, 0, pol)

	// Open the breaker with exactly Threshold failures.
	for i := 0; i < 2; i++ {
		_, err := c.Invoke(context.Background(), &InvokeRequest{Operation: "x"})
		if err == nil {
			t.Fatalf("expected error on attempt %d to open breaker, got nil", i+1)
		}
	}
	if c.breaker.CurrentState() != resilience.StateOpen {
		t.Fatal("expected StateOpen after threshold failures")
	}

	// Advance the fake clock 2 hours past real now so Allow() sees the cooldown as elapsed.
	fakeNow := time.Now().Add(2 * time.Hour)
	c.WithBreakerClock(func() time.Time { return fakeNow })

	// Probe: Allow() detects cooldown elapsed → StateHalfOpen; doOnce succeeds → StateClosed.
	resp, err := c.Invoke(context.Background(), &InvokeRequest{Operation: "x"})
	if err != nil {
		t.Fatalf("probe Invoke: %v", err)
	}
	if resp.BillableUnits != 1 {
		t.Errorf("probe billable_units = %d, want 1", resp.BillableUnits)
	}
	if c.breaker.CurrentState() != resilience.StateClosed {
		t.Errorf("breaker state = %v, want StateClosed after successful probe", c.breaker.CurrentState())
	}
}

// TestInvoke_DefaultPolicy_SingleShot verifies that the zero ResiliencePolicy
// reproduces today's exact single-shot behaviour (no retry on 5xx).
func TestInvoke_DefaultPolicy_SingleShot(t *testing.T) {
	var callCount atomic.Int32
	worker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		callCount.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("boom"))
	}))
	defer worker.Close()

	c := New(worker.URL, 5*time.Second, 0) // no policy = single shot
	_, err := c.Invoke(context.Background(), &InvokeRequest{Operation: "x"})
	if err == nil {
		t.Fatal("expected error")
	}
	if n := callCount.Load(); n != 1 {
		t.Errorf("call count = %d, want 1 (single-shot with no retry policy)", n)
	}
}
