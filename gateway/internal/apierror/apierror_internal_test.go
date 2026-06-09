package apierror

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestWrite_FallbackPathPreservesCallerValues(t *testing.T) {
	orig := marshalJSON
	defer func() { marshalJSON = orig }()

	// First call (envelope) fails; second call (fallback struct) uses real json.Marshal.
	calls := 0
	marshalJSON = func(v any) ([]byte, error) {
		calls++
		if calls == 1 {
			return nil, errors.New("forced marshal failure")
		}
		return json.Marshal(v)
	}

	w := httptest.NewRecorder()
	Write(w, "test-rid", http.StatusTooManyRequests, RATE_LIMITED, "rate limited", true)

	if w.Code != http.StatusTooManyRequests {
		t.Errorf("status = %d, want %d", w.Code, http.StatusTooManyRequests)
	}
	var got struct {
		Error Error `json:"error"`
	}
	if err := json.NewDecoder(w.Body).Decode(&got); err != nil {
		t.Fatalf("fallback body not valid JSON: %v", err)
	}
	if got.Error.Code != RATE_LIMITED {
		t.Errorf("error.code = %q, want %q", got.Error.Code, RATE_LIMITED)
	}
	if got.Error.Message != "rate limited" {
		t.Errorf("error.message = %q, want %q", got.Error.Message, "rate limited")
	}
	if !got.Error.Retryable {
		t.Error("error.retryable = false, want true")
	}
	if got.Error.RequestID != "test-rid" {
		t.Errorf("error.request_id = %q, want %q", got.Error.RequestID, "test-rid")
	}
}

func TestWrite_DoubleFallbackPreservesCallerValues(t *testing.T) {
	orig := marshalJSON
	defer func() { marshalJSON = orig }()

	// Both marshalJSON calls fail; Write falls through to fmt.Sprintf with %q,
	// which cannot fail and preserves all caller values including requestID.
	marshalJSON = func(v any) ([]byte, error) {
		return nil, errors.New("forced marshal failure")
	}

	w := httptest.NewRecorder()
	// Use RATE_LIMITED (not INTERNAL) so we can verify the caller's code survives.
	Write(w, "test-rid-double", http.StatusTooManyRequests, RATE_LIMITED, "rate limited", true)

	if w.Code != http.StatusTooManyRequests {
		t.Errorf("status = %d, want %d", w.Code, http.StatusTooManyRequests)
	}
	var got struct {
		Error Error `json:"error"`
	}
	if err := json.NewDecoder(w.Body).Decode(&got); err != nil {
		t.Fatalf("triple-fallback body not valid JSON: %v", err)
	}
	if got.Error.Code != RATE_LIMITED {
		t.Errorf("error.code = %q, want %q", got.Error.Code, RATE_LIMITED)
	}
	if got.Error.Message != "rate limited" {
		t.Errorf("error.message = %q, want %q", got.Error.Message, "rate limited")
	}
	if !got.Error.Retryable {
		t.Error("error.retryable = false, want true")
	}
	if got.Error.RequestID != "test-rid-double" {
		t.Errorf("error.request_id = %q, want %q", got.Error.RequestID, "test-rid-double")
	}
}
