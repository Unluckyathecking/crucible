package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/Unluckyathecking/crucible/gateway/internal/apierror"
	"github.com/Unluckyathecking/crucible/gateway/internal/auth"
	"github.com/Unluckyathecking/crucible/gateway/internal/billing"
	"github.com/Unluckyathecking/crucible/gateway/internal/config"
	"github.com/Unluckyathecking/crucible/gateway/internal/db"
	"github.com/Unluckyathecking/crucible/gateway/internal/jobs"
	mw "github.com/Unluckyathecking/crucible/gateway/internal/middleware"
	"github.com/Unluckyathecking/crucible/gateway/internal/observability"
	"github.com/Unluckyathecking/crucible/gateway/internal/openapi"
	"github.com/Unluckyathecking/crucible/gateway/internal/paging"
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
		if strings.Contains(body, f) {
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

	p := proxy.New(worker.URL, 5*time.Second, 0)
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
		if strings.Contains(bodyStr, f) {
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

	p := proxy.New(worker.URL, 5*time.Second, 0)
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

	p := proxy.New(worker.URL, 5*time.Second, 0)
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
	if strings.Contains(bodyStr, "DB_UNREACHABLE") || strings.Contains(bodyStr, "connection pool") {
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

	p := proxy.New(worker.URL, 5*time.Second, 0)
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

func TestWebhookIPRateLimited(t *testing.T) {
	// Regression for audit #11: the public /webhooks/stripe route is mounted
	// outside auth/quota gating and previously had no rate limit. A single IP
	// must be capped at 60 req/min; the 61st request returns 429 before reaching
	// the webhook handler. Each request carries no valid Stripe signature, so the
	// handler (when reached) returns 400 without touching the DB — the limiter is
	// the only thing that can produce a 429 here.
	healthy := &mockChecker{}
	d := &Deps{
		Cfg:     &config.Config{BodyLimitBytes: 1048576},
		Webhook: billing.NewWebhook("whsec_test", nil),
		Redis:   healthy,
		PG:      healthy,
	}
	router := NewRouter(d)

	const remoteAddr = "203.0.113.7:54321"
	send := func() int {
		req := httptest.NewRequest(http.MethodPost, "/webhooks/stripe", strings.NewReader(`{}`))
		req.RemoteAddr = remoteAddr
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		return w.Code
	}

	for i := 0; i < 60; i++ {
		if code := send(); code == http.StatusTooManyRequests {
			t.Fatalf("request %d was rate limited (429) before the 60/min cap", i+1)
		}
	}
	if code := send(); code != http.StatusTooManyRequests {
		t.Fatalf("61st request: expected 429, got %d", code)
	}
}

func TestWebhookRateLimitPerIP(t *testing.T) {
	// A different IP must not be penalized by another IP's traffic — the limiter
	// keys per-IP, not globally.
	healthy := &mockChecker{}
	d := &Deps{
		Cfg:     &config.Config{BodyLimitBytes: 1048576},
		Webhook: billing.NewWebhook("whsec_test", nil),
		Redis:   healthy,
		PG:      healthy,
	}
	router := NewRouter(d)

	send := func(addr string) int {
		req := httptest.NewRequest(http.MethodPost, "/webhooks/stripe", strings.NewReader(`{}`))
		req.RemoteAddr = addr
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		return w.Code
	}

	for i := 0; i < 61; i++ {
		send("198.51.100.1:1111")
	}
	if code := send("198.51.100.2:2222"); code == http.StatusTooManyRequests {
		t.Fatalf("second IP was rate limited by first IP's traffic, got %d", code)
	}
}

func TestWebhookRateLimitSpoofedHeaderCannotExceedCap(t *testing.T) {
	// Regression for the trust-boundary review on audit #11: httprate's KeyByRealIP
	// reads client-controlled headers (True-Client-IP > X-Real-IP > X-Forwarded-For >
	// RemoteAddr). Under the intended deployment, Caddy strips True-Client-IP and
	// X-Real-IP and overrides X-Forwarded-For with its observed {remote_host}, so the
	// gateway keys on a single trusted value the client cannot influence.
	//
	// The gateway also strips True-Client-IP and X-Real-IP in its own middleware
	// (defense-in-depth) so a Caddyfile mis-config cannot silently re-open the bypass.
	//
	// This test sends ALL THREE client-controlled identity headers with a FRESH value
	// on every request (simulating an attacker trying to mint a new rate-limit bucket
	// per request). RemoteAddr stays constant (Caddy's internal Docker address) and
	// X-Forwarded-For is set to the constant trusted value Caddy would emit. The
	// gateway must strip True-Client-IP and X-Real-IP before the limiter runs, so it
	// keys on the constant X-Forwarded-For — the 61st request must still return 429.
	//
	// If the stripping middleware is removed, KeyByRealIP keys on the rotating
	// True-Client-IP first; each request lands in its own bucket and the cap is never
	// reached, causing this test to fail (audit #11 re-opened).
	healthy := &mockChecker{}
	d := &Deps{
		Cfg:     &config.Config{BodyLimitBytes: 1048576},
		Webhook: billing.NewWebhook("whsec_test", nil),
		Redis:   healthy,
		PG:      healthy,
	}
	router := NewRouter(d)

	const (
		trustedXFF = "203.0.113.50" // constant value Caddy sets from {remote_host}
		caddyAddr  = "10.0.0.2:443" // constant Caddy Docker-bridge address
	)
	send := func(i int) int {
		req := httptest.NewRequest(http.MethodPost, "/webhooks/stripe", strings.NewReader(`{}`))
		// RemoteAddr is constant: in production this is Caddy's address on the Docker
		// network, identical for every request regardless of the real client.
		req.RemoteAddr = caddyAddr
		// Caddy sets X-Forwarded-For to the real client IP (constant for one attacker).
		req.Header.Set("X-Forwarded-For", trustedXFF)
		// Rotate True-Client-IP and X-Real-IP per request — an attacker's attempt to
		// mint a fresh rate-limit bucket each time. The gateway must strip these before
		// the limiter so they cannot influence the key.
		req.Header.Set("True-Client-IP", "198.51.100."+strconv.Itoa(i))
		req.Header.Set("X-Real-IP", "192.0.2."+strconv.Itoa(i%254+1))
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		return w.Code
	}

	for i := 0; i < 60; i++ {
		// Each request carries a unique True-Client-IP and X-Real-IP. Without the
		// stripping middleware, KeyByRealIP would key on True-Client-IP and each
		// request would land in its own bucket — the cap would never be reached.
		if code := send(i); code == http.StatusTooManyRequests {
			t.Fatalf("request %d rate limited before cap despite rotating spoofed headers (unexpected early 429)", i+1)
		}
	}
	// 61st request: spoofed headers rotate again, but the limiter must still see the
	// same constant X-Forwarded-For key and return 429.
	if code := send(255); code != http.StatusTooManyRequests {
		t.Fatalf("61st request: expected 429 — rotating spoofed headers must not exceed the per-IP cap, got %d", code)
	}
}

func TestInvokeDefaultExposureNeverLeaksWorkerInternals(t *testing.T) {
	// Regression for audit #20: only the explicit "full" opt-in forwards worker
	// detail. Every other value — including the unset/empty default — must surface
	// a stable code + safe message and never leak the worker's error code or
	// message. Cover empty, the configured "sanitized" default, and an unknown
	// value to lock the safe-by-default fallthrough.
	for _, mode := range []string{"", "sanitized", "unknown"} {
		t.Run("mode="+mode, func(t *testing.T) {
			worker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				_ = json.NewEncoder(w).Encode(map[string]any{
					"error": map[string]any{
						"code":      "INTERNAL_STACK",
						"message":   "panic: runtime error at /srv/worker/db.go:42",
						"retryable": false,
					},
				})
			}))
			defer worker.Close()

			p := proxy.New(worker.URL, 5*time.Second, 0)
			h := invoke(p, nil, mode, "test")

			req := httptest.NewRequest(http.MethodPost, "/v1/test", strings.NewReader(`{"input":"x"}`))
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
			if errObj["code"] != "WORKER_UNREACHABLE" {
				t.Errorf("expected stable code 'WORKER_UNREACHABLE', got %q", errObj["code"])
			}
			if errObj["message"] != "worker unavailable" {
				t.Errorf("expected safe message 'worker unavailable', got %q", errObj["message"])
			}

			bodyStr := w.Body.String()
			for _, leak := range []string{"INTERNAL_STACK", "panic", "runtime error", "/srv/worker/db.go"} {
				if strings.Contains(bodyStr, leak) {
					t.Errorf("mode %q leaked worker internal detail %q", mode, leak)
				}
			}
		})
	}
}

