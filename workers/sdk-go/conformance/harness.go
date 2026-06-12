// Package conformance provides an in-process harness that asserts the frozen
// tool.proto HTTP/JSON worker contract. Product workers call Harness from their
// go test suite to verify their handler satisfies the contract without binding
// a real port or spawning a subprocess.
package conformance

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	crucible "github.com/Unluckyathecking/crucible/workers/sdk-go"
)

// tb is the subset of *testing.T used by the assertion helpers. A recording shim
// can implement this interface to verify the harness detects violations in the
// package's own unit tests.
type tb interface {
	Helper()
	Fatalf(format string, args ...any)
	Fatal(args ...any)
	Errorf(format string, args ...any)
}

// invokeResp is the frozen contract shape for /invoke responses (mirrors test/conformance).
type invokeResp struct {
	Payload       json.RawMessage `json:"payload"`
	BillableUnits *uint64         `json:"billable_units"`
	Error         *invokeError    `json:"error"`
}

type invokeError struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	Retryable *bool  `json:"retryable"`
}

// Harness drives crucible.Handler(h) in-process via httptest.NewServer and asserts
// the frozen tool.proto HTTP/JSON contract. Call from a product worker's go test:
//
//	func TestConformance(t *testing.T) { conformance.Harness(t, myHandler) }
func Harness(t *testing.T, h crucible.HandlerFunc) {
	t.Helper()
	srv := httptest.NewServer(crucible.Handler(h))
	defer srv.Close()
	assertHealthz(t, srv)
	assertInvokeContract(t, srv)
	assertBillableUnitsNormalization(t)
	assertErrorEnvelope(t, srv)
}

// assertHealthz checks GET /healthz returns 200 and body byte-exactly {"status":"ok"}.
func assertHealthz(t tb, srv *httptest.Server) {
	t.Helper()
	resp, err := http.Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /healthz: expected 200, got %d", resp.StatusCode)
		return
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Fatalf("GET /healthz: expected Content-Type application/json, got %q", ct)
		return
	}
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(resp.Body); err != nil {
		t.Fatalf("GET /healthz: read body: %v", err)
		return
	}
	if got := strings.TrimSpace(buf.String()); got != `{"status":"ok"}` {
		t.Fatalf("GET /healthz: expected {\"status\":\"ok\"}, got %q", got)
	}
}

// assertInvokeContract checks POST /invoke with a valid envelope returns 200 with a
// valid shape: either success (payload present, billable_units >= 1) or a structured
// error envelope, never both, never billable_units < 1 on success.
func assertInvokeContract(t tb, srv *httptest.Server) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{
		"request_id": "conformance-1",
		"operation":  "conformance",
		"payload":    map[string]string{"hello": "world"},
	})
	resp, err := http.Post(srv.URL+"/invoke", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST /invoke: %v", err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /invoke: expected 200, got %d", resp.StatusCode)
		return
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Fatalf("POST /invoke: expected Content-Type application/json, got %q", ct)
		return
	}
	var r invokeResp
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		t.Fatalf("POST /invoke: decode: %v", err)
		return
	}

	hasPayload := len(r.Payload) > 0 && string(r.Payload) != "null"
	hasError := r.Error != nil

	if hasPayload && hasError {
		t.Fatal("POST /invoke: response must not contain both payload and error")
		return
	}
	if !hasPayload && !hasError {
		t.Fatal("POST /invoke: response must contain payload+billable_units or error")
		return
	}
	if hasPayload {
		if r.BillableUnits == nil || *r.BillableUnits < 1 {
			t.Fatalf("POST /invoke success: expected billable_units >= 1, got %v", r.BillableUnits)
		}
	}
	if hasError {
		if r.Error.Code == "" {
			t.Fatal("POST /invoke error.code must be non-empty")
		}
		if r.Error.Message == "" {
			t.Fatal("POST /invoke error.message must be non-empty")
		}
		if r.Error.Retryable == nil {
			t.Fatal("POST /invoke error.retryable must be present")
		}
	}
}

// assertBillableUnitsNormalization verifies that crucible.Handler normalizes a zero
// BillableUnits to >= 1 (mirrors invokeHandler + the gateway trust boundary).
// Uses an internal fixture handler to exercise the SDK normalization path directly.
func assertBillableUnitsNormalization(t tb) {
	t.Helper()
	// Zero-units fixture: explicitly returns 0 to exercise the SDK's normalization guard.
	zeroH := func(_ context.Context, _ crucible.Request) (crucible.Response, error) {
		return crucible.Response{Payload: map[string]string{"norm": "ok"}, BillableUnits: 0}, nil
	}
	normSrv := httptest.NewServer(crucible.Handler(zeroH))
	defer normSrv.Close()

	body, _ := json.Marshal(map[string]any{"operation": "norm"})
	resp, err := http.Post(normSrv.URL+"/invoke", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST /invoke (normalization): %v", err)
		return
	}
	defer resp.Body.Close()
	var r invokeResp
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		t.Fatalf("POST /invoke (normalization): decode: %v", err)
		return
	}
	if r.BillableUnits == nil || *r.BillableUnits < 1 {
		t.Fatalf("POST /invoke (normalization): expected billable_units >= 1 after SDK normalization, got %v", r.BillableUnits)
	}
}

// assertErrorEnvelope verifies that malformed JSON triggers the SDK's BAD_REQUEST
// structured error and that the envelope contains no success fields.
func assertErrorEnvelope(t tb, srv *httptest.Server) {
	t.Helper()
	resp, err := http.Post(srv.URL+"/invoke", "application/json", bytes.NewReader([]byte(`{not valid json}`)))
	if err != nil {
		t.Fatalf("POST /invoke (error envelope): %v", err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /invoke (error envelope): expected HTTP 200, got %d", resp.StatusCode)
		return
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Fatalf("POST /invoke (error envelope): expected Content-Type application/json, got %q", ct)
		return
	}
	var r invokeResp
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		t.Fatalf("POST /invoke (error envelope): decode: %v", err)
		return
	}
	if r.Error == nil {
		t.Fatal("POST /invoke (error envelope): expected error field in envelope")
		return
	}
	if r.Error.Code == "" {
		t.Fatal("POST /invoke (error envelope): error.code must be non-empty")
	}
	if r.Error.Message == "" {
		t.Fatal("POST /invoke (error envelope): error.message must be non-empty")
	}
	if r.Error.Retryable == nil {
		t.Fatal("POST /invoke (error envelope): error.retryable must be present")
	}
	if len(r.Payload) > 0 && string(r.Payload) != "null" {
		t.Fatalf("POST /invoke (error envelope): must not contain payload, got %s", r.Payload)
	}
	if r.BillableUnits != nil {
		t.Fatalf("POST /invoke (error envelope): must not contain billable_units, got %d", *r.BillableUnits)
	}
}
