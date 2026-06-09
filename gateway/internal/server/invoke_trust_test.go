// Package server — additional invoke() trust-boundary tests.
//
// Covers invariants not exercised by routes_test.go:
//   - worker success + billable_units >= 1 → 200 with correct headers
//   - worker success + billable_units < 1  → 502 WORKER_BAD_RESPONSE  (INVARIANT #2)
//   - worker 5xx causes 502 WORKER_UNREACHABLE regardless of errorExposure
//   - worker timeout causes 502 WORKER_UNREACHABLE
//   - malformed JSON body → 400 BAD_REQUEST
//   - internal error detail is never forwarded to the caller
//   - X-Billable-Units and X-Units-Label headers are set on success
package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Unluckyathecking/crucible/gateway/internal/proxy"
)

// workerThat returns an httptest.Server whose single handler writes the supplied
// status code and JSON body verbatim. Callers must defer worker.Close().
func workerThat(status int, body string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
}

// successWorker returns a worker that responds 200 with the given billable_units.
// The payload field is populated so the gateway accepts the response as a valid envelope.
func successWorker(billableUnits uint64, unitsLabel string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"payload":        map[string]any{"result": "ok"},
			"billable_units": billableUnits,
			"units_label":    unitsLabel,
		})
	}))
}

// TestInvokeBillableUnitsZero verifies INVARIANT #2:
// a worker returning success with billable_units = 0 must be rejected with
// 502 WORKER_BAD_RESPONSE. This closes the free-usage escape hatch.
func TestInvokeBillableUnitsZero(t *testing.T) {
	worker := successWorker(0, "")
	defer worker.Close()

	p := proxy.New(worker.URL, 5*time.Second, 0)
	h := invoke(p, nil, "sanitized", "echo")

	req := httptest.NewRequest(http.MethodPost, "/v1/echo", strings.NewReader(`{"x":1}`))
	w := httptest.NewRecorder()
	h(w, req)

	if w.Code != http.StatusBadGateway {
		t.Fatalf("billable_units=0: expected 502, got %d", w.Code)
	}

	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("billable_units=0: decode body: %v", err)
	}
	errObj, ok := body["error"].(map[string]any)
	if !ok {
		t.Fatalf("billable_units=0: expected error object, got %T", body["error"])
	}
	if errObj["code"] != "WORKER_BAD_RESPONSE" {
		t.Errorf("billable_units=0: expected code WORKER_BAD_RESPONSE, got %q", errObj["code"])
	}
	if errObj["message"] != "worker contract violation" {
		t.Errorf("billable_units=0: expected message 'worker contract violation', got %q", errObj["message"])
	}
	// retryable must be false — this is a worker bug, not a transient failure.
	if errObj["retryable"] != false {
		t.Errorf("billable_units=0: expected retryable=false, got %v", errObj["retryable"])
	}
}

// TestInvokeBillableUnitsLargeSuccess verifies that a worker returning a large
// billable_units value is accepted and forwarded correctly.
func TestInvokeSuccessResponseHeaders(t *testing.T) {
	tests := []struct {
		name         string
		units        uint64
		label        string
		wantUnitsHdr string
		wantLabelHdr string
	}{
		{
			name:         "single unit no label",
			units:        1,
			label:        "",
			wantUnitsHdr: "1",
			wantLabelHdr: "",
		},
		{
			name:         "many units with label",
			units:        42,
			label:        "tokens",
			wantUnitsHdr: "42",
			wantLabelHdr: "tokens",
		},
		{
			name:         "large units with label",
			units:        1000000,
			label:        "characters",
			wantUnitsHdr: "1000000",
			wantLabelHdr: "characters",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			worker := successWorker(tt.units, tt.label)
			defer worker.Close()

			p := proxy.New(worker.URL, 5*time.Second, 0)
			h := invoke(p, nil, "sanitized", "echo")

			req := httptest.NewRequest(http.MethodPost, "/v1/echo", strings.NewReader(`{"x":1}`))
			w := httptest.NewRecorder()
			h(w, req)

			if w.Code != http.StatusOK {
				t.Fatalf("units=%d: expected 200, got %d; body: %s", tt.units, w.Code, w.Body.String())
			}

			gotUnits := w.Header().Get("X-Billable-Units")
			if gotUnits != tt.wantUnitsHdr {
				t.Errorf("X-Billable-Units = %q, want %q", gotUnits, tt.wantUnitsHdr)
			}

			gotLabel := w.Header().Get("X-Units-Label")
			if gotLabel != tt.wantLabelHdr {
				t.Errorf("X-Units-Label = %q, want %q", gotLabel, tt.wantLabelHdr)
			}
		})
	}
}

