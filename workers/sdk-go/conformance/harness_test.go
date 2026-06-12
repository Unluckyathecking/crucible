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

// spyT captures calls to Fatal/Fatalf/Errorf without propagating them to the real test.
// Fatal and Fatalf call runtime.Goexit() so the goroutine exits cleanly with all deferred
// functions run; the caller checks spy.failed after the goroutine completes.
type spyT struct {
	mu     sync.Mutex
	failed bool
}

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

func (s *spyT) Errorf(_ string, _ ...any) {
	s.mu.Lock()
	s.failed = true
	s.mu.Unlock()
}

// runSpy runs f in a goroutine and waits for it. Handles runtime.Goexit() via defer.
func runSpy(spy *spyT, f func()) {
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		f()
	}()
	wg.Wait()
}

// conformantHandler is a fixture that satisfies the frozen contract.
func conformantHandler(_ context.Context, _ crucible.Request) (crucible.Response, error) {
	return crucible.Response{
		Payload:       map[string]string{"result": "ok"},
		BillableUnits: 1,
	}, nil
}

// TestHarnessAcceptsConformantHandler proves Harness does not falsely reject a handler
// that satisfies every contract requirement.
func TestHarnessAcceptsConformantHandler(t *testing.T) {
	Harness(t, conformantHandler)
}

// TestHarnessRejectsBillableUnitsZero proves assertInvokeContract detects billable_units=0
// in a response that bypasses the SDK's normalization (raw server emitting 0 directly).
func TestHarnessRejectsBillableUnitsZero(t *testing.T) {
	zero := uint64(0)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"status":"ok"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(invokeResp{
			Payload:       json.RawMessage(`{"ok":true}`),
			BillableUnits: &zero,
		})
	}))
	defer srv.Close()

	spy := &spyT{}
	runSpy(spy, func() { assertInvokeContract(spy, srv) })
	if !spy.failed {
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
		json.NewEncoder(w).Encode(invokeResp{
			Payload:       json.RawMessage(`{"ok":true}`),
			BillableUnits: &units,
			Error: &invokeError{
				Code:      "BOTH",
				Message:   "illegal: both payload and error set",
				Retryable: &retryable,
			},
		})
	}))
	defer srv.Close()

	spy := &spyT{}
	runSpy(spy, func() { assertInvokeContract(spy, srv) })
	if !spy.failed {
		t.Fatal("assertInvokeContract should fail when both payload and error are set")
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
	if !spy.failed {
		t.Fatal("assertHealthz should fail for a /healthz body that is not {\"status\":\"ok\"}")
	}
}
