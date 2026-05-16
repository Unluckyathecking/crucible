package proxy

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
