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
	"testing"
	"time"
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
	defer l.Close()

	go func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			// Just hold the connection open.
			defer conn.Close()
		}
	}()

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
