package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Unluckyathecking/crucible/gateway/internal/config"
	"github.com/Unluckyathecking/crucible/gateway/internal/proxy"
)

// mockChecker implements HealthChecker for testing.
type mockChecker struct {
	pingErr error
}

func (m *mockChecker) Ping(_ context.Context) error { return m.pingErr }

func TestHealthz(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	w := httptest.NewRecorder()
	healthz(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("healthz: expected 200, got %d", w.Code)
	}

	var body map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("healthz: failed to decode body: %v", err)
	}

	if body["status"] != "ok" {
		t.Errorf("healthz: expected status ok, got %q", body["status"])
	}
}

func TestHealthzBackwardCompatible(t *testing.T) {
	// Verify no new fields were added to /healthz — it must stay the plain {"status":"ok"} shape.
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	w := httptest.NewRecorder()
	healthz(w, req)

	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("healthz: failed to decode body: %v", err)
	}

	if got := len(body); got != 1 {
		t.Errorf("healthz: expected 1 top-level field, got %d (%v)", got, body)
	}
	if _, exists := body["checks"]; exists {
		t.Error("healthz: must not contain 'checks' field — that is /readyz territory")
	}
}

func TestReadyzAllHealthy(t *testing.T) {
	healthy := &mockChecker{}
	h := readyz(healthy, healthy)

	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	w := httptest.NewRecorder()
	h(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("readyz: expected 200, got %d", w.Code)
	}

	var body readyzResponse
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("readyz: failed to decode body: %v", err)
	}

	if body.Status != "ok" {
		t.Errorf("readyz: expected status ok, got %q", body.Status)
	}
	if body.Checks["redis"] != "ok" {
		t.Errorf("readyz: expected redis ok, got %q", body.Checks["redis"])
	}
	if body.Checks["postgres"] != "ok" {
		t.Errorf("readyz: expected postgres ok, got %q", body.Checks["postgres"])
	}
}

func TestReadyzRedisDown(t *testing.T) {
	healthy := &mockChecker{}
	down := &mockChecker{pingErr: errors.New("connection refused")}
	h := readyz(down, healthy)

	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	w := httptest.NewRecorder()
	h(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("readyz: expected 200 even when degraded, got %d", w.Code)
	}

	var body readyzResponse
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("readyz: failed to decode body: %v", err)
	}

	if body.Status != "degraded" {
		t.Errorf("readyz: expected status degraded, got %q", body.Status)
	}
	if body.Checks["redis"] != "error" {
		t.Errorf("readyz: expected redis error, got %q", body.Checks["redis"])
	}
	if body.Checks["postgres"] != "ok" {
		t.Errorf("readyz: expected postgres ok, got %q", body.Checks["postgres"])
	}
}

func TestReadyzPostgresDown(t *testing.T) {
	healthy := &mockChecker{}
	down := &mockChecker{pingErr: errors.New("connection refused")}
	h := readyz(healthy, down)

	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	w := httptest.NewRecorder()
	h(w, req)

	var body readyzResponse
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("readyz: failed to decode body: %v", err)
	}

	if body.Status != "degraded" {
		t.Errorf("readyz: expected status degraded, got %q", body.Status)
	}
	if body.Checks["redis"] != "ok" {
		t.Errorf("readyz: expected redis ok, got %q", body.Checks["redis"])
	}
	if body.Checks["postgres"] != "error" {
		t.Errorf("readyz: expected postgres error, got %q", body.Checks["postgres"])
	}
}

func TestReadyzBothDown(t *testing.T) {
	down := &mockChecker{pingErr: errors.New("connection refused")}
	h := readyz(down, down)

	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	w := httptest.NewRecorder()
	h(w, req)

	var body readyzResponse
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("readyz: failed to decode body: %v", err)
	}

	if body.Status != "degraded" {
		t.Errorf("readyz: expected status degraded, got %q", body.Status)
	}
	if body.Checks["redis"] != "error" {
		t.Errorf("readyz: expected redis error, got %q", body.Checks["redis"])
	}
	if body.Checks["postgres"] != "error" {
		t.Errorf("readyz: expected postgres error, got %q", body.Checks["postgres"])
	}
}

