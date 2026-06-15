package errorlog

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

// mustReadBody reads all bytes from r.Body and fails the test on error.
func mustReadBody(t *testing.T, r *http.Request) string {
	t.Helper()
	b, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatalf("reading restored body: %v", err)
	}
	return string(b)
}

// errAfterReader emits all data bytes then returns io.ErrUnexpectedEOF.
type errAfterReader struct {
	data   []byte
	offset int
}

func (e *errAfterReader) Read(p []byte) (int, error) {
	if e.offset >= len(e.data) {
		return 0, io.ErrUnexpectedEOF
	}
	n := copy(p, e.data[e.offset:])
	e.offset += n
	if e.offset >= len(e.data) {
		return n, io.ErrUnexpectedEOF
	}
	return n, nil
}

// signalWriter captures zerolog output and closes done on first write.
type signalWriter struct {
	mu   sync.Mutex
	buf  bytes.Buffer
	once sync.Once
	done chan struct{}
}

func newSignalWriter() *signalWriter { return &signalWriter{done: make(chan struct{})} }

func (sw *signalWriter) Write(p []byte) (int, error) {
	sw.mu.Lock()
	n, err := sw.buf.Write(p)
	sw.mu.Unlock()
	if n > 0 {
		sw.once.Do(func() { close(sw.done) })
	}
	return n, err
}

func (sw *signalWriter) Bytes() []byte {
	sw.mu.Lock()
	defer sw.mu.Unlock()
	b := make([]byte, sw.buf.Len())
	copy(b, sw.buf.Bytes())
	return b
}

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
		t.Error("expected error from Hijack on non-hijacking writer, got nil")
	}
}

// TestMaybeCaptureRequestBody verifies the hot-path no-op, capture, truncation,
// and body-restoration invariants of MaybeCaptureRequestBody.
func TestMaybeCaptureRequestBody(t *testing.T) {
	makeReq := func(body string) *http.Request {
		r, _ := http.NewRequest(http.MethodPost, "/", strings.NewReader(body))
		return r
	}

	t.Run("off: returns nil without touching body", func(t *testing.T) {
		r := makeReq(`{"key":"value"}`)
		if got := MaybeCaptureRequestBody(r, 0); got != nil {
			t.Errorf("expected nil when maxBytes=0, got %q", got)
		}
		if body := mustReadBody(t, r); body != `{"key":"value"}` {
			t.Errorf("body was modified: got %q", body)
		}
	})

	t.Run("on: body fits within limit", func(t *testing.T) {
		const input = `{"hello":"world"}`
		r := makeReq(input)
		got := MaybeCaptureRequestBody(r, 4096)
		if string(got) != input {
			t.Errorf("payload mismatch: got %q, want %q", got, input)
		}
		if body := mustReadBody(t, r); body != input {
			t.Errorf("body not restored: got %q", body)
		}
	})

	t.Run("on: body exceeds limit, stored size <= maxBytes with marker", func(t *testing.T) {
		long := strings.Repeat("x", 100)
		const limit = len(payloadTruncationMarker) + 8
		r := makeReq(long)
		got := MaybeCaptureRequestBody(r, limit)
		if len(got) > limit {
			t.Errorf("stored payload len %d exceeds maxBytes %d", len(got), limit)
		}
		if !strings.HasSuffix(string(got), payloadTruncationMarker) {
			t.Errorf("expected truncation marker, got %q", got)
		}
		if body := mustReadBody(t, r); body != long {
			t.Errorf("body not restored after truncation: got %q", body)
		}
	})

	t.Run("on: truncation removes orphaned multi-byte lead byte at boundary", func(t *testing.T) {
		// Body: 8 ASCII bytes + 'é' (2-byte sequence 0xC3 0xA9) + 20 filler bytes.
		// limit = marker(12) + 9, so truncLen = 9. buf[8] = 0xC3 (lead byte, not
		// continuation), the loop stops, then the lead-byte check removes it.
		// Without the fix, 0xC3 would remain as a dangling lead byte (invalid UTF-8).
		body := strings.Repeat("a", 8) + "\xc3\xa9" + strings.Repeat("b", 20)
		const limit = len(payloadTruncationMarker) + 9
		r := makeReq(body)
		got := MaybeCaptureRequestBody(r, limit)
		if !strings.HasSuffix(string(got), payloadTruncationMarker) {
			t.Errorf("expected truncation marker, got %q", got)
		}
		prefix := strings.TrimSuffix(string(got), payloadTruncationMarker)
		if prefix != strings.Repeat("a", 8) {
			t.Errorf("orphaned lead byte not removed: prefix=%q, want 8 ASCII chars", prefix)
		}
	})

	t.Run("on: read error returns nil and restores partial body", func(t *testing.T) {
		partial := []byte("partial-data-before-error")
		r, _ := http.NewRequest(http.MethodPost, "/", &errAfterReader{data: partial})
		got := MaybeCaptureRequestBody(r, 4096)
		if got != nil {
			t.Errorf("expected nil on read error, got %q", got)
		}
		if restored := mustReadBody(t, r); restored != string(partial) {
			t.Errorf("r.Body not restored: got %q, want %q", restored, string(partial))
		}
	})
}

// TestRecord_GoroutineNeverLogsPayload verifies that the async insert goroutine
// never includes request payload bytes in log output.
func TestRecord_GoroutineNeverLogsPayload(t *testing.T) {
	pool, err := pgxpool.New(context.Background(),
		"postgres://x:x@127.0.0.1:59999/x?connect_timeout=1")
	if err != nil {
		t.Skipf("cannot create test pool: %v", err)
	}
	defer pool.Close()

	rec := New(pool)
	sw := newSignalWriter()
	origLogger := log.Logger
	log.Logger = zerolog.New(sw)
	t.Cleanup(func() { log.Logger = origLogger })

	sensitivePayload := []byte("SENSITIVE_SECRET_DATA_sentinel_abc123")
	rec.Record(context.Background(),
		uuid.New(), uuid.New(), "/v1/test", "ERR", "req-sentinel-1", "msg", 500,
		sensitivePayload)

	select {
	case <-sw.done:
	case <-time.After(5 * time.Second):
		t.Skip("goroutine did not log within 5 s; port 59999 may be in use")
	}
	output := sw.Bytes()
	if len(output) == 0 {
		t.Skip("no log output; port 59999 may be occupied")
	}
	if bytes.Contains(output, sensitivePayload) {
		t.Errorf("payload leaked into goroutine error log: %q", output)
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
	nilRec.Record(nil, [16]byte{}, [16]byte{}, "/v1/test", "ERR", "req-1", "msg", 500, nil)
}