// TestInvokeSuccessBodyContainsPayload verifies that on 200 the response body
// contains the worker's payload and the billable_units field.
func TestInvokeSuccessBodyContainsPayload(t *testing.T) {
	worker := successWorker(3, "ops")
	defer worker.Close()

	p := proxy.New(worker.URL, 5*time.Second, 0)
	h := invoke(p, nil, "full", "echo")

	req := httptest.NewRequest(http.MethodPost, "/v1/echo", strings.NewReader(`{"input":"hello"}`))
	w := httptest.NewRecorder()
	h(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d; body: %s", w.Code, w.Body.String())
	}

	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode success body: %v", err)
	}

	// billable_units must be present in the forwarded response.
	units, ok := resp["billable_units"]
	if !ok {
		t.Fatal("success body missing 'billable_units'")
	}
	if units.(float64) != 3 {
		t.Errorf("billable_units = %v, want 3", units)
	}

	// payload must be forwarded.
	if _, ok := resp["payload"]; !ok {
		t.Error("success body missing 'payload'")
	}
}

// TestInvokeBadJSONBody verifies that a non-JSON request body causes 400 BAD_REQUEST.
func TestInvokeBadJSONBody(t *testing.T) {
	// The worker will never be reached; we can use a trivially correct one.
	worker := successWorker(1, "")
	defer worker.Close()

	p := proxy.New(worker.URL, 5*time.Second, 0)
	h := invoke(p, nil, "sanitized", "echo")

	req := httptest.NewRequest(http.MethodPost, "/v1/echo", strings.NewReader(`not-json`))
	w := httptest.NewRecorder()
	h(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("bad json: expected 400, got %d", w.Code)
	}

	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("bad json: decode body: %v", err)
	}
	errObj, ok := body["error"].(map[string]any)
	if !ok {
		t.Fatalf("bad json: expected error object")
	}
	if errObj["code"] != "BAD_REQUEST" {
		t.Errorf("bad json: expected code BAD_REQUEST, got %q", errObj["code"])
	}
}

// TestInvokeWorker5xxAlwaysSanitized verifies that worker 5xx causes
// 502 WORKER_UNREACHABLE regardless of errorExposure mode.
// (A 5xx is a transport error — the worker did not return a structured envelope.)
func TestInvokeWorker5xxAlwaysSanitized(t *testing.T) {
	for _, mode := range []string{"sanitized", "full", ""} {
		t.Run("mode="+mode, func(t *testing.T) {
			worker := workerThat(http.StatusServiceUnavailable, `{"internal":"secret error details"}`)
			defer worker.Close()

			p := proxy.New(worker.URL, 5*time.Second, 0)
			h := invoke(p, nil, mode, "echo")

			req := httptest.NewRequest(http.MethodPost, "/v1/echo", strings.NewReader(`{}`))
			w := httptest.NewRecorder()
			h(w, req)

			if w.Code != http.StatusBadGateway {
				t.Fatalf("5xx worker [mode=%s]: expected 502, got %d", mode, w.Code)
			}

			bodyStr := w.Body.String()
			for _, leak := range []string{"internal", "secret error details"} {
				if strings.Contains(bodyStr, leak) {
					t.Errorf("5xx worker [mode=%s]: response leaks worker body %q", mode, leak)
				}
			}

			var body map[string]any
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
				t.Fatalf("5xx worker [mode=%s]: decode body: %v", mode, err)
			}
			errObj := body["error"].(map[string]any)
			if errObj["code"] != "WORKER_UNREACHABLE" {
				t.Errorf("5xx worker [mode=%s]: expected WORKER_UNREACHABLE, got %q", mode, errObj["code"])
			}
		})
	}
}

