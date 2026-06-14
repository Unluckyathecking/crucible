package crucible

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"
)

// invokeURL is the endpoint path used by all /invoke tests.
const invokeURL = "/invoke"

// newInvokeRequest builds a POST /invoke request with the given JSON body.
func newInvokeRequest(t *testing.T, body []byte) *http.Request {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, invokeURL, bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	return r
}

// decodeResponse is a convenience helper that decodes the response body into m.
func decodeResponse(t *testing.T, w *httptest.ResponseRecorder, m *map[string]any) {
	t.Helper()
	if err := json.NewDecoder(w.Body).Decode(m); err != nil {
		t.Fatalf("decode response: %v", err)
	}
}

// signBody computes X-Worker-Signature for the given body using the provided secret.
func signBody(t *testing.T, secret string, body []byte) string {
	t.Helper()
	ts := strconv.FormatInt(time.Now().UTC().Unix(), 10)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(ts))
	mac.Write([]byte("."))
	mac.Write(body)
	return "t=" + ts + ",v1=" + hex.EncodeToString(mac.Sum(nil))
}

// --- healthz -----------------------------------------------------------------

func TestHealthzReturns200(t *testing.T) {
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	healthHandler(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", w.Code)
	}
	var body map[string]any
	decodeResponse(t, w, &body)
	if body["status"] != "ok" {
		t.Fatalf("want status=ok, got %v", body["status"])
	}
}

// --- invoke: routing ---------------------------------------------------------

func TestInvokeMethodNotAllowed(t *testing.T) {
	h := invokeHandler(func(_ context.Context, _ Request) (Response, error) {
		return Response{}, nil
	}, "")

	for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodDelete} {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(method, invokeURL, nil)
		h(w, r)
		if w.Code != http.StatusMethodNotAllowed {
			t.Errorf("method %s: want 405, got %d", method, w.Code)
		}
	}
}

// --- invoke: malformed body -> 400 envelope ----------------------------------

func TestInvokeMalformedBody(t *testing.T) {
	h := invokeHandler(func(_ context.Context, _ Request) (Response, error) {
		return Response{}, nil
	}, "")

	w := httptest.NewRecorder()
	r := newInvokeRequest(t, []byte(`not valid json`))
	h(w, r)

	// HTTP status must be 200 (gateway distinguishes via envelope shape).
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", w.Code)
	}

	var body map[string]any
	decodeResponse(t, w, &body)
	errVal, ok := body["error"]
	if !ok {
		t.Fatalf("want error envelope, got %v", body)
	}
	errMap, ok := errVal.(map[string]any)
	if !ok {
		t.Fatalf("error field should be a map, got %T", errVal)
	}
	if errMap["code"] != "BAD_REQUEST" {
		t.Fatalf("want BAD_REQUEST, got %v", errMap["code"])
	}
}

// --- invoke: success + billable_units normalisation --------------------------

func TestInvokeSuccessDefaultsBillableUnitsToOne(t *testing.T) {
	handler := func(_ context.Context, in Request) (Response, error) {
		return Response{Payload: map[string]string{"echo": in.Operation}}, nil
	}
	h := invokeHandler(handler, "")

	payload, _ := json.Marshal(Request{RequestID: "r1", Operation: "echo"})
	w := httptest.NewRecorder()
	r := newInvokeRequest(t, payload)
	h(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", w.Code)
	}

	var body map[string]any
	decodeResponse(t, w, &body)

	// Must not carry an error envelope.
	if _, hasErr := body["error"]; hasErr {
		t.Fatalf("unexpected error envelope: %v", body)
	}

	units, ok := body["billable_units"].(float64)
	if !ok {
		t.Fatalf("billable_units missing or not numeric: %v", body)
	}
	if units < 1 {
		t.Fatalf("want billable_units>=1, got %v", units)
	}
}

func TestInvokeSuccessExplicitBillableUnitsPreserved(t *testing.T) {
	const wantUnits = uint64(5)
	handler := func(_ context.Context, _ Request) (Response, error) {
		return Response{Payload: "ok", BillableUnits: wantUnits}, nil
	}
	h := invokeHandler(handler, "")

	payload, _ := json.Marshal(Request{RequestID: "r2"})
	w := httptest.NewRecorder()
	r := newInvokeRequest(t, payload)
	h(w, r)

	var body map[string]any
	decodeResponse(t, w, &body)

	units := body["billable_units"].(float64)
	if uint64(units) != wantUnits {
		t.Fatalf("want %d billable_units, got %v", wantUnits, units)
	}
}