// TestInvokeErrorEnvelopeShape verifies the full four-field error envelope shape
// (top-level "error" key only, code/message/retryable/request_id present) on
// two invoke error paths: retryable=false (400 bad JSON body) and
// retryable=true (502 worker unreachable).
func TestInvokeErrorEnvelopeShape(t *testing.T) {
	assertShape := func(t *testing.T, w *httptest.ResponseRecorder, wantStatus int, wantRetryable bool, rid string) {
		t.Helper()
		if w.Code != wantStatus {
			t.Fatalf("expected %d, got %d", wantStatus, w.Code)
		}
		if ct := w.Header().Get("Content-Type"); ct != "application/json" {
			t.Errorf("Content-Type = %q, want application/json", ct)
		}
		if cc := w.Header().Get("Cache-Control"); cc != "no-store" {
			t.Errorf("Cache-Control = %q, want no-store", cc)
		}
		var top map[string]json.RawMessage
		if err := json.Unmarshal(w.Body.Bytes(), &top); err != nil {
			t.Fatalf("parse envelope: %v", err)
		}
		if len(top) != 1 {
			t.Errorf("envelope has %d top-level keys, want 1 (\"error\")", len(top))
		}
		errRaw, ok := top["error"]
		if !ok {
			t.Fatal("envelope missing top-level \"error\" key")
		}
		var obj struct {
			Code      string `json:"code"`
			Message   string `json:"message"`
			Retryable bool   `json:"retryable"`
			RequestID string `json:"request_id"`
		}
		dec := json.NewDecoder(bytes.NewReader(errRaw))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&obj); err != nil {
			t.Fatalf("error object has unexpected field or missing field: %v", err)
		}
		if obj.Code == "" {
			t.Error("error.code must not be empty")
		}
		if obj.Message == "" {
			t.Error("error.message must not be empty")
		}
		if obj.Retryable != wantRetryable {
			t.Errorf("error.retryable = %v, want %v", obj.Retryable, wantRetryable)
		}
		if obj.RequestID != rid {
			t.Errorf("error.request_id = %q, want %q", obj.RequestID, rid)
		}
	}

	t.Run("retryable=false (400 bad JSON body)", func(t *testing.T) {
		worker := successWorker(1, "")
		defer worker.Close()
		p := proxy.New(worker.URL, 5*time.Second, 0)
		h := invoke(p, nil, "sanitized", "echo")

		const rid = "test-rid-routes-400"
		req := httptest.NewRequest(http.MethodPost, "/v1/echo", strings.NewReader(`not-json`))
		req = req.WithContext(context.WithValue(req.Context(), mw.RequestIDKey, rid))
		w := httptest.NewRecorder()
		h(w, req)
		assertShape(t, w, http.StatusBadRequest, false, rid)
	})

	t.Run("retryable=true (502 worker unreachable)", func(t *testing.T) {
		// Close the worker before the request so the proxy gets connection refused,
		// which triggers WORKER_UNREACHABLE (retryable=true).
		worker := successWorker(1, "")
		p := proxy.New(worker.URL, 5*time.Second, 0)
		worker.Close()

		h := invoke(p, nil, "sanitized", "echo")
		const rid = "test-rid-routes-502"
		req := httptest.NewRequest(http.MethodPost, "/v1/echo", strings.NewReader(`{}`))
		req = req.WithContext(context.WithValue(req.Context(), mw.RequestIDKey, rid))
		w := httptest.NewRecorder()
		h(w, req)
		assertShape(t, w, http.StatusBadGateway, true, rid)
	})

	t.Run("full mode worker error passes through with four-field envelope", func(t *testing.T) {
		worker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"error": map[string]any{
					"code":      "INVALID_INPUT",
					"message":   "field required",
					"retryable": false,
				},
				"billable_units": 1,
			})
		}))
		defer worker.Close()
		p := proxy.New(worker.URL, 5*time.Second, 0)
		h := invoke(p, nil, "full", "echo")
		const rid = "test-rid-routes-full"
		req := httptest.NewRequest(http.MethodPost, "/v1/echo", strings.NewReader(`{}`))
		req = req.WithContext(context.WithValue(req.Context(), mw.RequestIDKey, rid))
		w := httptest.NewRecorder()
		h(w, req)
		assertShape(t, w, http.StatusBadGateway, false, rid)
		// In full mode the worker's own code must be forwarded, not WORKER_UNREACHABLE.
		var top map[string]json.RawMessage
		_ = json.Unmarshal(w.Body.Bytes(), &top)
		var obj struct {
			Code string `json:"code"`
		}
		_ = json.Unmarshal(top["error"], &obj)
		if obj.Code != "INVALID_INPUT" {
			t.Errorf("full mode: error.code = %q, want INVALID_INPUT", obj.Code)
		}
	})

	t.Run("full mode empty worker error code falls back to WORKER_BAD_RESPONSE", func(t *testing.T) {
		// Full mode guards against empty Code from non-SDK workers: an empty code
		// is not correlatable. UNKNOWN is reserved for Prometheus metric labels only;
		// WORKER_BAD_RESPONSE is the correct customer-facing fallback.
		worker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"error": map[string]any{
					"code":      "",
					"message":   "malformed error from worker",
					"retryable": false,
				},
				"billable_units": 1,
			})
		}))
		defer worker.Close()
		p := proxy.New(worker.URL, 5*time.Second, 0)
		h := invoke(p, nil, "full", "echo")
		const rid = "test-rid-routes-bad-code"
		req := httptest.NewRequest(http.MethodPost, "/v1/echo", strings.NewReader(`{}`))
		req = req.WithContext(context.WithValue(req.Context(), mw.RequestIDKey, rid))
		w := httptest.NewRecorder()
		h(w, req)
		if w.Code != http.StatusBadGateway {
			t.Fatalf("status = %d, want %d", w.Code, http.StatusBadGateway)
		}
		var top map[string]json.RawMessage
		if err := json.Unmarshal(w.Body.Bytes(), &top); err != nil {
			t.Fatalf("parse body: %v", err)
		}
		var obj struct {
			Code      string `json:"code"`
			RequestID string `json:"request_id"`
		}
		if err := json.Unmarshal(top["error"], &obj); err != nil {
			t.Fatalf("parse error object: %v", err)
		}
		if obj.Code != apierror.WORKER_BAD_RESPONSE {
			t.Errorf("full mode empty worker code: error.code = %q, want %q", obj.Code, apierror.WORKER_BAD_RESPONSE)
		}
		if obj.RequestID != rid {
			t.Errorf("error.request_id = %q, want %q", obj.RequestID, rid)
		}
	})

	t.Run("sanitized mode empty worker error code still returns WORKER_UNREACHABLE", func(t *testing.T) {
		// Sanitized mode must return WORKER_UNREACHABLE for ALL worker errors — including
		// the edge case where the worker returns an empty error code. The customer-facing
		// contract (code=WORKER_UNREACHABLE, retryable=true) must not vary by whether the
		// worker's code field is empty or non-empty.
		worker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"error": map[string]any{
					"code":      "",
					"message":   "malformed error from worker",
					"retryable": false,
				},
				"billable_units": 1,
			})
		}))
		defer worker.Close()
		p := proxy.New(worker.URL, 5*time.Second, 0)
		h := invoke(p, nil, "sanitized", "echo")
		const rid = "test-rid-sanitized-bad-code"
		req := httptest.NewRequest(http.MethodPost, "/v1/echo", strings.NewReader(`{}`))
		req = req.WithContext(context.WithValue(req.Context(), mw.RequestIDKey, rid))
		w := httptest.NewRecorder()
		h(w, req)
		if w.Code != http.StatusBadGateway {
			t.Fatalf("status = %d, want %d", w.Code, http.StatusBadGateway)
		}
		var top map[string]json.RawMessage
		if err := json.Unmarshal(w.Body.Bytes(), &top); err != nil {
			t.Fatalf("parse body: %v", err)
		}
		var obj struct {
			Code      string `json:"code"`
			Message   string `json:"message"`
			Retryable bool   `json:"retryable"`
			RequestID string `json:"request_id"`
		}
		dec := json.NewDecoder(bytes.NewReader(top["error"]))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&obj); err != nil {
			t.Fatalf("parse error object: %v", err)
		}
		if obj.Code != "WORKER_UNREACHABLE" {
			t.Errorf("sanitized empty code: error.code = %q, want WORKER_UNREACHABLE", obj.Code)
		}
		if obj.Message != "worker unavailable" {
			t.Errorf("sanitized empty code: error.message = %q, want %q", obj.Message, "worker unavailable")
		}
		if !obj.Retryable {
			t.Error("sanitized empty code: error.retryable = false, want true (WORKER_UNREACHABLE is retryable)")
		}
		if obj.RequestID != rid {
			t.Errorf("sanitized empty code: error.request_id = %q, want %q", obj.RequestID, rid)
		}
	})
}

