package crucible

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
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
	})

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
	})

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
	h := invokeHandler(handler)

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
	h := invokeHandler(handler)

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
	h := invokeHandler(handler)

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
	h := invokeHandler(handler)

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
	h := invokeHandler(handler)

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
