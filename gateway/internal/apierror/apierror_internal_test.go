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

// writeOrderRecorder wraps ResponseRecorder to detect if Write is called before WriteHeader.
// http.ResponseWriter requires WriteHeader to precede Write; reversing this triggers an
// implicit WriteHeader(200) which corrupts the response status.
type writeOrderRecorder struct {
	*httptest.ResponseRecorder
	headerWritten           bool
	writeCalledBeforeHeader bool
}

func (r *writeOrderRecorder) WriteHeader(code int) {
	r.headerWritten = true
	r.ResponseRecorder.WriteHeader(code)
}

func (r *writeOrderRecorder) Write(b []byte) (int, error) {
	if !r.headerWritten {
		r.writeCalledBeforeHeader = true
	}
	return r.ResponseRecorder.Write(b)
}

// TestWrite_WriteHeaderBeforeWrite verifies the http.ResponseWriter contract on the normal
// path and on every fallback path: WriteHeader must be called before Write.
func TestWrite_WriteHeaderBeforeWrite(t *testing.T) {
	t.Run("normal path", func(t *testing.T) {
		w := &writeOrderRecorder{ResponseRecorder: httptest.NewRecorder()}
		Write(w, "rid", http.StatusUnauthorized, UNAUTHORIZED, "invalid key", false)
		if w.writeCalledBeforeHeader {
			t.Error("Write was called before WriteHeader on normal path")
		}
		if w.Code != http.StatusUnauthorized {
			t.Errorf("status = %d, want %d", w.Code, http.StatusUnauthorized)
		}
	})

	t.Run("first-marshal-fails fallback", func(t *testing.T) {
		orig := marshalJSON
		defer func() { marshalJSON = orig }()
		calls := 0
		marshalJSON = func(v any) ([]byte, error) {
			calls++
			if calls == 1 {
				return nil, errors.New("forced")
			}
			return json.Marshal(v)
		}
		w := &writeOrderRecorder{ResponseRecorder: httptest.NewRecorder()}
		Write(w, "rid", http.StatusTooManyRequests, RATE_LIMITED, "rate limited", true)
		if w.writeCalledBeforeHeader {
			t.Error("Write was called before WriteHeader on first-marshal-fails fallback")
		}
		if w.Code != http.StatusTooManyRequests {
			t.Errorf("status = %d, want %d", w.Code, http.StatusTooManyRequests)
		}
	})

	t.Run("both-marshals-fail fallback", func(t *testing.T) {
		orig := marshalJSON
		defer func() { marshalJSON = orig }()
		marshalJSON = func(v any) ([]byte, error) {
			return nil, errors.New("forced")
		}
		w := &writeOrderRecorder{ResponseRecorder: httptest.NewRecorder()}
		Write(w, "rid", http.StatusInternalServerError, INTERNAL, "err", false)
		if w.writeCalledBeforeHeader {
			t.Error("Write was called before WriteHeader on both-marshals-fail fallback")
		}
		if w.Code != http.StatusInternalServerError {
			t.Errorf("status = %d, want %d", w.Code, http.StatusInternalServerError)
		}
		if w.Body.Len() == 0 {
			t.Error("body is empty on both-marshals-fail fallback; expected non-empty JSON")
		}
	})
}

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

	// marshalJSON fails on the first call; Write falls back to real json.Marshal directly.
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

	// marshalJSON fails; Write falls through to real json.Marshal on the same
	// envelope, which bypasses the injected marshalJSON and cannot fail.
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