// --- Billing route tests ---

// mockBillingService is a stub BillingService for billing route tests.
type mockBillingService struct {
	checkoutURL      string
	checkoutErr      error
	portalURL        string
	portalErr        error
	stripeCustomerID string
	lookupErr        error
}

func (m *mockBillingService) CreateCheckoutSession(_ context.Context, _, _ string) (string, error) {
	return m.checkoutURL, m.checkoutErr
}
func (m *mockBillingService) CreatePortalSession(_ context.Context, _ string) (string, error) {
	return m.portalURL, m.portalErr
}
func (m *mockBillingService) LookupStripeCustomerID(_ context.Context, _ string) (string, error) {
	return m.stripeCustomerID, m.lookupErr
}

// testKey returns an auth.Key with a stable UUID so billing handler tests can inject auth context.
func testKey() *auth.Key {
	id := uuid.MustParse("550e8400-e29b-41d4-a716-446655440000")
	cid := uuid.MustParse("660e8400-e29b-41d4-a716-446655440001")
	return &auth.Key{ID: id, Customer: auth.Customer{ID: cid, Email: "test@example.com", Plan: "free"}}
}

func TestBillingCheckout_Unauthorized(t *testing.T) {
	healthy := &mockChecker{}
	d := &Deps{
		Cfg:   &config.Config{BodyLimitBytes: 1048576},
		Redis: healthy,
		PG:    healthy,
	}
	router := NewRouter(d)

	req := httptest.NewRequest(http.MethodPost, "/v1/billing/checkout", strings.NewReader(`{"plan_id":"pro"}`))
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", w.Code)
	}
}

