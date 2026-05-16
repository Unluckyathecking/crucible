package proxy

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestInvoke_Success(t *testing.T) {
	worker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/invoke" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"payload":{"ok":true},"billable_units":7,"units_label":"pages"}`))
	}))
	defer worker.Close()

	c := New(worker.URL, 5*time.Second)
	resp, err := c.Invoke(context.Background(), &InvokeRequest{
		RequestID: "req_x",
		Operation: "echo",
		Payload:   json.RawMessage(`{"in":1}`),
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if resp.BillableUnits != 7 {
		t.Errorf("billable_units = %d, want 7", resp.BillableUnits)
	}
	if resp.UnitsLabel != "pages" {
		t.Errorf("units_label = %q, want pages", resp.UnitsLabel)
	}
	if resp.Error != nil {
		t.Errorf("unexpected error: %+v", resp.Error)
	}
}

func TestInvoke_WorkerError(t *testing.T) {
	worker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"error":{"code":"BAD_INPUT","message":"bad","retryable":false},"billable_units":1}`))
	}))
	defer worker.Close()

	c := New(worker.URL, 5*time.Second)
	resp, err := c.Invoke(context.Background(), &InvokeRequest{Operation: "x"})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if resp.Error == nil {
		t.Fatal("expected structured error")
	}
	if resp.Error.Code != "BAD_INPUT" {
		t.Errorf("code = %q, want BAD_INPUT", resp.Error.Code)
	}
}

func TestInvoke_Non200(t *testing.T) {
	worker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`worker exploded`))
	}))
	defer worker.Close()

	c := New(worker.URL, 5*time.Second)
	_, err := c.Invoke(context.Background(), &InvokeRequest{Operation: "x"})
	if err == nil {
		t.Fatal("expected error for non-200, got nil")
	}
	// The body should surface in the error message so operators can triage worker failures
	// without having to attach a debugger.
	if !strings.Contains(err.Error(), "worker exploded") {
		t.Errorf("error %q did not include worker response body", err.Error())
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("error %q did not include status code", err.Error())
	}
}

func TestInvoke_Non200_BodyTruncated(t *testing.T) {
	bigBody := strings.Repeat("x", 10000)
	worker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(bigBody))
	}))
	defer worker.Close()

	c := New(worker.URL, 5*time.Second)
	_, err := c.Invoke(context.Background(), &InvokeRequest{Operation: "x"})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	// The body peek caps at 512 bytes so a chatty worker can't blow up log lines.
	if len(err.Error()) > 700 {
		t.Errorf("error too long (%d bytes); body should be truncated to ~512", len(err.Error()))
	}
}

func TestInvoke_MalformedShape(t *testing.T) {
	worker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"billable_units":1}`)) // neither payload nor error
	}))
	defer worker.Close()

	c := New(worker.URL, 5*time.Second)
	_, err := c.Invoke(context.Background(), &InvokeRequest{Operation: "x"})
	if err == nil {
		t.Fatal("expected error for malformed response, got nil")
	}
}
