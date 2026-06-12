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
	"mime"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	crucible "github.com/Unluckyathecking/crucible/workers/sdk-go"
)

const (
	// contentTypeJSON is the frozen contract media type for all /healthz and /invoke responses.
	contentTypeJSON = "application/json"
	// harnessClientTimeout caps each HTTP round-trip; prevents a hung httptest.Server
	// from stalling the test suite indefinitely.
	harnessClientTimeout = 5 * time.Second
	// maxResponseBytes caps the body read from any /invoke response to prevent a
	// misbehaving handler from OOM-ing the test process.
	maxResponseBytes = 10 << 20 // 10 MiB, mirrors crucible.go's invokeHandler cap
)

// harnessClient returns a fresh http.Client. DisableKeepAlives: true ensures no
// pooled connections cross between independent Harness calls. One client is created
// per Harness call and shared across all assertions in that call for efficiency.
func harnessClient() *http.Client {
	return &http.Client{
		Timeout: harnessClientTimeout,
		Transport: &http.Transport{
			DisableKeepAlives: true,
		},
	}
}

// tb is the subset of *testing.T used by all assertion helpers, including those that
// spin up their own internal fixtures (assertBillableUnitsNormalization,
// assertHandlerStructuredError). Keeping every helper on tb (rather than *testing.T)
// lets the package's own negative tests drive them through spyT without a real
// *testing.T.
type tb interface {
	Helper()
	Fatalf(format string, args ...any)
	Fatal(args ...any)
	Errorf(format string, args ...any)
	Cleanup(func())
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
	Code    string `json:"code"`
	Message string `json:"message"`
	// Retryable is a pointer so the decoder can distinguish absent (nil) from false.
	// The wire format always includes this field (crucible.Error uses bool, never omitted),
	// but we decode with a pointer to detect malformed responses that omit it.
	Retryable *bool `json:"retryable"`
}

// checkContentType returns true if the header value parses as contentTypeJSON.
// Uses mime.ParseMediaType to correctly reject media types like "application/json+evil"
// that strings.HasPrefix would incorrectly accept.
func checkContentType(header string) bool {
	mt, _, err := mime.ParseMediaType(header)
	return err == nil && mt == contentTypeJSON
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

	srvMux, err := crucible.Handler(h)
	if err != nil {
		t.Fatalf("crucible.Handler: %v", err)
	}
	srv := httptest.NewServer(srvMux)
	t.Cleanup(srv.Close)

	// One client shared across all assertions in this Harness call.
	client := harnessClient()

	assertHealthz(t, srv, client)
	assertInvokeMethodNotAllowed(t, srv, client)
	assertInvokeContract(t, srv, client)
	assertBillableUnitsNormalization(t, client)
	assertHandlerStructuredError(t, client)
	assertErrorEnvelope(t, srv, client)
}

// assertHealthz checks GET /healthz returns 200, Content-Type application/json,
// and body byte-exactly {"status":"ok"} with no extra whitespace.
func assertHealthz(t tb, srv *httptest.Server, client *http.Client) {
	t.Helper()
	resp, err := client.Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /healthz: expected 200, got %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !checkContentType(ct) {
		t.Fatalf("GET /healthz: expected Content-Type %s, got %q", contentTypeJSON, ct)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		t.Fatalf("GET /healthz: read body: %v", err)
	}
	// Byte-exact match: the frozen contract requires exactly {"status":"ok"} with no
	// surrounding whitespace. TrimSpace would weaken this by accepting trailing newlines.
	if string(body) != `{"status":"ok"}` {
		t.Fatalf("GET /healthz: expected {\"status\":\"ok\"}, got %q", body)
	}
}

// assertInvokeMethodNotAllowed checks that all non-POST methods on /invoke are rejected
// with 405 Method Not Allowed (invokeHandler enforces POST-only per the frozen contract).
func assertInvokeMethodNotAllowed(t tb, srv *httptest.Server, client *http.Client) {
	t.Helper()
	for _, method := range []string{http.MethodGet, http.MethodHead, http.MethodPut, http.MethodDelete, http.MethodPatch, http.MethodOptions} {
		req, err := http.NewRequest(method, srv.URL+"/invoke", nil)
		if err != nil {
			t.Fatalf("%s /invoke: build request: %v", method, err)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("%s /invoke: %v", method, err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusMethodNotAllowed {
			t.Fatalf("%s /invoke: expected 405 Method Not Allowed, got %d", method, resp.StatusCode)
		}
	}
}

// assertInvokeContract checks POST /invoke with a valid envelope returns 200 with a
// valid shape: either success (payload present, billable_units >= 1) or a structured
// error envelope, never both, never billable_units < 1 on success.
func assertInvokeContract(t tb, srv *httptest.Server, client *http.Client) {
	t.Helper()
	reqBody, err := json.Marshal(map[string]any{
		"request_id": "conformance-1",
		"operation":  "conformance",
		"payload":    map[string]string{"hello": "world"},
	})
	if err != nil {
		t.Fatalf("marshal invoke request: %v", err)
	}
	resp, err := client.Post(srv.URL+"/invoke", contentTypeJSON, bytes.NewReader(reqBody))
	if err != nil {
		t.Fatalf("POST /invoke: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /invoke: expected 200, got %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !checkContentType(ct) {
		t.Fatalf("POST /invoke: expected Content-Type %s, got %q", contentTypeJSON, ct)
	}
	var r invokeResp
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponseBytes)).Decode(&r); err != nil {
		t.Fatalf("POST /invoke: decode: %v", err)
	}

	hasPayload := r.Payload != nil && !bytes.Equal(r.Payload, []byte("null"))
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
		if r.BillableUnits != nil {
			t.Fatalf("POST /invoke error: must not contain billable_units, got %d", *r.BillableUnits)
		}
	}
}