func TestBillingPortal_Unauthorized(t *testing.T) {
	healthy := &mockChecker{}
	d := &Deps{
		Cfg:   &config.Config{BodyLimitBytes: 1048576},
		Redis: healthy,
		PG:    healthy,
	}
	router := NewRouter(d)

	req := httptest.NewRequest(http.MethodPost, "/v1/billing/portal", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", w.Code)
	}
}

func TestBillingCheckout_WithKey(t *testing.T) {
	const wantURL = "https://checkout.stripe.com/pay/cs_test_abc"

	d := &Deps{Checkout: &mockBillingService{checkoutURL: wantURL}}
	h := billingCheckoutHandler(d)

	req := httptest.NewRequest(http.MethodPost, "/v1/billing/checkout", strings.NewReader(`{"plan_id":"pro"}`))
	req = req.WithContext(auth.WithKey(req.Context(), testKey()))
	w := httptest.NewRecorder()
	h(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", w.Code)
	}
	var resp map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if resp["url"] != wantURL {
		t.Errorf("url = %q, want %q", resp["url"], wantURL)
	}
}

func TestBillingPortal_WithKey(t *testing.T) {
	const wantURL = "https://billing.stripe.com/session/test_xyz"

	d := &Deps{Checkout: &mockBillingService{
		stripeCustomerID: "cus_test",
		portalURL:        wantURL,
	}}
	h := billingPortalHandler(d)

	req := httptest.NewRequest(http.MethodPost, "/v1/billing/portal", nil)
	req = req.WithContext(auth.WithKey(req.Context(), testKey()))
	w := httptest.NewRecorder()
	h(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", w.Code)
	}
	var resp map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if resp["url"] != wantURL {
		t.Errorf("url = %q, want %q", resp["url"], wantURL)
	}
}

func TestBillingPortal_NoStripeCustomer(t *testing.T) {
	// When stripeCustomerID is empty, the handler must return 402 NO_STRIPE_CUSTOMER.
	d := &Deps{Checkout: &mockBillingService{stripeCustomerID: ""}}
	h := billingPortalHandler(d)

	req := httptest.NewRequest(http.MethodPost, "/v1/billing/portal", nil)
	req = req.WithContext(auth.WithKey(req.Context(), testKey()))
	w := httptest.NewRecorder()
	h(w, req)

	if w.Code != http.StatusPaymentRequired {
		t.Errorf("status = %d, want 402", w.Code)
	}
	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	errObj, ok := resp["error"].(map[string]any)
	if !ok {
		t.Fatalf("expected error object, got %T", resp["error"])
	}
	if errObj["code"] != "NO_STRIPE_CUSTOMER" {
		t.Errorf("error.code = %q, want NO_STRIPE_CUSTOMER", errObj["code"])
	}
}

func TestBillingCheckout_InvalidPlanID(t *testing.T) {
	// planIDRE rejects IDs with uppercase letters, special characters, or > 32 chars.
	// The handler must return 400 without consulting the BillingService.
	cases := []struct {
		name   string
		planID string
	}{
		{"uppercase", "PRO"},
		{"special_chars", "INVALID!!!"},
		{"spaces", "pro plan"},
		{"too_long", strings.Repeat("a", 33)},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := &Deps{Checkout: &mockBillingService{}}
			h := billingCheckoutHandler(d)

			body := fmt.Sprintf(`{"plan_id":%q}`, tc.planID)
			req := httptest.NewRequest(http.MethodPost, "/v1/billing/checkout", strings.NewReader(body))
			req = req.WithContext(auth.WithKey(req.Context(), testKey()))
			w := httptest.NewRecorder()
			h(w, req)

			if w.Code != http.StatusBadRequest {
				t.Errorf("plan_id=%q: status = %d, want 400", tc.planID, w.Code)
			}
		})
	}
}

func TestBillingCheckout_NotConfigured(t *testing.T) {
	// Checkout == nil → 503 (main.go default when not wired up).
	d := &Deps{Checkout: nil}
	h := billingCheckoutHandler(d)

	req := httptest.NewRequest(http.MethodPost, "/v1/billing/checkout", strings.NewReader(`{"plan_id":"pro"}`))
	req = req.WithContext(auth.WithKey(req.Context(), testKey()))
	w := httptest.NewRecorder()
	h(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", w.Code)
	}
}

// mustChiRoutes asserts that h implements chi.Routes (required for chi.Walk) and
// fails the test with a type-specific message if not. NewRouter returns *chi.Mux
// which satisfies chi.Routes; this helper surfaces a clear failure if that changes.
func mustChiRoutes(t *testing.T, h http.Handler) chi.Routes {
	t.Helper()
	cr, ok := h.(chi.Routes)
	if !ok {
		t.Fatalf("NewRouter returned %T which does not implement chi.Routes; chi.Walk unavailable", h)
	}
	return cr
}

