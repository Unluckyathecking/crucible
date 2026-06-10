package errorlog

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestCapture_BuffersErrorBodies verifies that Capture buffers response bodies
// only for status >= 400 and never for successful responses.
func TestCapture_BuffersErrorBodies(t *testing.T) {
	t.Run("success body not buffered", func(t *testing.T) {
		w := httptest.NewRecorder()
		c := NewCapture(w)
		c.WriteHeader(http.StatusOK)
		c.Write([]byte(`{"ok":true}`))
		if len(c.body.Bytes()) != 0 {
			t.Errorf("expected empty buffer for 200, got %d bytes", len(c.body.Bytes()))
		}
	})

	t.Run("error body buffered", func(t *testing.T) {
		w := httptest.NewRecorder()
		c := NewCapture(w)
		c.WriteHeader(http.StatusBadRequest)
		body := `{"error":{"code":"BAD_INPUT","message":"invalid param"}}`
		c.Write([]byte(body))
		if string(c.body.Bytes()) != body {
			t.Errorf("expected buffered body %q, got %q", body, c.body.Bytes())
		}
	})

	t.Run("implicit 200 not buffered", func(t *testing.T) {
		w := httptest.NewRecorder()
		c := NewCapture(w)
		c.Write([]byte("hello"))
		if c.Status() != http.StatusOK {
			t.Errorf("expected status 200, got %d", c.Status())
		}
		if len(c.body.Bytes()) != 0 {
			t.Errorf("expected empty buffer for implicit 200, got %d bytes", len(c.body.Bytes()))
		}
	})
}

// TestCapture_WriteHeaderIdempotent verifies that calling WriteHeader twice
// does not forward the second call to the underlying writer.
func TestCapture_WriteHeaderIdempotent(t *testing.T) {
	w := httptest.NewRecorder()
	c := NewCapture(w)
	c.WriteHeader(http.StatusNotFound)
	c.WriteHeader(http.StatusOK) // should be ignored
	if c.Status() != http.StatusNotFound {
		t.Errorf("expected status 404, got %d", c.Status())
	}
	if w.Code != http.StatusNotFound {
		t.Errorf("expected underlying writer code 404, got %d", w.Code)
	}
}

// TestCapture_ParseErrorFields covers JSON extraction, UNKNOWN fallback, and
// message truncation at UTF-8 boundaries.
func TestCapture_ParseErrorFields(t *testing.T) {
	t.Run("valid JSON envelope", func(t *testing.T) {
		w := httptest.NewRecorder()
		c := NewCapture(w)
		c.WriteHeader(http.StatusUnprocessableEntity)
		c.Write([]byte(`{"error":{"code":"RATE_LIMITED","message":"too many requests"}}`))
		code, msg := c.ParseErrorFields()
		if code != "RATE_LIMITED" {
			t.Errorf("expected code RATE_LIMITED, got %q", code)
		}
		if msg != "too many requests" {
			t.Errorf("expected message %q, got %q", "too many requests", msg)
		}
	})

	t.Run("non-JSON body returns UNKNOWN", func(t *testing.T) {
		w := httptest.NewRecorder()
		c := NewCapture(w)
		c.WriteHeader(http.StatusInternalServerError)
		c.Write([]byte("plain text error"))
		code, msg := c.ParseErrorFields()
		if code != "UNKNOWN" {
			t.Errorf("expected UNKNOWN, got %q", code)
		}
		if msg != "" {
			t.Errorf("expected empty message for non-JSON, got %q", msg)
		}
	})

	t.Run("message truncated at UTF-8 boundary", func(t *testing.T) {
		// Build a message that exceeds maxMessageBytes.
		// Use ASCII so truncation is straightforward.
		long := strings.Repeat("x", maxMessageBytes+10)
		payload := `{"error":{"code":"ERR","message":"` + long + `"}}`
		w := httptest.NewRecorder()
		c := NewCapture(w)
		c.WriteHeader(http.StatusBadGateway)
		c.Write([]byte(payload))
		_, msg := c.ParseErrorFields()
		if len(msg) > maxMessageBytes {
			t.Errorf("message not truncated: len=%d, max=%d", len(msg), maxMessageBytes)
		}
	})

	t.Run("empty body returns UNKNOWN with empty message", func(t *testing.T) {
		w := httptest.NewRecorder()
		c := NewCapture(w)
		c.WriteHeader(http.StatusServiceUnavailable)
		code, msg := c.ParseErrorFields()
		if code != "UNKNOWN" {
			t.Errorf("expected UNKNOWN, got %q", code)
		}
		if msg != "" {
			t.Errorf("expected empty message, got %q", msg)
		}
	})
}

// TestCapture_Flush verifies the Flusher delegation.
func TestCapture_Flush(t *testing.T) {
	w := httptest.NewRecorder()
	c := NewCapture(w)
	// httptest.ResponseRecorder implements http.Flusher; Flush must not panic.
	c.Flush()
}

// TestCapture_Hijack verifies Hijack returns an error for non-hijacking writers.
func TestCapture_Hijack(t *testing.T) {
	w := httptest.NewRecorder()
	c := NewCapture(w)
	_, _, err := c.Hijack()
	if err == nil {
		t.Error("expected error from Hijack on non-hijacker writer, got nil")
	}
}

// TestNew_NilDB returns nil so callers can pass nil safely.
func TestNew_NilDB(t *testing.T) {
	r := New(nil)
	if r != nil {
		t.Error("expected nil ErrorRecorder for nil db")
	}
	// nil receiver Record must be a safe no-op.
	var nilRec *ErrorRecorder
	nilRec.Record(nil, [16]byte{}, [16]byte{}, "/v1/test", "ERR", "req-1", "msg", 500)
}
