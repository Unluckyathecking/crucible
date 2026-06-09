// Tests in this file MUST NOT run with t.Parallel() — they mutate the package-level
// marshalJSON var and restore it via defer. Concurrent mutation would cause data races.
package apierror

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestWrite_OverwritesContentTypeAndCacheControl documents that Write takes
// unconditional ownership of Content-Type and Cache-Control. Any pre-existing
// values for those two headers are replaced; this is intentional — error
// responses must always be JSON and must never be cached.
func TestWrite_OverwritesContentTypeAndCacheControl(t *testing.T) {
	w := httptest.NewRecorder()
	w.Header().Set("Content-Type", "text/plain")
	w.Header().Set("Cache-Control", "max-age=3600")

	Write(w, "rid", http.StatusBadRequest, BAD_REQUEST, "bad", false)

	if got := w.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json; Write must override caller's value", got)
	}
	if got := w.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store; Write must override caller's value", got)
	}
}

// TestWrite_DoesNotClobberOtherResponseHeaders verifies that Write's unconditional
// setting of Content-Type and Cache-Control does not disturb any other headers the
// caller has already set. In practice this protects the Retry-After and X-RateLimit-*
// headers written by ratelimit.Middleware immediately before it calls Write.
// Write takes ownership of Content-Type and Cache-Control only.
func TestWrite_DoesNotClobberOtherResponseHeaders(t *testing.T) {
	w := httptest.NewRecorder()
	w.Header().Set("Retry-After", "60")
	w.Header().Set("X-Custom", "preserved")

	Write(w, "rid", http.StatusTooManyRequests, RATE_LIMITED, "rate limited", true)

	if got := w.Header().Get("Retry-After"); got != "60" {
		t.Errorf("Retry-After = %q after Write, want %q; Write must not touch non-owned headers", got, "60")
	}
	if got := w.Header().Get("X-Custom"); got != "preserved" {
		t.Errorf("X-Custom = %q after Write, want %q; Write must not touch non-owned headers", got, "preserved")
	}
	if got := w.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got)
	}
	if got := w.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}
}

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
		t.Fatalf("double-fallback body not valid JSON: %v", err)
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
