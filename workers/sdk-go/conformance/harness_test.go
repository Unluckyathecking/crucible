package conformance

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"

	crucible "github.com/Unluckyathecking/crucible/workers/sdk-go"
)

// spyT captures calls to Fatal/Fatalf/Errorf without propagating failures to the
// real test. Fatal and Fatalf call runtime.Goexit() to exit the goroutine cleanly;
// Errorf sets failed without exiting, matching testing.T.Errorf semantics.
type spyT struct {
	mu     sync.Mutex
	failed bool
}

// Helper is intentionally a no-op: spyT is not backed by a real *testing.T so
// stack-trace frame skipping is not available. Callers should not rely on reported
// line numbers from spy-based tests.
func (s *spyT) Helper() {}

func (s *spyT) Fatal(_ ...any) {
	s.mu.Lock()
	s.failed = true
	s.mu.Unlock()
	runtime.Goexit()
}

func (s *spyT) Fatalf(_ string, _ ...any) {
	s.mu.Lock()
	s.failed = true
	s.mu.Unlock()
	runtime.Goexit()
}

// Errorf marks the spy as failed without exiting the goroutine, matching
// testing.T.Errorf semantics (non-fatal: execution continues after the call).
func (s *spyT) Errorf(_ string, _ ...any) {
	s.mu.Lock()
	s.failed = true
	s.mu.Unlock()
}

// hasFailed reports whether the spy captured a failure. Safe to call after
// runSpy returns; uses mu to establish the same happens-before as the writes.
func (s *spyT) hasFailed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.failed
}

// runSpy runs f in a dedicated goroutine and waits for it. runtime.Goexit() (called
// by spy.Fatal/Fatalf) terminates the goroutine without unwinding the stack — it is
// NOT recoverable by recover(). The deferred wg.Done() fires for both Goexit and
// normal return, so runSpy always unblocks. The recover() block catches panics from
// other sources (not from spy.Fatal/Fatalf) and marks the spy as failed.
func runSpy(spy *spyT, f func()) {
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer func() {
			if r := recover(); r != nil {
				spy.mu.Lock()
				spy.failed = true
				spy.mu.Unlock()
			}
		}()
		f()
	}()
	wg.Wait()
}

// TestHarnessAcceptsConformantHandler proves Harness does not falsely reject a well-formed
// handler. The invocation counter proves Harness actually called the handler, not just
// that it completed without failing. BillableUnits:0 exercises the SDK normalization path.
func TestHarnessAcceptsConformantHandler(t *testing.T) {
	var invoked int64
	handler := func(_ context.Context, _ crucible.Request) (crucible.Response, error) {
		atomic.AddInt64(&invoked, 1)
		return crucible.Response{
			Payload:       map[string]string{"result": "ok"},
			BillableUnits: 0,
		}, nil
	}
	Harness(t, handler)
	if atomic.LoadInt64(&invoked) == 0 {
		t.Fatal("handler was never invoked by Harness")
	}
}

// TestHarnessRejectsNilHandler proves that crucible.Handler(nil) returns an error rather
// than panicking, documenting the nil boundary on the exported constructor.
func TestHarnessRejectsNilHandler(t *testing.T) {
	_, err := crucible.Handler(nil)
	if err == nil {
		t.Fatal("crucible.Handler(nil) should return an error")
	}
}

// TestHarnessRejectsBillableUnitsZero proves assertInvokeContract detects billable_units=0
// in a raw response that bypasses the SDK's normalization guard.
func TestHarnessRejectsBillableUnitsZero(t *testing.T) {
	zero := uint64(0)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"status":"ok"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(invokeResp{
			Payload:       json.RawMessage(`{"ok":true}`),
			BillableUnits: &zero,
		}); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}))
	defer srv.Close()

	spy := &spyT{}
	runSpy(spy, func() { assertInvokeContract(spy, srv) })
	if !spy.hasFailed() {
		t.Fatal("assertInvokeContract should fail for billable_units=0")
	}
}

// TestHarnessRejectsBothPayloadAndError proves assertInvokeContract detects a response
// that carries both payload and error simultaneously (never-both invariant).
func TestHarnessRejectsBothPayloadAndError(t *testing.T) {
	retryable := false
	units := uint64(1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"status":"ok"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(invokeResp{
			Payload:       json.RawMessage(`{"ok":true}`),
			BillableUnits: &units,
			Error: &invokeError{
				Code:      "BOTH",
				Message:   "illegal: both payload and error set",
				Retryable: &retryable,
			},
		}); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}))
	defer srv.Close()

	spy := &spyT{}
	runSpy(spy, func() { assertInvokeContract(spy, srv) })
	if !spy.hasFailed() {
		t.Fatal("assertInvokeContract should fail when both payload and error are set")
	}
}

