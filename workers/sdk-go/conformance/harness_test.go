package conformance

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"runtime"
	"sync"
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

// runSpy runs f in a dedicated goroutine and waits for it. runtime.Goexit() calls
// (from spy.Fatal/Fatalf) and panics are both handled safely: deferred
// wg.Done() fires in all cases, so runSpy always returns.
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

// conformantHandler is a fixture that satisfies every contract requirement.
func conformantHandler(_ context.Context, _ crucible.Request) (crucible.Response, error) {
	return crucible.Response{
		Payload:       map[string]string{"result": "ok"},
		BillableUnits: 1,
	}, nil
}

// TestHarnessAcceptsConformantHandler proves Harness does not falsely reject a well-formed
// handler. Any failure inside Harness propagates to t and fails this test directly.
func TestHarnessAcceptsConformantHandler(t *testing.T) {
	Harness(t, conformantHandler)
}

// TestHarnessRejectsBillableUnitsZero proves assertInvokeContract detects billable_units=0
// in a raw response that bypasses the SDK's normalization guard.
func TestHarnessRejectsBillableUnitsZero(t *testing.T) {
	zero := uint64(0)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"status":"ok"}`))
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
			w.Write([]byte(`{"status":"ok"}`))
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
		w.WriteHeader(http.StatusServiceUnavailable)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"status":"down"}`))
	}))
	defer srv.Close()

	spy := &spyT{}
	runSpy(spy, func() { assertHealthz(spy, srv) })
	if !spy.hasFailed() {
		t.Fatal("assertHealthz should fail for non-200 /healthz status")
	}
}

// TestHarnessRejectsMalformedHealthz proves assertHealthz detects a /healthz body that
// does not match the byte-exact {"status":"ok"} requirement.
func TestHarnessRejectsMalformedHealthz(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"status":"degraded"}`))
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
			w.Write([]byte(`{"status":"ok"}`))
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

// TestHarnessRejectsErrorWithPayload proves assertHandlerStructuredError detects a
// structured error response that incorrectly carries a payload.
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

	spy := &spyT{}
	runSpy(spy, func() { assertHandlerStructuredError(spy, srv) })
	if !spy.hasFailed() {
		t.Fatal("assertHandlerStructuredError should fail when error response contains payload")
	}
}
