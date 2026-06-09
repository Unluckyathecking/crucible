package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Unluckyathecking/crucible/gateway/internal/auth"
	"github.com/Unluckyathecking/crucible/gateway/internal/billing"
	"github.com/Unluckyathecking/crucible/gateway/internal/config"
	mw "github.com/Unluckyathecking/crucible/gateway/internal/middleware"
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
				if contains(bodyStr, leak) {
					t.Errorf("mode %q leaked worker internal detail %q", mode, leak)
				}
			}
		})
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

// TestInvokeErrorEnvelopeShape verifies the full four-field error envelope shape
// (top-level "error" key only, code/message/retryable/request_id present) on an
// invoke error path. Replaces field-level coverage lost when writeJSONError was removed.
func TestInvokeErrorEnvelopeShape(t *testing.T) {
	worker := successWorker(1, "")
	defer worker.Close()

	p := proxy.New(worker.URL, 5*time.Second, 0)
	h := invoke(p, nil, "sanitized", "echo")

	const testRID = "test-rid-routes"
	req := httptest.NewRequest(http.MethodPost, "/v1/echo", strings.NewReader(`not-json`))
	req = req.WithContext(context.WithValue(req.Context(), mw.RequestIDKey, testRID))
	w := httptest.NewRecorder()
	h(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
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
	if obj.RequestID != testRID {
		t.Errorf("error.request_id = %q, want %q", obj.RequestID, testRID)
	}
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

