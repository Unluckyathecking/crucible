package middleware

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/rs/zerolog/log"
)

var okHandler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
})

var panicHandler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
	panic("boom")
})

func TestRequestID(t *testing.T) {
	tests := []struct {
		name          string
		inboundHeader string
		wantPrefix    string
		wantValidUUID bool
	}{
		{
			name:          "generates UUID when no inbound header",
			inboundHeader: "",
			wantValidUUID: true,
		},
		{
			name:          "respects inbound X-Request-ID",
			inboundHeader: "abc-123",
			wantPrefix:    "abc-123",
		},
		{
			name:          "rejects inbound header longer than 64 chars",
			inboundHeader: strings.Repeat("x", 65),
			wantValidUUID: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			if tt.inboundHeader != "" {
				req.Header.Set("X-Request-ID", tt.inboundHeader)
			}

			rec := httptest.NewRecorder()
			RequestID(okHandler).ServeHTTP(rec, req)

			got := rec.Header().Get("X-Request-ID")
			if got == "" {
				t.Fatal("X-Request-ID header not set")
			}

			if tt.wantPrefix != "" {
				if !strings.HasPrefix(got, tt.wantPrefix) {
					t.Errorf("X-Request-ID = %q, want prefix %q", got, tt.wantPrefix)
				}
			}

			if tt.wantValidUUID {
				if _, err := uuid.Parse(got); err != nil {
					t.Errorf("X-Request-ID = %q is not a valid UUID: %v", got, err)
				}
			}

			if len(tt.inboundHeader) > 64 && got == tt.inboundHeader {
				t.Errorf("over-long inbound header was passed through unchanged: %q", got)
			}
		})
	}
}

func TestRequestIDContext(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, ok := r.Context().Value(RequestIDKey).(string)
		if !ok {
			t.Error("RequestIDKey not found in context")
		}
		if id == "" {
			t.Error("request id is empty in context")
		}
	})

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	RequestID(handler).ServeHTTP(rec, req)
}

func TestRecoveryPanicReturns500JSON(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()

	Recovery(panicHandler).ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusInternalServerError)
	}

	ct := rec.Header().Get("Content-Type")
	if !strings.Contains(ct, "application/json") {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}

	var body map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("failed to decode JSON body: %v", err)
	}
	errObj, ok := body["error"].(map[string]interface{})
	if !ok {
		t.Fatalf("body missing error object: %v", body)
	}
	if errObj["code"] != "INTERNAL" {
		t.Errorf("error.code = %v, want INTERNAL", errObj["code"])
	}
	if errObj["message"] != "internal error" {
		t.Errorf("error.message = %v, want internal error", errObj["message"])
	}
}

func TestRecoveryHandlerContinues(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-Request-ID", "test-rid")
	rec := httptest.NewRecorder()

	Recovery(okHandler).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusOK)
	}
}

func TestAccessLogPassThrough(t *testing.T) {
	echoHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte("hello"))
	})

	req := httptest.NewRequest(http.MethodPost, "/test", nil)
	rec := httptest.NewRecorder()

	AccessLog(echoHandler).ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusCreated)
	}
	if rec.Body.String() != "hello" {
		t.Errorf("body = %q, want hello", rec.Body.String())
	}
}

func TestAccessLogWithPanic(t *testing.T) {
	var buf strings.Builder
	// We want to capture the zerolog output to verify the access log exists
	oldLogger := log.Logger
	defer func() { log.Logger = oldLogger }()

	// Overwrite the global logger temporarily
	log.Logger = log.Output(&buf)

	req := httptest.NewRequest(http.MethodGet, "/test-panic", nil)
	rec := httptest.NewRecorder()

	// Since we swapped the order, AccessLog should wrap Recovery
	handler := AccessLog(Recovery(panicHandler))
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusInternalServerError)
	}

	output := buf.String()
	if !strings.Contains(output, `"message":"access"`) {
		t.Errorf("expected access log message in output, got:\n%s", output)
	}
	if !strings.Contains(output, `"status":500`) {
		t.Errorf("expected access log to contain status 500, got:\n%s", output)
	}
	if !strings.Contains(output, `"path":"/test-panic"`) {
		t.Errorf("expected access log to contain path /test-panic, got:\n%s", output)
	}
}

func TestSecurityHeaders(t *testing.T) {
	tests := []struct {
		name   string
		header string
		want   string
	}{
		{"Strict-Transport-Security", "Strict-Transport-Security", "max-age=63072000; includeSubDomains"},
		{"X-Content-Type-Options", "X-Content-Type-Options", "nosniff"},
		{"X-Frame-Options", "X-Frame-Options", "DENY"},
		{"X-XSS-Protection", "X-XSS-Protection", "0"},
		{"Referrer-Policy", "Referrer-Policy", "strict-origin-when-cross-origin"},
		{"Permissions-Policy", "Permissions-Policy", "camera=(), microphone=(), geolocation=(), interest-cohort=()"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			rec := httptest.NewRecorder()

			SecurityHeaders(okHandler).ServeHTTP(rec, req)

			got := rec.Header().Get(tt.header)
			if got != tt.want {
				t.Errorf("%s = %q, want %q", tt.header, got, tt.want)
			}
		})
	}
}