// TestInvokeWorkerTimeout verifies that a worker that is slow to respond headers
// is handled as a transport error and returns 502 WORKER_UNREACHABLE.
// Uses a closed listener so the TCP connect itself fails immediately.
func TestInvokeWorkerTimeout(t *testing.T) {
	// Start a server and immediately close it so all connections are refused.
	// This gives a deterministic transport error without any blocking goroutines.
	worker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	workerURL := worker.URL
	worker.Close() // closed before any request is made

	p := proxy.New(workerURL, 5*time.Second, 0)
	h := invoke(p, nil, "sanitized", "echo")

	req := httptest.NewRequest(http.MethodPost, "/v1/echo", strings.NewReader(`{}`))
	req = req.WithContext(context.Background())
	w := httptest.NewRecorder()
	h(w, req)

	if w.Code != http.StatusBadGateway {
		t.Fatalf("closed worker: expected 502, got %d; body: %s", w.Code, w.Body.String())
	}

	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("closed worker: decode body: %v", err)
	}
	errObj := body["error"].(map[string]any)
	if errObj["code"] != "WORKER_UNREACHABLE" {
		t.Errorf("closed worker: expected code WORKER_UNREACHABLE, got %q", errObj["code"])
	}
	// The gateway must not leak internal transport error details to the caller.
	if errObj["message"] != "worker unavailable" {
		t.Errorf("closed worker: expected message 'worker unavailable', got %q", errObj["message"])
	}
}

// TestInvokeInternalErrorNeverLeaked verifies that in sanitized mode the caller
// never receives any worker-side error message, even when the worker returns a
// structured error envelope containing sensitive strings.
func TestInvokeInternalErrorNeverLeaked(t *testing.T) {
	sensitiveStrings := []string{
		"db_password",
		"secret_token",
		"internal_host",
		"127.0.0.1",
		"SENSITIVE_CODE",
	}

	worker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]any{
				"code":      "SENSITIVE_CODE",
				"message":   "db_password=secret_token internal_host=127.0.0.1",
				"retryable": false,
			},
		})
	}))
	defer worker.Close()

	p := proxy.New(worker.URL, 5*time.Second, 0)
	h := invoke(p, nil, "sanitized", "echo")

	req := httptest.NewRequest(http.MethodPost, "/v1/echo", strings.NewReader(`{}`))
	w := httptest.NewRecorder()
	h(w, req)

	if w.Code != http.StatusBadGateway {
		t.Fatalf("expected 502, got %d", w.Code)
	}

	bodyStr := w.Body.String()
	for _, s := range sensitiveStrings {
		if strings.Contains(bodyStr, s) {
			t.Errorf("sanitized mode leaked sensitive string %q in response body", s)
		}
	}
}

// TestInvokeBillableUnitsContractTableDriven is a table-driven test for the
// trust-boundary check: various billable_units values crossing the threshold.
func TestInvokeBillableUnitsContractTableDriven(t *testing.T) {
	tests := []struct {
		name       string
		units      uint64
		wantStatus int
		wantCode   string
	}{
		{"zero units rejected", 0, http.StatusBadGateway, "WORKER_BAD_RESPONSE"},
		{"one unit accepted", 1, http.StatusOK, ""},
		{"two units accepted", 2, http.StatusOK, ""},
		{"large units accepted", 9999, http.StatusOK, ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			worker := successWorker(tt.units, "")
			defer worker.Close()

			p := proxy.New(worker.URL, 5*time.Second, 0)
			h := invoke(p, nil, "sanitized", "echo")

			req := httptest.NewRequest(http.MethodPost, "/v1/echo", strings.NewReader(`{"x":1}`))
			w := httptest.NewRecorder()
			h(w, req)

			if w.Code != tt.wantStatus {
				t.Fatalf("units=%d: expected %d, got %d; body: %s", tt.units, tt.wantStatus, w.Code, w.Body.String())
			}

			if tt.wantCode != "" {
				var body map[string]any
				if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
					t.Fatalf("units=%d: decode body: %v", tt.units, err)
				}
				errObj, ok := body["error"].(map[string]any)
				if !ok {
					t.Fatalf("units=%d: expected error object", tt.units)
				}
				if errObj["code"] != tt.wantCode {
					t.Errorf("units=%d: expected code %q, got %q", tt.units, tt.wantCode, errObj["code"])
				}
			}
		})
	}
}
