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

func TestWrite_DoubleFallbackEmitsValidJSON(t *testing.T) {
	orig := marshalJSON
	defer func() { marshalJSON = orig }()

	// Both marshal calls fail; static hardcoded literal must be valid JSON.
	marshalJSON = func(v any) ([]byte, error) {
		return nil, errors.New("forced marshal failure")
	}

	w := httptest.NewRecorder()
	Write(w, "rid", http.StatusInternalServerError, INTERNAL, "err", false)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", w.Code)
	}
	var got struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.NewDecoder(w.Body).Decode(&got); err != nil {
		t.Fatalf("static fallback not valid JSON: %v", err)
	}
	if got.Error.Code != INTERNAL {
		t.Errorf("error.code = %q, want INTERNAL", got.Error.Code)
	}
}