func TestSecurityHeadersPassThrough(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	})

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()

	SecurityHeaders(handler).ServeHTTP(rec, req)

	if rec.Code != http.StatusTeapot {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusTeapot)
	}
}

// TestSecurityHeadersAllPresent verifies every required OWASP header appears on every response.
func TestSecurityHeadersAllPresent(t *testing.T) {
	required := []string{
		"Strict-Transport-Security",
		"X-Content-Type-Options",
		"X-Frame-Options",
		"X-XSS-Protection",
		"Referrer-Policy",
		"Permissions-Policy",
	}

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	rec := httptest.NewRecorder()
	SecurityHeaders(okHandler).ServeHTTP(rec, req)

	for _, h := range required {
		if got := rec.Header().Get(h); got == "" {
			t.Errorf("header %q missing from response", h)
		}
	}
}

// TestBodyLimitAllowsRequestUnderLimit confirms small bodies pass through intact.
func TestBodyLimitAllowsRequestUnderLimit(t *testing.T) {
	const max = 10  // bytes
	body := "hello" // 5 bytes — under limit

	readHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "read error", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(data)
	})

	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	rec := httptest.NewRecorder()
	BodyLimit(max)(readHandler).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if rec.Body.String() != body {
		t.Errorf("body = %q, want %q", rec.Body.String(), body)
	}
}

// TestBodyLimitRejects413 verifies that reading a body beyond the limit surfaces a MaxBytesError.
// The middleware sets http.MaxBytesReader; a well-behaved handler detects the error and replies 413.
func TestBodyLimitRejects413(t *testing.T) {
	const max = 5 // bytes
	oversized := strings.Repeat("x", 100)

	guardHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, err := io.ReadAll(r.Body)
		if err != nil {
			var mbe *http.MaxBytesError
			if errors.As(err, &mbe) {
				http.Error(w, `{"error":{"code":"REQUEST_TOO_LARGE","message":"request body too large"}}`, http.StatusRequestEntityTooLarge)
				return
			}
			http.Error(w, "read error", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(oversized))
	rec := httptest.NewRecorder()
	BodyLimit(max)(guardHandler).ServeHTTP(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want %d (413)", rec.Code, http.StatusRequestEntityTooLarge)
	}
}

// TestBodyLimitExactlyAtLimit verifies a body exactly at the limit is accepted.
func TestBodyLimitExactlyAtLimit(t *testing.T) {
	const max = 5
	body := "hello" // exactly 5 bytes

	readHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "read error", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	rec := httptest.NewRecorder()
	BodyLimit(max)(readHandler).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusOK)
	}
}

// TestRecoveryNoLeakOfPanicValue asserts that the panic value never appears in the response body.
func TestRecoveryNoLeakOfPanicValue(t *testing.T) {
	secretPanic := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("super-secret-internal-db-password-1234")
	})

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	Recovery(secretPanic).ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "super-secret-internal-db-password-1234") {
		t.Error("panic value leaked into response body")
	}
	// Confirm the body is valid JSON with the safe envelope only.
	var body map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("response body is not valid JSON: %v", err)
	}
}

// TestRecoveryWithRequestIDInContext verifies that when RequestID runs before Recovery,
// the logged request-id matches the one set by RequestID middleware.
func TestRecoveryWithRequestIDInContext(t *testing.T) {
	const testRID = "test-request-id-abc"

	panicWithRID := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("whoops")
	})

	var buf strings.Builder
	oldLogger := log.Logger
	defer func() { log.Logger = oldLogger }()
	log.Logger = log.Output(&buf)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-Request-ID", testRID)

	rec := httptest.NewRecorder()
	// Stack: RequestID → Recovery → panicHandler, so rid is in context when Recovery logs.
	RequestID(Recovery(panicWithRID)).ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rec.Code)
	}
	output := buf.String()
	if !strings.Contains(output, testRID) {
		t.Errorf("expected request_id %q in log output, got:\n%s", testRID, output)
	}
}

// TestRequestIDPropagatesIntoContext verifies the id placed on the response header
// is also the id stored in the request context.
func TestRequestIDPropagatesIntoContext(t *testing.T) {
	var ctxID, headerID string

	captureHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctxID, _ = r.Context().Value(RequestIDKey).(string)
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	RequestID(captureHandler).ServeHTTP(rec, req)

	headerID = rec.Header().Get("X-Request-ID")

	if headerID == "" {
		t.Fatal("X-Request-ID response header is empty")
	}
	if ctxID == "" {
		t.Fatal("request id not found in context")
	}
	if ctxID != headerID {
		t.Errorf("context id %q != response header id %q", ctxID, headerID)
	}
}