// TestV1RoutesDriftGuard is the drift guard: it builds the real router, walks its
// mounted /v1 patterns (excluding /v1/billing/*), and asserts that the POST set
// equals the /v1 paths produced by openapi.Build(V1Routes). Also asserts:
//   - all per-product /v1 routes are POST-only (spec invariant)
//   - each route's OpenAPI operationId matches the expected "invoke_<segment>" pattern
//   - V1Routes contains no duplicate paths
//   - all generated operationIds are unique
func TestV1RoutesDriftGuard(t *testing.T) {
	healthy := &mockChecker{}
	d := &Deps{
		Cfg:   &config.Config{BodyLimitBytes: 1048576},
		Redis: healthy,
		PG:    healthy,
		// auth.Middleware, ratelimit.Middleware, quota.Middleware, and the invoke
		// handler are closures: they capture their arguments but never dereference
		// them at registration time. chi.Walk traverses the route tree without
		// dispatching any requests, so nil fields are never accessed.
	}
	router := NewRouter(d)
	chiRoutes := mustChiRoutes(t, router)

	// Verify V1Routes has no duplicate paths. NewRouter calls openapi.Handler(V1Routes)
	// which calls openapi.Build(), and Build panics on duplicate full paths (/v1+Path).
	// This check produces a clear t.Error rather than a test-level panic from Build.
	seen := make(map[string]bool, len(V1Routes))
	for _, rt := range V1Routes {
		key := "/v1" + rt.Path
		if seen[key] {
			t.Errorf("duplicate Path in V1Routes: %q (chi would silently shadow the earlier handler)", rt.Path)
		}
		seen[key] = true
	}

	// mounted[method][path] for all /v1 non-billing routes.
	mounted := make(map[string]map[string]struct{})
	if err := chi.Walk(chiRoutes, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		if !strings.HasPrefix(route, "/v1/") || strings.HasPrefix(route, "/v1/billing/") {
			return nil
		}
		if mounted[method] == nil {
			mounted[method] = make(map[string]struct{})
		}
		mounted[method][route] = struct{}{}
		return nil
	}); err != nil {
		t.Fatalf("chi.Walk: %v", err)
	}

	// All per-product /v1 routes must be POST-only: exactly one method key in mounted.
	if len(mounted) != 1 {
		t.Fatalf("expected exactly 1 HTTP method for /v1 routes, got %d: %v", len(mounted), mounted)
	}
	if _, ok := mounted[http.MethodPost]; !ok {
		t.Fatalf("expected POST to be the only /v1 method, got: %v", mounted)
	}
	if got, want := len(mounted[http.MethodPost]), len(V1Routes); got != want {
		t.Errorf("mounted %d /v1 POST routes, expected %d (V1Routes and routes.go are out of sync)", got, want)
	}

	// Collect /v1/* POST paths from the OpenAPI document (excluding /v1/billing/*).
	doc := openapi.Build(V1Routes)
	documented := make(map[string]struct{})
	for path, item := range doc.Paths {
		if strings.HasPrefix(path, "/v1/") && !strings.HasPrefix(path, "/v1/billing/") {
			if item.Post == nil {
				t.Errorf("openapi path %s has no POST operation (per-product routes must have POST)", path)
				continue
			}
			documented[path] = struct{}{}
		}
	}

	mountedPOST := mounted[http.MethodPost]
	for path := range mountedPOST {
		if _, ok := documented[path]; !ok {
			t.Errorf("route %s is mounted in router but absent from openapi.Build(V1Routes)", path)
		}
	}
	for path := range documented {
		if _, ok := mountedPOST[path]; !ok {
			t.Errorf("path %s is in openapi.Build(V1Routes) but not mounted in router", path)
		}
	}

	// Verify the operationId in the OpenAPI document matches "invoke_<path-segment>"
	// for every route the router actually mounted (derived from chi, not from V1Routes).
	seenOpIDs := make(map[string]string, len(mountedPOST)) // opID → path
	for path := range mountedPOST {
		item, ok := doc.Paths[path]
		if !ok || item.Post == nil {
			continue // already reported in parity check above
		}
		wantOpID := openapi.OperationIDFromPath(strings.TrimPrefix(path, "/v1"))
		if item.Post.OperationID != wantOpID {
			t.Errorf("path %s: openapi operationId = %q, want %q", path, item.Post.OperationID, wantOpID)
		}
		if firstPath, collision := seenOpIDs[item.Post.OperationID]; collision {
			t.Errorf("duplicate operationId %q: shared by paths %s and %s", item.Post.OperationID, firstPath, path)
		} else {
			seenOpIDs[item.Post.OperationID] = path
		}
	}

	// Verify no V1Route has empty required fields or reserved path prefixes.
	for _, rt := range V1Routes {
		if rt.Operation == "" {
			t.Errorf("V1Routes path %q has empty Operation field (opaque worker operation string required)", rt.Path)
		}
		if rt.Summary == "" {
			t.Errorf("V1Routes path %q has empty Summary field (required for OpenAPI document)", rt.Path)
		}
		if strings.HasPrefix(rt.Path, "/billing/") {
			t.Errorf("V1Routes must not contain billing paths (handled separately in NewRouter): %q", rt.Path)
		}
	}
}

// TestInvokeWorkerUnreachableIncrementsMetric asserts that the WORKER_UNREACHABLE
// counter is incremented when the worker transport call fails (err != nil path).
func TestInvokeWorkerUnreachableIncrementsMetric(t *testing.T) {
	worker := successWorker(1, "")
	p := proxy.New(worker.URL, 5*time.Second, 0)
	worker.Close() // cause connection refused so Invoke returns an error

	h := invoke(p, nil, "sanitized", "echo")

	before := testutil.ToFloat64(observability.WorkerErrorsTotal.WithLabelValues(apierror.WORKER_UNREACHABLE))
	req := httptest.NewRequest(http.MethodPost, "/v1/echo", strings.NewReader(`{}`))
	w := httptest.NewRecorder()
	h(w, req)

	if w.Code != http.StatusBadGateway {
		t.Fatalf("expected 502, got %d", w.Code)
	}
	if got := testutil.ToFloat64(observability.WorkerErrorsTotal.WithLabelValues(apierror.WORKER_UNREACHABLE)); got != before+1 {
		t.Errorf("WorkerErrorsTotal[WORKER_UNREACHABLE] = %v, want %v", got, before+1)
	}
}

// TestInvokeBillableUnitsViolationIncrementsMetric asserts that the WORKER_BAD_RESPONSE
// counter is incremented when the worker returns billable_units < 1 (invariant #2 path).
func TestInvokeBillableUnitsViolationIncrementsMetric(t *testing.T) {
	worker := successWorker(0, "") // billable_units=0 triggers the trust-boundary rejection
	defer worker.Close()

	p := proxy.New(worker.URL, 5*time.Second, 0)
	h := invoke(p, nil, "sanitized", "echo")

	before := testutil.ToFloat64(observability.WorkerErrorsTotal.WithLabelValues(apierror.WORKER_BAD_RESPONSE))
	req := httptest.NewRequest(http.MethodPost, "/v1/echo", strings.NewReader(`{}`))
	w := httptest.NewRecorder()
	h(w, req)

	if w.Code != http.StatusBadGateway {
		t.Fatalf("expected 502, got %d", w.Code)
	}
	if got := testutil.ToFloat64(observability.WorkerErrorsTotal.WithLabelValues(apierror.WORKER_BAD_RESPONSE)); got != before+1 {
		t.Errorf("WorkerErrorsTotal[WORKER_BAD_RESPONSE] = %v, want %v", got, before+1)
	}
}

