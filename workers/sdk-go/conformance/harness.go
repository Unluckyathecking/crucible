// Package conformance provides an in-process harness that asserts the frozen
// HTTP/JSON worker contract (derived from gateway/proto/tool.proto). Product
// workers call Harness from their go test suite to verify their handler
// satisfies the contract without binding a real port or spawning a subprocess.
package conformance

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	crucible "github.com/Unluckyathecking/crucible/workers/sdk-go"
)

// httpClient is shared across all assertion helpers. The 5-second timeout
// prevents a hung httptest.Server from stalling the test suite indefinitely.
var httpClient = &http.Client{Timeout: 5 * time.Second}

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
// BillableUnits and Error are pointers so the decoder can distinguish an absent field
// (nil) from a zero/false value, matching the proto oneof result semantics.
type invokeResp struct {
	Payload       json.RawMessage `json:"payload"`
	BillableUnits *uint64         `json:"billable_units"`
	Error         *invokeError    `json:"error"`
}

type invokeError struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	// Retryable is a pointer so the decoder can distinguish absent (nil) from false.
	// The wire format always includes this field (crucible.Error uses bool, never omitted),
	// but we decode with a pointer to detect malformed responses that omit it.
	Retryable *bool `json:"retryable"`
}

// Harness drives crucible.Handler(h) in-process via httptest.NewServer and asserts
// the frozen tool.proto HTTP/JSON contract. Call from a product worker's go test:
//
//	func TestConformance(t *testing.T) { conformance.Harness(t, myHandler) }
//
// Harness is not safe to call concurrently from multiple goroutines with the same t
// because *testing.T is not goroutine-safe (its internal state races).
func Harness(t *testing.T, h crucible.HandlerFunc) {
	t.Helper()
	if h == nil {
		t.Fatalf("conformance.Harness: nil HandlerFunc")
	}

	srvMux, err := crucible.Handler(h)
	if err != nil {
		t.Fatalf("crucible.Handler: %v", err)
	}
	srv := httptest.NewServer(srvMux)
	t.Cleanup(srv.Close)

	assertHealthz(t, srv)
	assertInvokeContract(t, srv)
	assertBillableUnitsNormalization(t)
	assertHandlerStructuredError(t)
	assertErrorEnvelope(t, srv)
}

// assertHealthz checks GET /healthz returns 200 and body byte-exactly {"status":"ok"}.
func assertHealthz(t tb, srv *httptest.Server) {
	t.Helper()
	resp, err := httpClient.Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /healthz: expected 200, got %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("GET /healthz: expected Content-Type application/json, got %q", ct)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("GET /healthz: read body: %v", err)
	}
	if got := strings.TrimSpace(string(body)); got != `{"status":"ok"}` {
		t.Fatalf("GET /healthz: expected {\"status\":\"ok\"}, got %q", got)
	}
}