// checkNormalizationResponse verifies that the /invoke response at srv carries
// billable_units >= 1. Used by assertBillableUnitsNormalization (with an internal
// SDK-wrapped fixture) and by the package's own tests (with a raw fixture that
// bypasses normalization, proving the assertion detects the violation).
func checkNormalizationResponse(t tb, srv *httptest.Server, client *http.Client) {
	t.Helper()
	reqBody, err := json.Marshal(map[string]any{"operation": "norm"})
	if err != nil {
		t.Fatalf("marshal normalization request: %v", err)
	}
	resp, err := client.Post(srv.URL+"/invoke", contentTypeJSON, bytes.NewReader(reqBody))
	if err != nil {
		t.Fatalf("POST /invoke (normalization): %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /invoke (normalization): expected 200, got %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !checkContentType(ct) {
		t.Fatalf("POST /invoke (normalization): expected Content-Type %s, got %q", contentTypeJSON, ct)
	}
	var r invokeResp
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponseBytes)).Decode(&r); err != nil {
		t.Fatalf("POST /invoke (normalization): decode: %v", err)
	}
	if r.BillableUnits == nil || *r.BillableUnits < 1 {
		t.Fatalf("POST /invoke (normalization): expected billable_units >= 1, got %v", r.BillableUnits)
	}
}

// assertBillableUnitsNormalization verifies that crucible.Handler normalizes a zero
// BillableUnits to >= 1 (mirrors invokeHandler + the gateway trust boundary).
// Uses an internal fixture handler to exercise the SDK normalization path directly.
// Accepts tb so the package's own negative tests can drive it through spyT.
func assertBillableUnitsNormalization(t tb, client *http.Client) {
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
	checkNormalizationResponse(t, normSrv, client)
}

// assertHandlerStructuredError verifies that the SDK correctly serializes a *crucible.Error
// into the structured error envelope. It intentionally uses a synthetic errH fixture rather
// than the caller's handler because conformance tests what the SDK does with the error, not
// whether the caller's handler ever returns one (which is handler-specific business logic).
// It creates and tears down its own httptest.Server so Harness has no cross-assertion
// server dependencies. Accepts tb so the package's own negative tests can drive it through spyT.
func assertHandlerStructuredError(t tb, client *http.Client) {
	t.Helper()
	errH := func(_ context.Context, _ crucible.Request) (crucible.Response, error) {
		return crucible.Response{}, &crucible.Error{Code: "HANDLER_ERR", Message: "handler-returned error", Retryable: true}
	}
	errMux, err := crucible.Handler(errH)
	if err != nil {
		t.Fatalf("crucible.Handler(errH): %v", err)
	}
	errSrv := httptest.NewServer(errMux)
	t.Cleanup(errSrv.Close)

	reqBody, err := json.Marshal(map[string]any{"operation": "err"})
	if err != nil {
		t.Fatalf("marshal error-handler request: %v", err)
	}
	checkErrorEnvelopeAt(t, errSrv, client, reqBody, "HANDLER_ERR")
}

// assertErrorEnvelope verifies that malformed JSON triggers the SDK's BAD_REQUEST
// structured error and that the envelope contains no success fields.
func assertErrorEnvelope(t tb, srv *httptest.Server, client *http.Client) {
	t.Helper()
	checkErrorEnvelopeAt(t, srv, client, []byte(`{not valid json}`), "BAD_REQUEST")
}

// checkErrorEnvelopeAt posts body to srv's /invoke and asserts the response is a valid
// structured error envelope: error.code and error.message non-empty, error.retryable
// present, no payload, no billable_units. If wantCode is non-empty the response code
// must match it exactly.
func checkErrorEnvelopeAt(t tb, srv *httptest.Server, client *http.Client, body []byte, wantCode string) {
	t.Helper()
	resp, err := client.Post(srv.URL+"/invoke", contentTypeJSON, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST /invoke (error envelope): %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /invoke (error envelope): expected HTTP 200, got %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !checkContentType(ct) {
		t.Fatalf("POST /invoke (error envelope): expected Content-Type %s, got %q", contentTypeJSON, ct)
	}
	var r invokeResp
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponseBytes)).Decode(&r); err != nil {
		t.Fatalf("POST /invoke (error envelope): decode: %v", err)
	}
	if r.Error == nil {
		t.Fatal("POST /invoke (error envelope): expected error field in envelope")
	}
	if r.Error.Code == "" {
		t.Fatal("POST /invoke (error envelope): error.code must be non-empty")
	}
	if wantCode != "" && r.Error.Code != wantCode {
		t.Fatalf("POST /invoke (error envelope): expected error.code %q, got %q", wantCode, r.Error.Code)
	}
	if r.Error.Message == "" {
		t.Fatal("POST /invoke (error envelope): error.message must be non-empty")
	}
	if r.Error.Retryable == nil {
		t.Fatal("POST /invoke (error envelope): error.retryable must be present")
	}
	if r.Payload != nil && !bytes.Equal(r.Payload, []byte("null")) {
		t.Fatalf("POST /invoke (error envelope): must not contain payload, got %s", r.Payload)
	}
	if r.BillableUnits != nil {
		t.Fatalf("POST /invoke (error envelope): must not contain billable_units, got %d", *r.BillableUnits)
	}
}