// TestInvokeNon2xx_WorkerBodyAbsentFromCustomerResponse is the route-level acceptance
// gate for the non-2xx body-capture feature. It verifies that a distinctive diagnostic
// body returned by the worker on a non-2xx response is captured for operator logs but
// is completely absent from the customer-facing HTTP response envelope.
func TestInvokeNon2xx_WorkerBodyAbsentFromCustomerResponse(t *testing.T) {
	const workerDiagBody = "db_error: too many connections; pool_exhausted=true; retry_count=999"
	worker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(workerDiagBody))
	}))
	defer worker.Close()

	p := proxy.New(worker.URL, 5*time.Second, 0)
	h := invoke(p, nil, "full", "test") // "full" mode to confirm even full-exposure never leaks non-2xx bodies

	req := httptest.NewRequest(http.MethodPost, "/v1/test", strings.NewReader(`{"input":"hello"}`))
	w := httptest.NewRecorder()
	h(w, req)

	if w.Code != http.StatusBadGateway {
		t.Fatalf("expected 502, got %d", w.Code)
	}

	customerResponse := w.Body.String()

	// The worker's diagnostic body must NEVER appear in the customer-facing response.
	// Operators see it via the structured log (log.Error().Err(err)); customers see only
	// the sanitized WORKER_UNREACHABLE envelope.
	if strings.Contains(customerResponse, workerDiagBody) {
		t.Errorf("customer response contains worker diagnostic body %q; must be operator-only", workerDiagBody)
	}
	// Spot-check: none of the distinctive substrings should leak either.
	for _, fragment := range []string{"db_error", "pool_exhausted", "retry_count"} {
		if strings.Contains(customerResponse, fragment) {
			t.Errorf("customer response leaks worker body fragment %q", fragment)
		}
	}

	// The response IS the sanitized WORKER_UNREACHABLE envelope.
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("customer response is not valid JSON: %v", err)
	}
	errObj, ok := body["error"].(map[string]any)
	if !ok {
		t.Fatalf("expected error object in customer response, got %T", body["error"])
	}
	if errObj["code"] != apierror.WORKER_UNREACHABLE {
		t.Errorf("customer error code = %q, want %q", errObj["code"], apierror.WORKER_UNREACHABLE)
	}
	if errObj["message"] != "worker unavailable" {
		t.Errorf("customer error message = %q, want %q", errObj["message"], "worker unavailable")
	}
}

// --- GET /v1/jobs (jobsListHandler) tests ---
//
// jobsListHandler is called directly (bypassing NewRouter/auth.Middleware),
// the same pattern TestBillingCheckout_WithKey uses above: auth.WithKey
// injects the request context jobsListHandler reads via auth.FromContext,
// so no auth.Store/Redis is needed. jobs.Store itself needs a real
// customer_id/api_key_id foreign key target, hence the local Postgres
// helpers below, mirroring gateway/internal/jobs/store_test.go's
// newTestPostgres/seedCustomer.