// --- invoke: handler errors --------------------------------------------------

func TestInvokeHandlerStructuredError(t *testing.T) {
	serr := &Error{Code: "NOT_FOUND", Message: "thing not found", Retryable: false}
	handler := func(_ context.Context, _ Request) (Response, error) {
		return Response{}, serr
	}
	h := invokeHandler(handler, "")

	payload, _ := json.Marshal(Request{RequestID: "r3"})
	w := httptest.NewRecorder()
	r := newInvokeRequest(t, payload)
	h(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", w.Code)
	}

	var body map[string]any
	decodeResponse(t, w, &body)

	errMap, ok := body["error"].(map[string]any)
	if !ok {
		t.Fatalf("want error envelope, got %v", body)
	}
	if errMap["code"] != "NOT_FOUND" {
		t.Fatalf("want NOT_FOUND, got %v", errMap["code"])
	}
	if errMap["retryable"] != false {
		t.Fatalf("want retryable=false, got %v", errMap["retryable"])
	}
}

func TestInvokeHandlerUnstructuredErrorBecomesInternal(t *testing.T) {
	handler := func(_ context.Context, _ Request) (Response, error) {
		return Response{}, errors.New("database exploded")
	}
	h := invokeHandler(handler, "")

	payload, _ := json.Marshal(Request{RequestID: "r4"})
	w := httptest.NewRecorder()
	r := newInvokeRequest(t, payload)
	h(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", w.Code)
	}

	var body map[string]any
	decodeResponse(t, w, &body)

	errMap, ok := body["error"].(map[string]any)
	if !ok {
		t.Fatalf("want error envelope, got %v", body)
	}
	if errMap["code"] != "INTERNAL" {
		t.Fatalf("want INTERNAL, got %v", errMap["code"])
	}
	// Internal error message must not leak the original cause.
	if errMap["message"] == "database exploded" {
		t.Fatal("internal error must not leak original cause")
	}
}

// TestInvokeErrorWrappedStructuredError verifies that a *Error returned via
// fmt.Errorf("%w") is still surfaced as a structured error (not an INTERNAL).
func TestInvokeHandlerWrappedStructuredError(t *testing.T) {
	inner := &Error{Code: "QUOTA_EXCEEDED", Message: "quota hit", Retryable: false}
	handler := func(_ context.Context, _ Request) (Response, error) {
		return Response{}, fmt.Errorf("quota check: %w", inner)
	}
	h := invokeHandler(handler, "")

	payload, _ := json.Marshal(Request{RequestID: "r5"})
	w := httptest.NewRecorder()
	r := newInvokeRequest(t, payload)
	h(w, r)

	var body map[string]any
	decodeResponse(t, w, &body)

	errMap, ok := body["error"].(map[string]any)
	if !ok {
		t.Fatalf("want error envelope, got %v", body)
	}
	if errMap["code"] != "QUOTA_EXCEEDED" {
		t.Fatalf("want QUOTA_EXCEEDED, got %v", errMap["code"])
	}
}

// --- Error type --------------------------------------------------------------

func TestErrorString(t *testing.T) {
	e := &Error{Code: "FOO", Message: "bar"}
	want := "FOO: bar"
	if e.Error() != want {
		t.Fatalf("want %q, got %q", want, e.Error())
	}
}

// --- invoke: HMAC-SHA256 channel-auth (X-Worker-Signature) ------------------
// Test matrix mirrors billing/webhook_test.go: valid, missing, wrong-secret,
// tampered-body, stale-timestamp, and the disabled-path (empty secret).

func TestInvokeSignature_ValidSignatureAccepted(t *testing.T) {
	const secret = "test-shared-secret-valid"
	h := invokeHandler(func(_ context.Context, _ Request) (Response, error) {
		return Response{Payload: "ok"}, nil
	}, secret)

	body, _ := json.Marshal(Request{RequestID: "sig1"})
	w := httptest.NewRecorder()
	r := newInvokeRequest(t, body)
	r.Header.Set(workerSigHeader, signBody(t, secret, body))
	h(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", w.Code)
	}
	var resp map[string]any
	decodeResponse(t, w, &resp)
	if _, hasErr := resp["error"]; hasErr {
		t.Fatalf("expected success, got error envelope: %v", resp["error"])
	}
}

