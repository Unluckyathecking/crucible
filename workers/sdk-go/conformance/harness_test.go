package conformance

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	crucible "github.com/Unluckyathecking/crucible/workers/sdk-go"
)

// Compile-time check: spyT must satisfy the tb interface.
var _ tb = (*spyT)(nil)

// spyT captures calls to Fatal/Fatalf/Errorf without propagating failures to the
// real test. Fatal and Fatalf call runtime.Goexit() to exit the goroutine cleanly;
// Errorf sets failed without exiting, matching testing.T.Errorf semantics.
// failed is an atomic.Bool so reads and writes are lock-free; mu protects the
// message slices and cleanups list.
type spyT struct {
	failed   atomic.Bool
	mu       sync.Mutex
	fatalMsg string   // last fatal message (only one, since Goexit stops further calls)
	errMsgs  []string // accumulated Errorf messages
	panicVal any      // value from recover(), non-nil if goroutine panicked
	cleanups []func()
}

// Helper is intentionally a no-op: spyT is not backed by a real *testing.T so
// stack-trace frame skipping is not available. Callers should not rely on reported
// line numbers from spy-based tests.
func (s *spyT) Helper() {}

func (s *spyT) Fatal(args ...any) {
	s.mu.Lock()
	s.fatalMsg = fmt.Sprint(args...)
	s.mu.Unlock()
	s.failed.Store(true)
	runtime.Goexit()
}

func (s *spyT) Fatalf(format string, args ...any) {
	s.mu.Lock()
	s.fatalMsg = fmt.Sprintf(format, args...)
	s.mu.Unlock()
	s.failed.Store(true)
	runtime.Goexit()
}

// Errorf marks the spy as failed without exiting the goroutine, matching
// testing.T.Errorf semantics (non-fatal: execution continues after the call).
func (s *spyT) Errorf(format string, args ...any) {
	s.mu.Lock()
	s.errMsgs = append(s.errMsgs, fmt.Sprintf(format, args...))
	s.mu.Unlock()
	s.failed.Store(true)
}

// Cleanup registers f to run after the spy's goroutine exits, in LIFO order,
// matching *testing.T.Cleanup semantics. runSpy drains the list after wg.Wait().
func (s *spyT) Cleanup(f func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cleanups = append(s.cleanups, f)
}

// runCleanups executes all registered Cleanup functions in LIFO order.
// Called by runSpy after the spy goroutine exits.
func (s *spyT) runCleanups() {
	s.mu.Lock()
	fns := make([]func(), len(s.cleanups))
	copy(fns, s.cleanups)
	s.cleanups = nil
	s.mu.Unlock()
	for i := len(fns) - 1; i >= 0; i-- {
		fns[i]()
	}
}

// hasFailed reports whether the spy captured a failure. Safe to call after
// runSpy returns.
func (s *spyT) hasFailed() bool {
	return s.failed.Load()
}

// lastFatal returns the last fatal message captured by Fatal or Fatalf.
func (s *spyT) lastFatal() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.fatalMsg
}

// panicValue returns the value passed to panic(), or nil if no panic occurred.
func (s *spyT) panicValue() any {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.panicVal
}

// runSpy runs f in a dedicated goroutine and waits for it. runtime.Goexit() (called
// by spy.Fatal/Fatalf) terminates the goroutine without unwinding the stack — it is
// NOT recoverable by recover(). The deferred wg.Done() fires for both Goexit and
// normal return, so runSpy always unblocks. The recover() block catches panics from
// other sources (not from spy.Fatal/Fatalf) and stores the panic value in spy.panicVal.
func runSpy(spy *spyT, f func()) {
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer func() {
			if r := recover(); r != nil {
				spy.mu.Lock()
				spy.panicVal = r
				spy.mu.Unlock()
				spy.failed.Store(true)
			}
		}()
		f()
	}()
	wg.Wait()
	spy.runCleanups()
}

// newMockServer builds an httptest.Server that returns {"status":"ok"} on /healthz,
// delegates /invoke to invokeH, and returns 404 for any other path. Used by
// negative-case tests that need a raw server bypassing the SDK to exercise specific
// assertion-detection paths.
func newMockServer(invokeH http.HandlerFunc) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/healthz":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"status":"ok"}`))
		case "/invoke":
			invokeH(w, r)
		default:
			http.NotFound(w, r)
		}
	}))
}

// TestHarnessAcceptsRetryableFalse proves that a handler returning a structured error
// with Retryable: false (the zero value) is accepted by Harness — the harness checks
// retryable is present (non-nil on the wire), not that it is true.
func TestHarnessAcceptsRetryableFalse(t *testing.T) {
	Harness(t, func(_ context.Context, _ crucible.Request) (crucible.Response, error) {
		return crucible.Response{}, &crucible.Error{Code: "NOT_RETRYABLE", Message: "permanent failure", Retryable: false}
	})
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

// TestHandlerRejectsNilHandler proves that crucible.Handler(nil) returns an error rather
// than panicking, documenting the nil boundary on the exported constructor.
func TestHandlerRejectsNilHandler(t *testing.T) {
	_, err := crucible.Handler(nil)
	if err == nil {
		t.Fatal("crucible.Handler(nil) should return an error")
	}
	if !strings.Contains(err.Error(), "nil HandlerFunc") {
		t.Fatalf("expected error to mention 'nil HandlerFunc', got: %v", err)
	}
}

// TestHarnessRejectsRawBillableUnitsZero proves assertInvokeContract detects billable_units=0
// in a raw response that bypasses the SDK's normalization guard.
func TestHarnessRejectsRawBillableUnitsZero(t *testing.T) {
	zero := uint64(0)
	srv := newMockServer(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(invokeResp{
			Payload:       json.RawMessage(`{"ok":true}`),
			BillableUnits: &zero,
		}); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	})
	defer srv.Close()

	spy := &spyT{}
	client := harnessClient()
	runSpy(spy, func() { assertInvokeContract(spy, srv, client) })
	if !spy.hasFailed() {
		t.Fatal("assertInvokeContract should fail for billable_units=0")
	}
}

