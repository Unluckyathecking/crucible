package errorlog

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"
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

	t.Run("message truncated before multi-byte rune straddling boundary", func(t *testing.T) {
		// Place a 2-byte rune (é = U+00E9 = 0xC3 0xA9) so that its start byte
		// lands at position maxMessageBytes-1 and its continuation byte at
		// maxMessageBytes. The truncation logic must walk back past the
		// continuation byte and exclude the incomplete rune, yielding a
		// valid UTF-8 prefix of length maxMessageBytes-1.
		prefix := strings.Repeat("a", maxMessageBytes-1)
		// é encodes as 2 bytes; trailing "z" pushes len past maxMessageBytes.
		long := prefix + "éz"
		payload := `{"error":{"code":"ERR","message":"` + long + `"}}`
		w := httptest.NewRecorder()
		c := NewCapture(w)
		c.WriteHeader(http.StatusBadGateway)
		c.Write([]byte(payload))
		_, msg := c.ParseErrorFields()
		if !utf8.ValidString(msg) {
			t.Errorf("truncated message is not valid UTF-8: %q", msg)
		}
		if len(msg) > maxMessageBytes {
			t.Errorf("message not truncated: len=%d, max=%d", len(msg), maxMessageBytes)
		}
		// The partial é (continuation byte 0xA9) must not appear in the output.
		if len(msg) > 0 && msg[len(msg)-1] >= 0x80 && msg[len(msg)-1] <= 0xBF {
			t.Errorf("message ends with a bare continuation byte: 0x%02x", msg[len(msg)-1])
		}
		// Verify the truncated string is the correct ASCII prefix.
		if msg != prefix {
			t.Errorf("expected %q, got %q", prefix, msg)
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
			t.Errorf("expected nil when maxBytes=0, got %q", got)
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
		if string(got) != input {
			t.Errorf("payload mismatch: got %q, want %q", got, input)
		}
		// r.Body must be restored so the downstream handler can still read it.
		if body := readBody(r); body != input {
			t.Errorf("body not restored: got %q, want %q", body, input)
		}
	})

	t.Run("on: body exceeds limit, stored size <= maxBytes and marker present", func(t *testing.T) {
		long := strings.Repeat("x", 100)
		r := makeReq(long)
		// Calculated so truncLen > 0 regardless of marker length changes.
		const limit = len(payloadTruncationMarker) + 8
		got := MaybeCaptureRequestBody(r, limit)
		if got == nil {
			t.Fatal("expected non-nil payload")
		}
		// Total stored size must not exceed limit.
		if len(got) > limit {
			t.Errorf("stored payload len %d exceeds maxBytes %d", len(got), limit)
		}
		// Truncation marker must appear at the end so consumers can distinguish
		// truncated from complete payloads without relying on exact size.
		if !strings.HasSuffix(string(got), payloadTruncationMarker) {
			t.Errorf("expected truncation marker suffix %q, got %q", payloadTruncationMarker, got)
		}
		// Exact expected value: buf[:limit-markerLen] + marker.
		markerLen := len(payloadTruncationMarker)
		want := []byte(strings.Repeat("x", limit-markerLen) + payloadTruncationMarker)
		if string(got) != string(want) {
			t.Errorf("got %q, want %q", got, want)
		}
		// r.Body must still yield the full original body.
		if body := readBody(r); body != long {
			t.Errorf("body not fully restored after truncation: got %q", body)
		}
	})

	t.Run("nil body returns nil", func(t *testing.T) {
		r, _ := http.NewRequest(http.MethodPost, "/", nil)
		if got := MaybeCaptureRequestBody(r, 4096); got != nil {
			t.Errorf("expected nil for nil body, got %q", got)
		}
	})

	t.Run("http.NoBody returns nil", func(t *testing.T) {
		r, _ := http.NewRequest(http.MethodPost, "/", http.NoBody)
		if got := MaybeCaptureRequestBody(r, 4096); got != nil {
			t.Errorf("expected nil for http.NoBody, got %q", got)
		}
	})

	t.Run("on: truncLen<0 path returns raw prefix without marker", func(t *testing.T) {
		// When maxBytes < len(payloadTruncationMarker), truncLen is negative and the
		// code returns the raw first maxBytes bytes without appending the marker
		// (config.Load prevents this in production via the >= 13 byte minimum check).
		// BYTEA semantics: raw bytes are stored as-is; UTF-8 alignment is the display
		// layer's responsibility (see truncateUtf8Buffer in route.ts).
		body := []byte{0x61, 0xF0, 0x9F, 0x98, 0x80, 0x62} // "a😀b" (6 bytes)
		r, _ := http.NewRequest(http.MethodPost, "/", bytes.NewReader(body))
		// maxBytes=5 < len(marker)=12 → truncLen < 0 → raw first 5 bytes, no marker.
		got := MaybeCaptureRequestBody(r, 5)
		if !bytes.Equal(got, body[:5]) {
			t.Errorf("got %x, want raw prefix %x", got, body[:5])
		}
		if len(got) > 5 {
			t.Errorf("stored len %d exceeds maxBytes 5", len(got))
		}
		restored, _ := io.ReadAll(r.Body)
		if !bytes.Equal(restored, body) {
			t.Errorf("body not restored: got %x", restored)
		}
	})

	t.Run("on: invalid UTF-8 bytes stored verbatim and body restored", func(t *testing.T) {
		// Verify BYTEA semantics: arbitrary bytes are captured as-is without
		// re-encoding. The dashboard layer converts to UTF-8 with replacement chars.
		invalidUtf8 := []byte{0xC3, 0x28, 0xFF, 0xFE, 0x00, 0x41}
		r, _ := http.NewRequest(http.MethodPost, "/", bytes.NewReader(invalidUtf8))
		got := MaybeCaptureRequestBody(r, 4096)
		if !bytes.Equal(got, invalidUtf8) {
			t.Errorf("payload mismatch: got %x, want %x", got, invalidUtf8)
		}
		restored, _ := io.ReadAll(r.Body)
		if !bytes.Equal(restored, invalidUtf8) {
			t.Errorf("body not restored: got %x, want %x", restored, invalidUtf8)
		}
	})
}

// TestNew_NilDB returns nil so callers can pass nil safely.
func TestNew_NilDB(t *testing.T) {
	r := New(nil)
	if r != nil {
		t.Error("expected nil ErrorRecorder for nil db")
	}
	// nil receiver Record must be a safe no-op — verify no panic occurs.
	func() {
		defer func() {
			if p := recover(); p != nil {
				t.Errorf("nil receiver Record panicked: %v", p)
			}
		}()
		var nilRec *ErrorRecorder
		nilRec.Record(nil, [16]byte{}, [16]byte{}, "/v1/test", "ERR", "req-1", "msg", 500, nil)
		payload := []byte("test-payload")
		nilRec.Record(nil, [16]byte{}, [16]byte{}, "/v1/test", "ERR", "req-1", "msg", 500, payload)
	}()
}