// jobsListTestPostgres mirrors jobs/store_test.go's newTestPostgres: skip
// when Postgres is unreachable, unless the DSN was explicitly requested (CI),
// in which case failure is fatal. Applies migrations (idempotent —
// invariant #8) so the suite is self-contained.
func jobsListTestPostgres(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	explicit := dsn != ""
	if !explicit {
		if v := os.Getenv("POSTGRES_DSN"); v != "" {
			dsn = v
			explicit = true
		} else {
			dsn = "postgres://crucible@localhost:5432/crucible?sslmode=disable"
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		if explicit {
			t.Fatalf("TEST_DATABASE_URL set but postgres unavailable: %v", err)
		}
		t.Skipf("postgres unavailable, skipping: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		if explicit {
			t.Fatalf("TEST_DATABASE_URL set but postgres ping failed: %v", err)
		}
		t.Skipf("postgres ping failed, skipping: %v", err)
	}
	if err := db.Apply(ctx, pool); err != nil {
		pool.Close()
		t.Fatalf("apply migrations: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// seedJobsListCustomer inserts a minimal customers + api_keys row pair and
// registers cleanup (async_jobs first, matching jobs/store_test.go's
// seedCustomer — async_jobs has no ON DELETE CASCADE from customers).
// Returns an *auth.Key usable as jobsListHandler's auth.FromContext value.
func seedJobsListCustomer(t *testing.T, pool *pgxpool.Pool, email string) *auth.Key {
	t.Helper()
	ctx := context.Background()
	var custID uuid.UUID
	if err := pool.QueryRow(ctx,
		`INSERT INTO customers (email, plan_id) VALUES ($1, 'free') RETURNING id`, email,
	).Scan(&custID); err != nil {
		t.Fatalf("seedJobsListCustomer: %v", err)
	}
	var keyID uuid.UUID
	prefix := "cru_test_" + uuid.New().String()[:8]
	if err := pool.QueryRow(ctx,
		`INSERT INTO api_keys (customer_id, prefix, hash) VALUES ($1, $2, '\x00') RETURNING id`, custID, prefix,
	).Scan(&keyID); err != nil {
		t.Fatalf("seedJobsListCustomer: insert api_key: %v", err)
	}
	t.Cleanup(func() {
		ctx := context.Background()
		_, _ = pool.Exec(ctx, `DELETE FROM async_jobs WHERE customer_id = $1`, custID)
		_, _ = pool.Exec(ctx, `DELETE FROM customers WHERE id = $1`, custID)
	})
	return &auth.Key{ID: keyID, Customer: auth.Customer{ID: custID, Email: email, Plan: "free"}}
}

func TestJobsListHandler_CustomerScopedShapeAndHeader(t *testing.T) {
	pool := jobsListTestPostgres(t)
	store := jobs.NewStore(pool)
	keyA := seedJobsListCustomer(t, pool, "jobs-list-route-a-"+uuid.New().String()+"@example.com")
	keyB := seedJobsListCustomer(t, pool, "jobs-list-route-b-"+uuid.New().String()+"@example.com")

	if _, err := store.Enqueue(context.Background(), keyA.Customer.ID, keyA.ID, "echo", "req-a", "free", json.RawMessage(`{}`), 0, ""); err != nil {
		t.Fatalf("Enqueue (A): %v", err)
	}
	if _, err := store.Enqueue(context.Background(), keyB.Customer.ID, keyB.ID, "echo", "req-b", "free", json.RawMessage(`{}`), 0, ""); err != nil {
		t.Fatalf("Enqueue (B): %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/v1/jobs", nil)
	req = req.WithContext(auth.WithKey(req.Context(), keyA))
	w := httptest.NewRecorder()
	jobsListHandler(store)(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want %q", got, "no-store")
	}

	var body paging.Page[jobListItem]
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v\nbody: %s", err, w.Body.String())
	}
	if body.Total != 1 || len(body.Items) != 1 {
		t.Fatalf("items/total = %d/%d, want 1/1 (must contain only the caller's own jobs)", len(body.Items), body.Total)
	}
	if body.Items[0].Operation != "echo" {
		t.Errorf("items[0].Operation = %q, want %q", body.Items[0].Operation, "echo")
	}
}

func TestJobsListHandler_StatusFilter(t *testing.T) {
	pool := jobsListTestPostgres(t)
	store := jobs.NewStore(pool)
	key := seedJobsListCustomer(t, pool, "jobs-list-route-status-"+uuid.New().String()+"@example.com")

	if _, err := store.Enqueue(context.Background(), key.Customer.ID, key.ID, "echo", "req-queued", "free", json.RawMessage(`{}`), 0, ""); err != nil {
		t.Fatalf("Enqueue (queued): %v", err)
	}
	failedID, err := store.Enqueue(context.Background(), key.Customer.ID, key.ID, "echo", "req-failed", "free", json.RawMessage(`{}`), 0, "")
	if err != nil {
		t.Fatalf("Enqueue (failed): %v", err)
	}
	if err := store.Fail(context.Background(), failedID, "WORKER_BAD_RESPONSE", "worker contract violation"); err != nil {
		t.Fatalf("Fail: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/v1/jobs?status=failed", nil)
	req = req.WithContext(auth.WithKey(req.Context(), key))
	w := httptest.NewRecorder()
	jobsListHandler(store)(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	var body paging.Page[jobListItem]
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v\nbody: %s", err, w.Body.String())
	}
	if body.Total != 1 || len(body.Items) != 1 {
		t.Fatalf("?status=failed items/total = %d/%d, want 1/1", len(body.Items), body.Total)
	}
	if body.Items[0].JobID != failedID.String() {
		t.Errorf("?status=failed returned job %q, want %q", body.Items[0].JobID, failedID)
	}
	if body.Items[0].Error == nil || body.Items[0].Error.Code != "WORKER_BAD_RESPONSE" {
		t.Errorf("?status=failed item error = %+v, want code WORKER_BAD_RESPONSE", body.Items[0].Error)
	}
}

func TestJobsListHandler_UnknownStatus400(t *testing.T) {
	pool := jobsListTestPostgres(t)
	store := jobs.NewStore(pool)
	key := seedJobsListCustomer(t, pool, "jobs-list-route-badstatus-"+uuid.New().String()+"@example.com")

	req := httptest.NewRequest(http.MethodGet, "/v1/jobs?status=not-a-real-status", nil)
	req = req.WithContext(auth.WithKey(req.Context(), key))
	w := httptest.NewRecorder()
	jobsListHandler(store)(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", w.Code, w.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v\nbody: %s", err, w.Body.String())
	}
	errObj, ok := body["error"].(map[string]any)
	if !ok {
		t.Fatalf("expected error object, got %T", body["error"])
	}
	if errObj["code"] != apierror.BAD_REQUEST {
		t.Errorf("error.code = %v, want %q", errObj["code"], apierror.BAD_REQUEST)
	}
}

func TestJobsListHandler_NotConfigured(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/v1/jobs", nil)
	req = req.WithContext(auth.WithKey(req.Context(), testKey()))
	w := httptest.NewRecorder()
	jobsListHandler(nil)(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503: %s", w.Code, w.Body.String())
	}
}

func TestJobsListHandler_Unauthorized(t *testing.T) {
	pool := jobsListTestPostgres(t)
	store := jobs.NewStore(pool)

	req := httptest.NewRequest(http.MethodGet, "/v1/jobs", nil)
	w := httptest.NewRecorder()
	jobsListHandler(store)(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401: %s", w.Code, w.Body.String())
	}
}

// --- POST /v1/jobs/{id}/cancel (jobsCancelHandler) tests ---
//
// jobsCancelHandler reads its "id" path param via chi.URLParam, so — unlike
// jobsListHandler above — these tests route requests through a small chi
// router (mirroring operator/jobs_handlers_test.go's newJobsAdminRouter
// pattern) rather than calling the handler directly. A middleware injects
// the auth.Key the same way auth.Middleware would, without needing a real
// auth.Store.

// newJobsCancelRouter mounts jobsCancelHandler at the same path routes.go
// registers it under, with a middleware injecting key into the request
// context via auth.WithKey (nil key exercises the unauthenticated path).
func newJobsCancelRouter(store *jobs.Store, key *auth.Key) http.Handler {
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			if key != nil {
				req = req.WithContext(auth.WithKey(req.Context(), key))
			}
			next.ServeHTTP(w, req)
		})
	})
	r.Post("/v1/jobs/{id}/cancel", jobsCancelHandler(store))
	return r
}

