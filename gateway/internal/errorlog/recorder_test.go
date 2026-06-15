package errorlog

import (
	"io"
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

// TestMaybeCaptureRequestBody verifies body buffering, truncation, and hot-path no-op.
func TestMaybeCaptureRequestBody(t *testing.T) {
	makeReq := func(body string) *http.Request {
		r, _ := http.NewRequest(http.MethodPost, "/", strings.NewReader(body))
		return r
	}
	readBody := func(r *http.Request) string {
		b, _ := io.ReadAll(r.Body)
		return string(b)
	}

	t.Run("off: returns nil without touching body", func(t *testing.T) {
		r := makeReq(`{"key":"value"}`)
		got := MaybeCaptureRequestBody(r, 0)
		if got != nil {
			t.Errorf("expected nil when maxBytes=0, got %q", *got)
		}
		// Body must be fully intact — no buffering on the hot path.
		if body := readBody(r); body != `{"key":"value"}` {
			t.Errorf("body was modified: got %q", body)
		}
	})

	t.Run("on: body fits within limit", func(t *testing.T) {
		const input = `{"hello":"world"}`
		r := makeReq(input)
		got := MaybeCaptureRequestBody(r, 4096)
		if got == nil {
			t.Fatal("expected non-nil payload")
		}
		if *got != input {
			t.Errorf("payload mismatch: got %q, want %q", *got, input)
		}
		// r.Body must be restored so the downstream handler can still read it.
		if body := readBody(r); body != input {
			t.Errorf("body not restored: got %q, want %q", body, input)
		}
	})

	t.Run("on: body exceeds limit gets truncation marker", func(t *testing.T) {
		long := strings.Repeat("x", 100)
		r := makeReq(long)
		const limit = 10
		got := MaybeCaptureRequestBody(r, limit)
		if got == nil {
			t.Fatal("expected non-nil payload")
		}
		// Assert the exact output: first `limit` bytes of body + truncation marker.
		want := strings.Repeat("x", limit) + payloadTruncationMarker
		if *got != want {
			t.Errorf("got %q, want %q", *got, want)
		}
		// r.Body must still yield the full original body.
		if body := readBody(r); body != long {
			t.Errorf("body not fully restored after truncation: got %q", body)
		}
	})

	t.Run("nil body returns nil", func(t *testing.T) {
		r, _ := http.NewRequest(http.MethodPost, "/", nil)
		if got := MaybeCaptureRequestBody(r, 4096); got != nil {
			t.Errorf("expected nil for nil body, got %q", *got)
		}
	})
}

// TestNew_NilDB returns nil so callers can pass nil safely.
func TestNew_NilDB(t *testing.T) {
	r := New(nil)
	if r != nil {
		t.Error("expected nil ErrorRecorder for nil db")
	}
	// nil receiver Record must be a safe no-op.
	var nilRec *ErrorRecorder
	nilRec.Record(nil, [16]byte{}, [16]byte{}, "/v1/test", "ERR", "req-1", "msg", 500, nil)
}
