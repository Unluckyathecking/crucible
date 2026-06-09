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
				t.Fatalf("body is not valid JSON: %v", err)
			}
			if got.Error.Code != tt.code {
				t.Errorf("error.code = %q, want %q", got.Error.Code, tt.code)
			}
			if got.Error.Message != tt.message {
				t.Errorf("error.message = %q, want %q", got.Error.Message, tt.message)
			}
			if got.Error.Retryable != tt.retryable {
				t.Errorf("error.retryable = %v, want %v", got.Error.Retryable, tt.retryable)
			}
			if got.Error.RequestID != tt.requestID {
				t.Errorf("error.request_id = %q, want %q", got.Error.RequestID, tt.requestID)
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
	apierror.Write(w, "req-456", http.StatusInternalServerError, apierror.INTERNAL, "err", false)

	var top map[string]json.RawMessage
	if err := json.Unmarshal(w.Body.Bytes(), &top); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(top) != 1 {
		t.Fatalf("top-level keys = %d, want 1", len(top))
	}
	errRaw, ok := top["error"]
	if !ok {
		t.Fatal("missing top-level 'error' key")
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
		t.Fatalf("error object decode failed or has unknown field: %v", err)
	}
	if obj.Code != apierror.INTERNAL {
		t.Errorf("code = %q, want %q", obj.Code, apierror.INTERNAL)
	}
	if obj.Message != "err" {
		t.Errorf("message = %q, want %q", obj.Message, "err")
	}
	if obj.Retryable != false {
		t.Errorf("retryable = %v, want false", obj.Retryable)
	}
	if obj.RequestID != "req-456" {
		t.Errorf("request_id = %q, want %q", obj.RequestID, "req-456")
	}
}

// TestWrite_EmptyRequestIDEmittedAsEmptyString verifies that an empty-string
// requestID is serialised as "request_id":"" (key present, value empty) and
// not omitted. The OpenAPI schema marks request_id as required, so omitempty
// would be a schema violation.
func TestWrite_EmptyRequestIDEmittedAsEmptyString(t *testing.T) {
	w := httptest.NewRecorder()
	apierror.Write(w, "", http.StatusInternalServerError, apierror.INTERNAL, "err", false)

	var top map[string]json.RawMessage
	if err := json.Unmarshal(w.Body.Bytes(), &top); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	var inner map[string]json.RawMessage
	if err := json.Unmarshal(top["error"], &inner); err != nil {
		t.Fatalf("unmarshal error object: %v", err)
	}
	rid, ok := inner["request_id"]
	if !ok {
		t.Fatal("request_id field missing from error object")
	}
	if string(rid) != `""` {
		t.Errorf("request_id = %s, want empty string \"\"", rid)
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