// TestHarnessRejectsBothPayloadAndError proves assertInvokeContract detects a response
// that carries both payload and error simultaneously (never-both invariant).
func TestHarnessRejectsBothPayloadAndError(t *testing.T) {
	retryable := false
	units := uint64(1)
	srv := newMockServer(func(w http.ResponseWriter, _ *http.Request) {
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
	})
	defer srv.Close()

	spy := &spyT{}
	client := harnessClient()
	runSpy(spy, func() { assertInvokeContract(spy, srv, client) })
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
	client := harnessClient()
	runSpy(spy, func() { assertHealthz(spy, srv, client) })
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
	client := harnessClient()
	runSpy(spy, func() { assertHealthz(spy, srv, client) })
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
	client := harnessClient()
	runSpy(spy, func() { assertHealthz(spy, srv, client) })
	if !spy.hasFailed() {
		t.Fatal("assertHealthz should fail for a /healthz body that is not {\"status\":\"ok\"}")
	}
}

// TestHarnessRejectsInvokeNonPostMethod proves assertInvokeMethodNotAllowed detects a server
// that incorrectly accepts non-POST requests on /invoke (contract requires POST-only).
func TestHarnessRejectsInvokeNonPostMethod(t *testing.T) {
	// A raw server that returns 200 for all methods on /invoke (violates contract).
	srv := newMockServer(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		units := uint64(1)
		if err := json.NewEncoder(w).Encode(invokeResp{
			Payload:       json.RawMessage(`{"ok":true}`),
			BillableUnits: &units,
		}); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	})
	defer srv.Close()

	spy := &spyT{}
	client := harnessClient()
	runSpy(spy, func() { assertInvokeMethodNotAllowed(spy, srv, client) })
	if !spy.hasFailed() {
		t.Fatal("assertInvokeMethodNotAllowed should fail when non-POST /invoke returns 200 instead of 405")
	}
}

// TestHarnessRejectsEmptyInvokeEnvelope proves assertInvokeContract detects a response
// that carries neither payload nor error (empty envelope).
func TestHarnessRejectsEmptyInvokeEnvelope(t *testing.T) {
	srv := newMockServer(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Empty envelope: neither payload nor error present.
		if err := json.NewEncoder(w).Encode(invokeResp{}); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	})
	defer srv.Close()

	spy := &spyT{}
	client := harnessClient()
	runSpy(spy, func() { assertInvokeContract(spy, srv, client) })
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
	client := harnessClient()
	runSpy(spy, func() { checkNormalizationResponse(spy, srv, client) })
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
	client := harnessClient()
	runSpy(spy, func() { assertErrorEnvelope(spy, srv, client) })
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

	reqBody, err := json.Marshal(map[string]any{"operation": "err"})
	if err != nil {
		t.Fatalf("marshal request body: %v", err)
	}
	spy := &spyT{}
	client := harnessClient()
	runSpy(spy, func() { checkErrorEnvelopeAt(spy, srv, client, reqBody, "ERR_WITH_PAYLOAD") })
	if !spy.hasFailed() {
		t.Fatal("checkErrorEnvelopeAt should fail when error response contains payload")
	}
}

// TestSpyTPanicRecovery proves that runSpy catches goroutine panics, stores the
// panic value in spyT, and marks the spy as failed.
func TestSpyTPanicRecovery(t *testing.T) {
	spy := &spyT{}
	runSpy(spy, func() { panic("intentional panic") })
	if !spy.hasFailed() {
		t.Fatal("spy should mark failed when the goroutine panics")
	}
	if v := spy.panicValue(); v != "intentional panic" {
		t.Fatalf("expected panic value %q, got %v", "intentional panic", v)
	}
}

// TestHarnessPlainErrorWrapped proves the SDK wraps a plain (non-*crucible.Error) handler
// error as an INTERNAL structured error envelope, satisfying the frozen contract.
func TestHarnessPlainErrorWrapped(t *testing.T) {
	mux, err := crucible.Handler(func(_ context.Context, _ crucible.Request) (crucible.Response, error) {
		return crucible.Response{}, errors.New("plain error")
	})
	if err != nil {
		t.Fatalf("crucible.Handler: %v", err)
	}
	srv := httptest.NewServer(mux)
	defer srv.Close()
	client := harnessClient()
	reqBody, err := json.Marshal(map[string]any{"operation": "err"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	checkErrorEnvelopeAt(t, srv, client, reqBody, "INTERNAL")
}