// assertInvokeContract checks POST /invoke with a valid envelope returns 200 with a
// valid shape: either success (payload present, billable_units >= 1) or a structured
// error envelope, never both, never billable_units < 1 on success.
func assertInvokeContract(t tb, srv *httptest.Server) {
	t.Helper()
	reqBody, err := json.Marshal(map[string]any{
		"request_id": "conformance-1",
		"operation":  "conformance",
		"payload":    map[string]string{"hello": "world"},
	})
	if err != nil {
		t.Fatalf("marshal invoke request: %v", err)
	}
	resp, err := httpClient.Post(srv.URL+"/invoke", "application/json", bytes.NewReader(reqBody))
	if err != nil {
		t.Fatalf("POST /invoke: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /invoke: expected 200, got %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("POST /invoke: expected Content-Type application/json, got %q", ct)
	}
	var r invokeResp
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		t.Fatalf("POST /invoke: decode: %v", err)
	}

	hasPayload := len(r.Payload) > 0 && !bytes.Equal(r.Payload, []byte("null"))
	hasError := r.Error != nil

	if hasPayload && hasError {
		t.Fatal("POST /invoke: response must not contain both payload and error")
	}
	if !hasPayload && !hasError {
		t.Fatal("POST /invoke: response must contain payload+billable_units or error")
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

// checkNormalizationResponse verifies that the /invoke response at srv carries
// billable_units >= 1. Used by assertBillableUnitsNormalization (with an internal
// SDK-wrapped fixture) and by the package's own tests (with a raw fixture that
// bypasses normalization, proving the assertion detects the violation).
func checkNormalizationResponse(t tb, srv *httptest.Server) {
	t.Helper()
	reqBody, err := json.Marshal(map[string]any{"operation": "norm"})
	if err != nil {
		t.Fatalf("marshal normalization request: %v", err)
	}
	resp, err := httpClient.Post(srv.URL+"/invoke", "application/json", bytes.NewReader(reqBody))
	if err != nil {
		t.Fatalf("POST /invoke (normalization): %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /invoke (normalization): expected 200, got %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("POST /invoke (normalization): expected Content-Type application/json, got %q", ct)
	}
	var r invokeResp
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		t.Fatalf("POST /invoke (normalization): decode: %v", err)
	}
	if r.BillableUnits == nil || *r.BillableUnits < 1 {
		t.Fatalf("POST /invoke (normalization): expected billable_units >= 1, got %v", r.BillableUnits)
	}
}

// assertBillableUnitsNormalization verifies that crucible.Handler normalizes a zero
// BillableUnits to >= 1 (mirrors invokeHandler + the gateway trust boundary).
// Uses an internal fixture handler to exercise the SDK normalization path directly.
func assertBillableUnitsNormalization(t *testing.T) {
	t.Helper()
	// Zero-units fixture: explicitly returns 0 to exercise the SDK's normalization guard.
	zeroH := func(_ context.Context, _ crucible.Request) (crucible.Response, error) {
		return crucible.Response{Payload: map[string]string{"norm": "ok"}, BillableUnits: 0}, nil
	}
	normMux, err := crucible.Handler(zeroH)
	if err != nil {
		t.Fatalf("crucible.Handler(zeroH): %v", err)
	}
	normSrv := httptest.NewServer(normMux)
	t.Cleanup(normSrv.Close)
	checkNormalizationResponse(t, normSrv)
}

// assertHandlerStructuredError verifies that a handler returning *crucible.Error produces
// the correct structured error envelope and no success fields. It creates and tears down
// its own httptest.Server so Harness has no cross-assertion server dependencies.
func assertHandlerStructuredError(t tb) {
	t.Helper()
	errH := func(_ context.Context, _ crucible.Request) (crucible.Response, error) {
		return crucible.Response{}, &crucible.Error{Code: "HANDLER_ERR", Message: "handler-returned error", Retryable: true}
	}
	errMux, err := crucible.Handler(errH)
	if err != nil {
		t.Fatalf("crucible.Handler(errH): %v", err)
	}
	errSrv := httptest.NewServer(errMux)
	defer errSrv.Close()

	reqBody, err := json.Marshal(map[string]any{"operation": "err"})
	if err != nil {
		t.Fatalf("marshal error-handler request: %v", err)
	}
	checkErrorEnvelopeAt(t, errSrv, reqBody)
}

// assertErrorEnvelope verifies that malformed JSON triggers the SDK's BAD_REQUEST
// structured error and that the envelope contains no success fields.
func assertErrorEnvelope(t tb, srv *httptest.Server) {
	t.Helper()
	checkErrorEnvelopeAt(t, srv, []byte(`{not valid json}`))
}

// checkErrorEnvelopeAt posts body to srv's /invoke and asserts the response is a valid
// structured error envelope: error.code and error.message non-empty, error.retryable
// present, no payload, no billable_units.
func checkErrorEnvelopeAt(t tb, srv *httptest.Server, body []byte) {
	t.Helper()
	resp, err := httpClient.Post(srv.URL+"/invoke", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST /invoke (error envelope): %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /invoke (error envelope): expected HTTP 200, got %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("POST /invoke (error envelope): expected Content-Type application/json, got %q", ct)
	}
	var r invokeResp
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		t.Fatalf("POST /invoke (error envelope): decode: %v", err)
	}
	if r.Error == nil {
		t.Fatal("POST /invoke (error envelope): expected error field in envelope")
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
	if len(r.Payload) > 0 && !bytes.Equal(r.Payload, []byte("null")) {
		t.Fatalf("POST /invoke (error envelope): must not contain payload, got %s", r.Payload)
	}
	if r.BillableUnits != nil {
		t.Fatalf("POST /invoke (error envelope): must not contain billable_units, got %d", *r.BillableUnits)
	}
}