func TestReadyzRouteRegistered(t *testing.T) {
	healthy := &mockChecker{}
	d := &Deps{
		Cfg:   &config.Config{BodyLimitBytes: 1048576},
		Redis: healthy,
		PG:    healthy,
	}
	router := NewRouter(d)

	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("readyz through router: expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var body readyzResponse
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("readyz through router: failed to decode body: %v", err)
	}
	if body.Status != "ok" {
		t.Errorf("readyz through router: expected status ok, got %q", body.Status)
	}
}

func TestReadyzDoesNotLeakInternals(t *testing.T) {
	// The response MUST NOT contain connection strings, passwords, or raw error messages.
	down := &mockChecker{pingErr: errors.New("dial tcp 10.0.0.1:5432: connect: connection refused")}
	h := readyz(down, down)

	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	w := httptest.NewRecorder()
	h(w, req)

	body := w.Body.String()

	// These patterns must be absent from the public-facing response.
	forbidden := []string{
		"connection refused",
		"tcp",
		"password",
		"secret",
		"127.0.0.1",
		"localhost",
		"postgres://",
		"redis://",
	}
	for _, f := range forbidden {
		if contains(body, f) {
			t.Errorf("readyz: response leaks internal detail %q", f)
		}
	}
}

func TestInvokeErrorExposureSanitized(t *testing.T) {
	// Sanitized mode: worker's structured error is replaced with generic "worker unavailable".
	worker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]any{
				"code":      "SENSITIVE_DETAIL",
				"message":   "leaked internal secret",
				"retryable": false,
			},
		})
	}))
	defer worker.Close()

	p := proxy.New(worker.URL, 5*time.Second)
	h := invoke(p, nil, "sanitized", "test")

	req := httptest.NewRequest(http.MethodPost, "/v1/test", strings.NewReader(`{"input":"hello"}`))
	w := httptest.NewRecorder()
	h(w, req)

	if w.Code != http.StatusBadGateway {
		t.Fatalf("expected 502, got %d", w.Code)
	}

	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("failed to decode body: %v", err)
	}

	errObj, ok := body["error"].(map[string]any)
	if !ok {
		t.Fatalf("expected error object, got %T", body["error"])
	}
	if errObj["message"] != "worker unavailable" {
		t.Errorf("expected message 'worker unavailable', got %q", errObj["message"])
	}
	if errObj["code"] != "WORKER_UNREACHABLE" {
		t.Errorf("expected code 'WORKER_UNREACHABLE', got %q", errObj["code"])
	}

	// Sanitized mode MUST NOT leak worker internals.
	bodyStr := w.Body.String()
	forbidden := []string{"SENSITIVE_DETAIL", "leaked internal secret"}
	for _, f := range forbidden {
		if contains(bodyStr, f) {
			t.Errorf("sanitized response leaks worker detail %q", f)
		}
	}
}

func TestInvokeErrorExposureFull(t *testing.T) {
	// Full mode: worker's structured error envelope is returned to the caller.
	worker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]any{
				"code":      "INVALID_INPUT",
				"message":   "field 'name' is required",
				"retryable": false,
			},
			"billable_units": 1,
		})
	}))
	defer worker.Close()

	p := proxy.New(worker.URL, 5*time.Second)
	h := invoke(p, nil, "full", "test")

	req := httptest.NewRequest(http.MethodPost, "/v1/test", strings.NewReader(`{"input":"hello"}`))
	w := httptest.NewRecorder()
	h(w, req)

	if w.Code != http.StatusBadGateway {
		t.Fatalf("expected 502, got %d", w.Code)
	}

	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("failed to decode body: %v", err)
	}

	errObj, ok := body["error"].(map[string]any)
	if !ok {
		t.Fatalf("expected error object, got %T", body["error"])
	}
	if errObj["code"] != "INVALID_INPUT" {
		t.Errorf("expected code 'INVALID_INPUT', got %q", errObj["code"])
	}
	if errObj["message"] != "field 'name' is required" {
		t.Errorf("expected message 'field \"name\" is required', got %q", errObj["message"])
	}
	if errObj["retryable"] != false {
		t.Errorf("expected retryable false, got %v", errObj["retryable"])
	}
}

