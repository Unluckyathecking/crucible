package crucible

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

// --- parity: names must be byte-identical across Go/Rust/TS --------------------

func TestMetricNamesParity(t *testing.T) {
	const (
		wantRequests = "crucible_worker_requests_total"
		wantErrors   = "crucible_worker_errors_total"
		wantDuration = "crucible_worker_request_duration_seconds"
	)
	if metricRequestsTotal != wantRequests {
		t.Errorf("requests name: got %q want %q", metricRequestsTotal, wantRequests)
	}
	if metricErrorsTotal != wantErrors {
		t.Errorf("errors name: got %q want %q", metricErrorsTotal, wantErrors)
	}
	if metricDurationSecs != wantDuration {
		t.Errorf("duration name: got %q want %q", metricDurationSecs, wantDuration)
	}
}

// --- cardinality: only {operation, outcome} are ever used as labels -----------

func TestMetricLabelCardinality(t *testing.T) {
	m := newWorkerMetrics()
	// Exercise both outcome values so the error counter path is also exercised.
	m.observe("test_op", "ok", 0)
	m.observe("test_op", "error", time.Millisecond)
	m.observe("other_op", "ok", 500*time.Millisecond)

	// Scrape the /metrics text output and verify no unbounded label names appear.
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	w := httptest.NewRecorder()
	m.httpHandler().ServeHTTP(w, req)

	body := w.Body.String()

	forbidden := []string{
		"request_id", "customer_id", "payload", "plan", "metadata",
	}
	for _, label := range forbidden {
		if strings.Contains(body, label+`="`) {
			t.Errorf("forbidden label %q found in metrics output", label)
		}
	}
	if !strings.Contains(body, `operation="test_op"`) {
		t.Error("operation label not found in metrics output")
	}
	if !strings.Contains(body, `outcome="ok"`) {
		t.Error(`outcome="ok" label not found in metrics output`)
	}
	if !strings.Contains(body, `outcome="error"`) {
		t.Error(`outcome="error" label not found in metrics output`)
	}
	if !strings.Contains(body, `operation="other_op"`) {
		t.Error("other_op operation label not found — multiple operations must not bleed into each other")
	}
}

// --- success path: requests counter and duration are recorded -----------------

func TestMetricsRecordedOnSuccess(t *testing.T) {
	m := newWorkerMetrics()
	h := invokeHandler(func(_ context.Context, _ Request) (Response, error) {
		return Response{Payload: "ok"}, nil
	}, "", m)

	payload, _ := json.Marshal(Request{Operation: "do_thing"})
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/invoke", bytes.NewReader(payload))
	r.Header.Set("Content-Type", "application/json")
	h(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", w.Code)
	}

	req2 := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	w2 := httptest.NewRecorder()
	m.httpHandler().ServeHTTP(w2, req2)
	body := w2.Body.String()

	if !strings.Contains(body, `crucible_worker_requests_total{operation="do_thing",outcome="ok"} 1`) {
		t.Errorf("requests counter not recorded; metrics output:\n%s", body)
	}
}

// --- error path: errors counter is recorded with outcome=error ----------------

func TestMetricsRecordedOnError(t *testing.T) {
	m := newWorkerMetrics()
	h := invokeHandler(func(_ context.Context, _ Request) (Response, error) {
		return Response{}, errors.New("boom")
	}, "", m)

	payload, _ := json.Marshal(Request{Operation: "fail_op"})
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/invoke", bytes.NewReader(payload))
	r.Header.Set("Content-Type", "application/json")
	h(w, r)

	req2 := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	w2 := httptest.NewRecorder()
	m.httpHandler().ServeHTTP(w2, req2)
	body := w2.Body.String()

	if !strings.Contains(body, `crucible_worker_errors_total{operation="fail_op",outcome="error"} 1`) {
		t.Errorf("errors counter not recorded; metrics output:\n%s", body)
	}
	if !strings.Contains(body, `crucible_worker_requests_total{operation="fail_op",outcome="error"} 1`) {
		t.Errorf("requests counter not recorded for error; metrics output:\n%s", body)
	}
}

// --- disabled path: nil metrics, no second listener, /invoke identical --------

func TestMetricsDisabledPathNoSideEffects(t *testing.T) {
	// initMetrics must return nil,nil when WORKER_METRICS_PORT is completely unset
	// (not just set to an empty string — os.LookupEnv distinguishes these cases).
	saved, hadVal := os.LookupEnv("WORKER_METRICS_PORT")
	os.Unsetenv("WORKER_METRICS_PORT")
	defer func() {
		if hadVal {
			os.Setenv("WORKER_METRICS_PORT", saved)
		} else {
			os.Unsetenv("WORKER_METRICS_PORT")
		}
	}()
	if m, srv := initMetrics(); m != nil || srv != nil {
		t.Error("expected nil metrics when WORKER_METRICS_PORT is not set")
	}

	// An empty string must also disable metrics (os.Getenv returns "" for both cases,
	// but explicitly verify the empty-string branch for completeness).
	t.Setenv("WORKER_METRICS_PORT", "")
	if m, srv := initMetrics(); m != nil || srv != nil {
		t.Error("expected nil metrics when WORKER_METRICS_PORT is empty string")
	}

	// invokeHandler with nil metrics must behave byte-for-byte like today.
	h := invokeHandler(func(_ context.Context, in Request) (Response, error) {
		return Response{Payload: "ok"}, nil
	}, "", nil)

	payload, _ := json.Marshal(Request{RequestID: "r1", Operation: "no_metrics"})
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/invoke", bytes.NewReader(payload))
	r.Header.Set("Content-Type", "application/json")
	h(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", w.Code)
	}
	var body map[string]any
	if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, hasErr := body["error"]; hasErr {
		t.Fatalf("unexpected error with metrics disabled: %v", body["error"])
	}
}

// --- billable_units contract: metrics recording must not alter the response ---

func TestBillableUnitsContractUnchangedWithMetrics(t *testing.T) {
	m := newWorkerMetrics()
	h := invokeHandler(func(_ context.Context, _ Request) (Response, error) {
		return Response{Payload: "ok"}, nil // BillableUnits == 0 → must default to 1
	}, "", m)

	payload, _ := json.Marshal(Request{Operation: "units_op"})
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/invoke", bytes.NewReader(payload))
	r.Header.Set("Content-Type", "application/json")
	h(w, r)

	var body map[string]any
	if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	units, ok := body["billable_units"].(float64)
	if !ok || units < 1 {
		t.Fatalf("want billable_units>=1, got %v", body["billable_units"])
	}
}

// TestMetricsExplicitBillableUnitsPreservedWithMetrics verifies that an explicit
// non-zero BillableUnits value is not altered when metrics are active.
func TestMetricsExplicitBillableUnitsPreservedWithMetrics(t *testing.T) {
	const want = uint64(7)
	m := newWorkerMetrics()
	h := invokeHandler(func(_ context.Context, _ Request) (Response, error) {
		return Response{Payload: "ok", BillableUnits: want}, nil
	}, "", m)

	payload, _ := json.Marshal(Request{Operation: "units_explicit"})
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/invoke", bytes.NewReader(payload))
	r.Header.Set("Content-Type", "application/json")
	h(w, r)

	var body map[string]any
	if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	got := uint64(body["billable_units"].(float64))
	if got != want {
		t.Fatalf("want %d billable_units, got %d", want, got)
	}
}