func TestInvokeSignature_MissingSignatureRejected(t *testing.T) {
	h := invokeHandler(func(_ context.Context, _ Request) (Response, error) {
		return Response{Payload: "ok"}, nil
	}, "test-shared-secret-missing")

	body, _ := json.Marshal(Request{RequestID: "sig2"})
	w := httptest.NewRecorder()
	r := newInvokeRequest(t, body)
	// deliberately no X-Worker-Signature header
	h(w, r)

	var resp map[string]any
	decodeResponse(t, w, &resp)
	errMap, ok := resp["error"].(map[string]any)
	if !ok {
		t.Fatalf("expected error envelope, got %v", resp)
	}
	if errMap["code"] != "UNAUTHORIZED" {
		t.Errorf("code = %v, want UNAUTHORIZED", errMap["code"])
	}
}

func TestInvokeSignature_WrongSecretRejected(t *testing.T) {
	h := invokeHandler(func(_ context.Context, _ Request) (Response, error) {
		return Response{Payload: "ok"}, nil
	}, "correct-secret")

	body, _ := json.Marshal(Request{RequestID: "sig3"})
	w := httptest.NewRecorder()
	r := newInvokeRequest(t, body)
	r.Header.Set(workerSigHeader, signBody(t, "wrong-secret", body))
	h(w, r)

	var resp map[string]any
	decodeResponse(t, w, &resp)
	errMap, ok := resp["error"].(map[string]any)
	if !ok {
		t.Fatalf("expected error envelope, got %v", resp)
	}
	if errMap["code"] != "UNAUTHORIZED" {
		t.Errorf("code = %v, want UNAUTHORIZED", errMap["code"])
	}
}

func TestInvokeSignature_TamperedBodyRejected(t *testing.T) {
	const secret = "test-shared-secret-tampered"
	h := invokeHandler(func(_ context.Context, _ Request) (Response, error) {
		return Response{Payload: "ok"}, nil
	}, secret)

	originalBody, _ := json.Marshal(Request{RequestID: "sig4"})
	tamperedBody, _ := json.Marshal(Request{RequestID: "TAMPERED"})

	// Sign the original body but send the tampered body — HMAC should fail.
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, invokeURL, bytes.NewReader(tamperedBody))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set(workerSigHeader, signBody(t, secret, originalBody))
	h(w, r)

	var resp map[string]any
	decodeResponse(t, w, &resp)
	errMap, ok := resp["error"].(map[string]any)
	if !ok {
		t.Fatalf("expected error envelope, got %v", resp)
	}
	if errMap["code"] != "UNAUTHORIZED" {
		t.Errorf("code = %v, want UNAUTHORIZED", errMap["code"])
	}
}

func TestInvokeSignature_StaleTimestampRejected(t *testing.T) {
	const secret = "test-shared-secret-stale"
	h := invokeHandler(func(_ context.Context, _ Request) (Response, error) {
		return Response{Payload: "ok"}, nil
	}, secret)

	body, _ := json.Marshal(Request{RequestID: "sig5"})

	// Build a signature with a timestamp 10 minutes in the past — outside the 5-minute window.
	staleTS := strconv.FormatInt(time.Now().UTC().Add(-10*time.Minute).Unix(), 10)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(staleTS))
	mac.Write([]byte("."))
	mac.Write(body)
	staleSig := "t=" + staleTS + ",v1=" + hex.EncodeToString(mac.Sum(nil))

	w := httptest.NewRecorder()
	r := newInvokeRequest(t, body)
	r.Header.Set(workerSigHeader, staleSig)
	h(w, r)

	var resp map[string]any
	decodeResponse(t, w, &resp)
	errMap, ok := resp["error"].(map[string]any)
	if !ok {
		t.Fatalf("expected error envelope, got %v", resp)
	}
	if errMap["code"] != "UNAUTHORIZED" {
		t.Errorf("code = %v, want UNAUTHORIZED", errMap["code"])
	}
}

// TestInvokeSignature_DisabledPathSucceeds verifies that when no shared secret is
// configured, an unsigned call succeeds — opt-in only, today's behaviour preserved.
func TestInvokeSignature_DisabledPathSucceeds(t *testing.T) {
	h := invokeHandler(func(_ context.Context, _ Request) (Response, error) {
		return Response{Payload: "ok"}, nil
	}, "") // empty secret = signing disabled

	body, _ := json.Marshal(Request{RequestID: "sig6"})
	w := httptest.NewRecorder()
	r := newInvokeRequest(t, body)
	// No signature header — must succeed because secret is not configured.
	h(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", w.Code)
	}
	var resp map[string]any
	decodeResponse(t, w, &resp)
	if _, hasErr := resp["error"]; hasErr {
		t.Fatalf("expected success (signing disabled), got error envelope: %v", resp["error"])
	}
}