func TestInvokeErrorExposureDefaultSanitized(t *testing.T) {
	// Empty or unrecognized mode should fall back to sanitized behavior.
	worker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]any{
				"code":      "DB_UNREACHABLE",
				"message":   "postgres connection pool exhausted",
				"retryable": true,
			},
		})
	}))
	defer worker.Close()

	p := proxy.New(worker.URL, 5*time.Second)
	h := invoke(p, nil, "", "test")

	req := httptest.NewRequest(http.MethodPost, "/v1/test", strings.NewReader(`{"input":"hello"}`))
	w := httptest.NewRecorder()
	h(w, req)

	if w.Code != http.StatusBadGateway {
		t.Fatalf("expected 502, got %d", w.Code)
	}

	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("failed to decode body: %v", err)
	}

	errObj := body["error"].(map[string]any)
	if errObj["message"] != "worker unavailable" {
		t.Errorf("expected message 'worker unavailable', got %q", errObj["message"])
	}

	bodyStr := w.Body.String()
	if contains(bodyStr, "DB_UNREACHABLE") || contains(bodyStr, "connection pool") {
		t.Error("default (sanitized) mode leaked worker internals")
	}
}

func TestInvokeErrorExposureTransportErrorAlwaysSanitized(t *testing.T) {
	// Transport errors (worker unreachable) are ALWAYS sanitized regardless of mode.
	worker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"boom"}`))
	}))
	defer worker.Close()

	p := proxy.New(worker.URL, 5*time.Second)
	h := invoke(p, nil, "full", "test")

	req := httptest.NewRequest(http.MethodPost, "/v1/test", strings.NewReader(`{"input":"hello"}`))
	w := httptest.NewRecorder()
	h(w, req)

	if w.Code != http.StatusBadGateway {
		t.Fatalf("expected 502, got %d", w.Code)
	}

	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("failed to decode body: %v", err)
	}

	errObj := body["error"].(map[string]any)
	// Transport errors always sanitized — worker message never exposed.
	if errObj["message"] != "worker unavailable" {
		t.Errorf("expected message 'worker unavailable', got %q", errObj["message"])
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && searchSubstring(s, substr)
}

func searchSubstring(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

func TestWriteJSONError(t *testing.T) {
	type errorEnvelope struct {
		Error struct {
			Code      string `json:"code"`
			Message   string `json:"message"`
			Retryable bool   `json:"retryable"`
		} `json:"error"`
	}

	cases := []struct {
		status    int
		code      string
		msg       string
		retryable bool
	}{
		{http.StatusBadRequest, "BAD_INPUT", "invalid payload", false},
		{http.StatusTooManyRequests, "RATE_LIMITED", "too many requests", true},
		{http.StatusServiceUnavailable, "WORKER_UNAVAILABLE", "worker unreachable", true},
	}

	for _, tc := range cases {
		w := httptest.NewRecorder()
		writeJSONError(w, tc.status, tc.code, tc.msg, tc.retryable)

		if w.Code != tc.status {
			t.Errorf("writeJSONError(%d): got HTTP status %d", tc.status, w.Code)
		}
		if ct := w.Header().Get("Content-Type"); ct != "application/json" {
			t.Errorf("writeJSONError(%d): Content-Type %q, want application/json", tc.status, ct)
		}

		var env errorEnvelope
		if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
			t.Fatalf("writeJSONError(%d): failed to decode body: %v", tc.status, err)
		}
		if env.Error.Code != tc.code {
			t.Errorf("writeJSONError(%d): error.code %q, want %q", tc.status, env.Error.Code, tc.code)
		}
		if env.Error.Message != tc.msg {
			t.Errorf("writeJSONError(%d): error.message %q, want %q", tc.status, env.Error.Message, tc.msg)
		}
		if env.Error.Retryable != tc.retryable {
			t.Errorf("writeJSONError(%d): error.retryable %v, want %v", tc.status, env.Error.Retryable, tc.retryable)
		}
	}
}