// TestHarnessRejectsHealthzNon200 proves assertHealthz detects a non-200 /healthz status.
func TestHarnessRejectsHealthzNon200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"status":"down"}`))
	}))
	defer srv.Close()

	spy := &spyT{}
	runSpy(spy, func() { assertHealthz(spy, srv) })
	if !spy.hasFailed() {
		t.Fatal("assertHealthz should fail for non-200 /healthz status")
	}
}

// TestHarnessRejectsHealthzWrongContentType proves assertHealthz detects a /healthz
// response with the correct body and status but a non-JSON Content-Type.
func TestHarnessRejectsHealthzWrongContentType(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	defer srv.Close()

	spy := &spyT{}
	runSpy(spy, func() { assertHealthz(spy, srv) })
	if !spy.hasFailed() {
		t.Fatal("assertHealthz should fail for a /healthz response with non-JSON Content-Type")
	}
}

// TestHarnessRejectsMalformedHealthz proves assertHealthz detects a /healthz body that
// does not match the byte-exact {"status":"ok"} requirement.
func TestHarnessRejectsMalformedHealthz(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"degraded"}`))
	}))
	defer srv.Close()

	spy := &spyT{}
	runSpy(spy, func() { assertHealthz(spy, srv) })
	if !spy.hasFailed() {
		t.Fatal("assertHealthz should fail for a /healthz body that is not {\"status\":\"ok\"}")
	}
}

// TestHarnessRejectsEmptyInvokeEnvelope proves assertInvokeContract detects a response
// that carries neither payload nor error (empty envelope).
func TestHarnessRejectsEmptyInvokeEnvelope(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"status":"ok"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		// Empty envelope: neither payload nor error present.
		if err := json.NewEncoder(w).Encode(invokeResp{}); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}))
	defer srv.Close()

	spy := &spyT{}
	runSpy(spy, func() { assertInvokeContract(spy, srv) })
	if !spy.hasFailed() {
		t.Fatal("assertInvokeContract should fail when response has neither payload nor error")
	}
}

// TestHarnessRejectsNormalizationZero proves checkNormalizationResponse detects
// billable_units=0 in a raw response that bypasses the SDK's normalization guard.
func TestHarnessRejectsNormalizationZero(t *testing.T) {
	zero := uint64(0)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(invokeResp{
			Payload:       json.RawMessage(`{"norm":"bypass"}`),
			BillableUnits: &zero,
		}); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}))
	defer srv.Close()

	spy := &spyT{}
	runSpy(spy, func() { checkNormalizationResponse(spy, srv) })
	if !spy.hasFailed() {
		t.Fatal("checkNormalizationResponse should fail for billable_units=0")
	}
}

// TestHarnessRejectsSuccessEnvelopeOnMalformedRequest proves assertErrorEnvelope detects
// a server that returns a success envelope instead of a structured error for malformed JSON.
func TestHarnessRejectsSuccessEnvelopeOnMalformedRequest(t *testing.T) {
	units := uint64(1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(invokeResp{
			Payload:       json.RawMessage(`{"ok":true}`),
			BillableUnits: &units,
		}); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}))
	defer srv.Close()

	spy := &spyT{}
	runSpy(spy, func() { assertErrorEnvelope(spy, srv) })
	if !spy.hasFailed() {
		t.Fatal("assertErrorEnvelope should fail when server returns success instead of error")
	}
}

// TestHarnessRejectsErrorWithPayload proves checkErrorEnvelopeAt detects a structured
// error response that incorrectly carries a payload. assertHandlerStructuredError delegates
// to checkErrorEnvelopeAt, so this covers the shared detection path.
func TestHarnessRejectsErrorWithPayload(t *testing.T) {
	retryable := true
	units := uint64(1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(invokeResp{
			Payload:       json.RawMessage(`{"bad":"data"}`),
			BillableUnits: &units,
			Error: &invokeError{
				Code:      "ERR_WITH_PAYLOAD",
				Message:   "error should not have payload",
				Retryable: &retryable,
			},
		}); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}))
	defer srv.Close()

	reqBody, _ := json.Marshal(map[string]any{"operation": "err"})
	spy := &spyT{}
	runSpy(spy, func() { checkErrorEnvelopeAt(spy, srv, reqBody) })
	if !spy.hasFailed() {
		t.Fatal("checkErrorEnvelopeAt should fail when error response contains payload")
	}
}
