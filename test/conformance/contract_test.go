// Package conformance verifies the frozen worker HTTP contract:
//
//	GET  /healthz → 200, body exactly {"status":"ok"}
//	POST /invoke  → 200, success envelope OR error envelope, never both
//
// Run against any worker by setting WORKER_URL, e.g.:
//
//	WORKER_URL=http://127.0.0.1:8081 go test -v ./...
package conformance

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
)

var workerURL string

func TestMain(m *testing.M) {
	workerURL = os.Getenv("WORKER_URL")
	if workerURL == "" {
		fmt.Fprintln(os.Stderr, "WORKER_URL env var required (e.g. http://127.0.0.1:8081)")
		os.Exit(1)
	}
	os.Exit(m.Run())
}

// invokeResp mirrors the two possible /invoke response shapes.
// Pointer fields are nil when the key is absent, distinguishing absence from zero value.
type invokeResp struct {
	Payload       json.RawMessage `json:"payload"`
	BillableUnits *uint64         `json:"billable_units"`
	UnitsLabel    string          `json:"units_label,omitempty"`
	Error         *invokeError    `json:"error"`
}

type invokeError struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	Retryable *bool  `json:"retryable"`
}

// TestHealthz asserts GET /healthz returns 200 with body exactly {"status":"ok"}.
func TestHealthz(t *testing.T) {
	resp, err := http.Get(workerURL + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	var buf bytes.Buffer
	if _, err := buf.ReadFrom(resp.Body); err != nil {
		t.Fatalf("read body: %v", err)
	}
	got := strings.TrimSpace(buf.String())
	if got != `{"status":"ok"}` {
		t.Fatalf("expected body {\"status\":\"ok\"}, got %q", got)
	}
}

// TestInvokeSuccess asserts a well-formed request returns a success envelope:
// payload present, billable_units >= 1, no error field.
func TestInvokeSuccess(t *testing.T) {
	r := doInvoke(t, map[string]any{
		"operation": "echo",
		"payload":   map[string]string{"hello": "world"},
	})

	if r.Error != nil {
		t.Fatalf("unexpected error field in success response: code=%s message=%s", r.Error.Code, r.Error.Message)
	}
	if len(r.Payload) == 0 || string(r.Payload) == "null" {
		t.Fatal("expected non-null payload in success response")
	}
	if r.BillableUnits == nil || *r.BillableUnits < 1 {
		t.Fatalf("expected billable_units >= 1, got %v", r.BillableUnits)
	}
}

// TestInvokeBillableUnitsNormalization asserts that a request without an explicit
// units hint still yields billable_units >= 1 on success.
func TestInvokeBillableUnitsNormalization(t *testing.T) {
	r := doInvoke(t, map[string]any{
		"operation": "echo",
		"payload":   nil,
	})

	if r.Error != nil {
		t.Fatalf("unexpected error: %+v", r.Error)
	}
	if r.BillableUnits == nil || *r.BillableUnits < 1 {
		t.Fatalf("expected billable_units >= 1 after normalisation, got %v", r.BillableUnits)
	}
}

// TestInvokeErrorEnvelope asserts that an error condition returns HTTP 200 with
// {"error":{"code":..., "message":..., "retryable":...}} and no payload or billable_units.
func TestInvokeErrorEnvelope(t *testing.T) {
	// Malformed JSON triggers the SDK's BAD_REQUEST structured error, exercising the
	// full error-envelope path without requiring a stub modification.
	raw := []byte(`{not valid json}`)
	resp, err := http.Post(workerURL+"/invoke", "application/json", bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("POST /invoke: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("error envelope must be HTTP 200, got %d", resp.StatusCode)
	}

	var r invokeResp
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		t.Fatalf("decode error envelope: %v", err)
	}

	if r.Error == nil {
		t.Fatal("expected error field in error envelope, got none")
	}
	if r.Error.Code == "" {
		t.Fatal("error.code must be non-empty")
	}
	if r.Error.Message == "" {
		t.Fatal("error.message must be non-empty")
	}
	// retryable is a required boolean; *bool distinguishes absent from false.
	if r.Error.Retryable == nil {
		t.Fatal("error.retryable must be present (true or false)")
	}

	// Error envelope must not contain success fields.
	if len(r.Payload) > 0 && string(r.Payload) != "null" {
		t.Fatalf("error envelope must not contain payload, got: %s", r.Payload)
	}
	if r.BillableUnits != nil {
		t.Fatalf("error envelope must not contain billable_units, got %d", *r.BillableUnits)
	}
}

// doInvoke is a shared helper: POST /invoke, assert HTTP 200, decode and return the response.
func doInvoke(t *testing.T, body any) invokeResp {
	t.Helper()
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	resp, err := http.Post(workerURL+"/invoke", "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatalf("POST /invoke: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected HTTP 200 from /invoke, got %d", resp.StatusCode)
	}
	var r invokeResp
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		t.Fatalf("decode /invoke response: %v", err)
	}
	return r
}