// TestJobsCancelHandler_TableDriven covers all three documented outcomes:
// 200 on queued->cancelled, 409 (JOB_NOT_CANCELLABLE) when the job exists
// but isn't queued, and 404 when the job is absent or owned by another
// customer (IDOR-safe, indistinguishable by design — mirrors jobsGetHandler).
func TestJobsCancelHandler_TableDriven(t *testing.T) {
	pool := jobsListTestPostgres(t)
	store := jobs.NewStore(pool)
	key := seedJobsListCustomer(t, pool, "jobs-cancel-route-"+uuid.New().String()+"@example.com")
	otherKey := seedJobsListCustomer(t, pool, "jobs-cancel-route-other-"+uuid.New().String()+"@example.com")

	newJob := func(t *testing.T) uuid.UUID {
		t.Helper()
		id, err := store.Enqueue(context.Background(), key.Customer.ID, key.ID, "echo", "req-"+uuid.New().String(), "free", json.RawMessage(`{}`), 0, "")
		if err != nil {
			t.Fatalf("Enqueue: %v", err)
		}
		return id
	}

	doCancel := func(t *testing.T, router http.Handler, id uuid.UUID) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/v1/jobs/"+id.String()+"/cancel", nil)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		return w
	}

	t.Run("queued job is cancelled with 200", func(t *testing.T) {
		id := newJob(t)
		w := doCancel(t, newJobsCancelRouter(store, key), id)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
		}
		var body asyncJobResponse
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode response: %v\nbody: %s", err, w.Body.String())
		}
		if body.Status != jobs.StatusCancelled {
			t.Errorf("status field = %q, want %q", body.Status, jobs.StatusCancelled)
		}

		job, ok, err := store.Get(context.Background(), id, key.Customer.ID)
		if err != nil || !ok {
			t.Fatalf("Get: ok=%v err=%v", ok, err)
		}
		if job.Status != jobs.StatusCancelled {
			t.Errorf("persisted status = %q, want %q", job.Status, jobs.StatusCancelled)
		}
	})

	t.Run("running job is rejected with 409", func(t *testing.T) {
		id := newJob(t)
		if _, err := store.Claim(context.Background(), 10, uuid.New(), 0); err != nil {
			t.Fatalf("Claim: %v", err)
		}
		w := doCancel(t, newJobsCancelRouter(store, key), id)
		if w.Code != http.StatusConflict {
			t.Fatalf("status = %d, want 409: %s", w.Code, w.Body.String())
		}
		var body map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode response: %v\nbody: %s", err, w.Body.String())
		}
		errObj, ok := body["error"].(map[string]any)
		if !ok {
			t.Fatalf("expected error object, got %T", body["error"])
		}
		if errObj["code"] != "JOB_NOT_CANCELLABLE" {
			t.Errorf("error.code = %v, want JOB_NOT_CANCELLABLE", errObj["code"])
		}
	})

	t.Run("succeeded job is rejected with 409", func(t *testing.T) {
		id := newJob(t)
		if _, err := store.Claim(context.Background(), 10, uuid.New(), 0); err != nil {
			t.Fatalf("Claim: %v", err)
		}
		if err := store.Complete(context.Background(), id, json.RawMessage(`{}`), 1, "units"); err != nil {
			t.Fatalf("Complete: %v", err)
		}
		w := doCancel(t, newJobsCancelRouter(store, key), id)
		if w.Code != http.StatusConflict {
			t.Fatalf("status = %d, want 409: %s", w.Code, w.Body.String())
		}
	})

	t.Run("nonexistent job is 404", func(t *testing.T) {
		w := doCancel(t, newJobsCancelRouter(store, key), uuid.New())
		if w.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404: %s", w.Code, w.Body.String())
		}
	})

	t.Run("another customer's job is 404 (IDOR)", func(t *testing.T) {
		id := newJob(t)
		w := doCancel(t, newJobsCancelRouter(store, otherKey), id)
		if w.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404: %s", w.Code, w.Body.String())
		}
		job, ok, err := store.Get(context.Background(), id, key.Customer.ID)
		if err != nil || !ok {
			t.Fatalf("Get: ok=%v err=%v", ok, err)
		}
		if job.Status != jobs.StatusQueued {
			t.Errorf("status = %q, want %q (must be left untouched)", job.Status, jobs.StatusQueued)
		}
	})
}

// TestEnqueueAsync_BacklogCeiling is the acceptance test for
// JOB_MAX_QUEUED_PER_CUSTOMER: enqueues under the ceiling still return 202
// with a job_id, and the enqueue that would push the customer's queued+
// running backlog past the ceiling is rejected with 429 JOB_BACKLOG_EXCEEDED
// instead of growing the backlog further.
func TestEnqueueAsync_BacklogCeiling(t *testing.T) {
	pool := jobsListTestPostgres(t)
	store := jobs.NewStore(pool)
	key := seedJobsListCustomer(t, pool, "jobs-enqueue-ceiling-"+uuid.New().String()+"@example.com")

	doEnqueue := func(t *testing.T, maxQueuedPerCustomer int) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/v1/echo", strings.NewReader(`{}`))
		req = req.WithContext(auth.WithKey(req.Context(), key))
		w := httptest.NewRecorder()
		enqueueAsync(store, "echo", 0, maxQueuedPerCustomer)(w, req)
		return w
	}

	const ceiling = 2
	for i := 0; i < ceiling; i++ {
		w := doEnqueue(t, ceiling)
		if w.Code != http.StatusAccepted {
			t.Fatalf("enqueue %d: status = %d, want 202: %s", i, w.Code, w.Body.String())
		}
		var body map[string]string
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode response: %v\nbody: %s", err, w.Body.String())
		}
		if body["job_id"] == "" {
			t.Errorf("enqueue %d: job_id missing from response", i)
		}
	}

	w := doEnqueue(t, ceiling)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("over-ceiling enqueue: status = %d, want 429: %s", w.Code, w.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v\nbody: %s", err, w.Body.String())
	}
	errObj, ok := body["error"].(map[string]any)
	if !ok {
		t.Fatalf("expected error object, got %T", body["error"])
	}
	if errObj["code"] != jobBacklogExceededCode {
		t.Errorf("error.code = %v, want %q", errObj["code"], jobBacklogExceededCode)
	}
}

// TestEnqueueAsync_BacklogCeilingDisabledAdmitsUnconditionally proves the
// zero-value default (maxQueuedPerCustomer=0) preserves today's behaviour:
// enqueue never returns 429 regardless of how deep the caller's backlog is.
func TestEnqueueAsync_BacklogCeilingDisabledAdmitsUnconditionally(t *testing.T) {
	pool := jobsListTestPostgres(t)
	store := jobs.NewStore(pool)
	key := seedJobsListCustomer(t, pool, "jobs-enqueue-unbounded-"+uuid.New().String()+"@example.com")

	for i := 0; i < 5; i++ {
		req := httptest.NewRequest(http.MethodPost, "/v1/echo", strings.NewReader(`{}`))
		req = req.WithContext(auth.WithKey(req.Context(), key))
		w := httptest.NewRecorder()
		enqueueAsync(store, "echo", 0, 0)(w, req)
		if w.Code != http.StatusAccepted {
			t.Fatalf("enqueue %d: status = %d, want 202 (ceiling disabled): %s", i, w.Code, w.Body.String())
		}
	}
}

func TestJobsCancelHandler_NotConfigured(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/v1/jobs/"+uuid.New().String()+"/cancel", nil)
	w := httptest.NewRecorder()
	newJobsCancelRouter(nil, testKey()).ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503: %s", w.Code, w.Body.String())
	}
}

func TestJobsCancelHandler_Unauthorized(t *testing.T) {
	pool := jobsListTestPostgres(t)
	store := jobs.NewStore(pool)

	req := httptest.NewRequest(http.MethodPost, "/v1/jobs/"+uuid.New().String()+"/cancel", nil)
	w := httptest.NewRecorder()
	newJobsCancelRouter(store, nil).ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401: %s", w.Code, w.Body.String())
	}
}

func TestJobsCancelHandler_InvalidID(t *testing.T) {
	pool := jobsListTestPostgres(t)
	store := jobs.NewStore(pool)
	key := seedJobsListCustomer(t, pool, "jobs-cancel-invalid-id-"+uuid.New().String()+"@example.com")

	req := httptest.NewRequest(http.MethodPost, "/v1/jobs/not-a-uuid/cancel", nil)
	w := httptest.NewRecorder()
	newJobsCancelRouter(store, key).ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", w.Code, w.Body.String())
	}
}

