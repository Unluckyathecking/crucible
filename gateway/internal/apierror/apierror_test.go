package apierror_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Unluckyathecking/crucible/gateway/internal/apierror"
)

func TestWrite_StatusAndHeaders(t *testing.T) {
	tests := []struct {
		name      string
		status    int
		code      string
		message   string
		retryable bool
		requestID string
	}{
		{"unauthorized", http.StatusUnauthorized, apierror.UNAUTHORIZED, "invalid api key", false, "req-abc"},
		{"rate_limited retryable", http.StatusTooManyRequests, apierror.RATE_LIMITED, "rate limit exceeded", true, "req-xyz"},
		{"internal no rid", http.StatusInternalServerError, apierror.INTERNAL, "internal error", false, ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			apierror.Write(w, tt.requestID, tt.status, tt.code, tt.message, tt.retryable)

			if w.Code != tt.status {
				t.Errorf("status = %d, want %d", w.Code, tt.status)
			}
			if ct := w.Header().Get("Content-Type"); ct != "application/json" {
				t.Errorf("Content-Type = %q, want application/json", ct)
			}
			if cc := w.Header().Get("Cache-Control"); cc != "no-store" {
				t.Errorf("Cache-Control = %q, want no-store", cc)
			}
			var got struct {
				Error apierror.Error `json:"error"`
			}
			if err := json.NewDecoder(w.Body).Decode(&got); err != nil {
				t.Errorf("body is not valid JSON: %v", err)
			} else {
				if got.Error.Code != tt.code {
					t.Errorf("error.code = %q, want %q", got.Error.Code, tt.code)
				}
				if got.Error.Retryable != tt.retryable {
					t.Errorf("error.retryable = %v, want %v", got.Error.Retryable, tt.retryable)
				}
			}
		})
	}
}

func TestWrite_EnvelopeShape(t *testing.T) {
	w := httptest.NewRecorder()
	apierror.Write(w, "req-123", http.StatusBadRequest, apierror.BAD_REQUEST, "bad input", false)

	var top map[string]json.RawMessage
	if err := json.Unmarshal(w.Body.Bytes(), &top); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(top) != 1 {
		t.Errorf("top-level keys = %d, want 1", len(top))
	}
	if _, ok := top["error"]; !ok {
		t.Error("missing top-level \"error\" key")
	}
}

func TestWrite_AllFieldsPresent(t *testing.T) {
	w := httptest.NewRecorder()
	apierror.Write(w, "", http.StatusInternalServerError, apierror.INTERNAL, "err", false)

	var raw map[string]map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	errObj := raw["error"]
	for _, field := range []string{"code", "message", "retryable", "request_id"} {
		if _, ok := errObj[field]; !ok {
			t.Errorf("field %q missing from error object", field)
		}
	}
}

func TestWrite_RetryableTrue(t *testing.T) {
	w := httptest.NewRecorder()
	apierror.Write(w, "", http.StatusTooManyRequests, apierror.RATE_LIMITED, "limited", true)

	var got struct {
		Error apierror.Error `json:"error"`
	}
	if err := json.NewDecoder(w.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !got.Error.Retryable {
		t.Error("retryable = false, want true")
	}
}

func TestWrite_RetryableFalse(t *testing.T) {
	w := httptest.NewRecorder()
	apierror.Write(w, "", http.StatusUnauthorized, apierror.UNAUTHORIZED, "unauthorized", false)

	var got struct {
		Error apierror.Error `json:"error"`
	}
	if err := json.NewDecoder(w.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Error.Retryable {
		t.Error("retryable = true, want false")
	}
}

func TestWrite_RequestIDPassthrough(t *testing.T) {
	rid := "unique-request-id-passthrough"
	w := httptest.NewRecorder()
	apierror.Write(w, rid, http.StatusBadRequest, apierror.BAD_REQUEST, "bad", false)

	var got struct {
		Error apierror.Error `json:"error"`
	}
	if err := json.NewDecoder(w.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Error.RequestID != rid {
		t.Errorf("request_id = %q, want %q", got.Error.RequestID, rid)
	}
}

func TestWrite_RequestIDWithJSONSpecialCharsRoundTrips(t *testing.T) {
	// json.Marshal on the envelope struct escapes JSON-special chars in requestID
	// so the body remains valid JSON and the decoded value equals the original string.
	rid := `req"with"quotes\and` + "\ttab"
	w := httptest.NewRecorder()
	apierror.Write(w, rid, http.StatusBadRequest, apierror.BAD_REQUEST, "bad", false)

	var got struct {
		Error apierror.Error `json:"error"`
	}
	if err := json.NewDecoder(w.Body).Decode(&got); err != nil {
		t.Fatalf("decode failed (request_id not properly escaped?): %v", err)
	}
	if got.Error.RequestID != rid {
		t.Errorf("request_id = %q, want %q", got.Error.RequestID, rid)
	}
}

func TestWrite_NoTrailingNewline(t *testing.T) {
	w := httptest.NewRecorder()
	apierror.Write(w, "req-1", http.StatusBadRequest, apierror.BAD_REQUEST, "bad", false)
	if bytes.HasSuffix(w.Body.Bytes(), []byte{'\n'}) {
		t.Error("Write produced trailing newline; body must be canonical JSON without trailing whitespace")
	}
}

func TestWrite_CodeConstantsUnique(t *testing.T) {
	codes := []string{
		apierror.UNAUTHORIZED,
		apierror.INTERNAL,
		apierror.RATE_LIMITED,
		apierror.QUOTA_EXCEEDED,
		apierror.BAD_REQUEST,
		apierror.WORKER_UNREACHABLE,
		apierror.WORKER_BAD_RESPONSE,
		apierror.STRIPE_ERROR,
		apierror.NOT_CONFIGURED,
		apierror.PLAN_NOT_FOUND,
		apierror.NO_STRIPE_CUSTOMER,
		apierror.IDEMPOTENCY_CONFLICT,
		apierror.IDEMPOTENCY_KEY_REUSE,
		apierror.IDEMPOTENCY_KEY_INVALID,
	}
	seen := make(map[string]bool, len(codes))
	for _, c := range codes {
		if seen[c] {
			t.Errorf("duplicate constant value %q", c)
		}
		seen[c] = true
	}
}
