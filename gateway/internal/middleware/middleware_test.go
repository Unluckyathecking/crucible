package middleware

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
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
