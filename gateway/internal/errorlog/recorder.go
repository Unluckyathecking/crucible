// Package errorlog records non-2xx /v1 response events into the durable
// error_events table for customer-facing error-history inspection.
//
// Design invariants:
//   - A nil *ErrorRecorder is always a safe no-op so callers need no nil guard.
//   - Record is non-blocking: it queues work on a bounded semaphore channel and
//     returns immediately, never blocking or altering the HTTP response path.
//   - Goroutines are capped at maxConcurrent; when the cap is reached, the
//     event is dropped with a warning log rather than blocking the gateway.
//   - Each goroutine uses a fresh background context; a cancelled request context
//     never aborts an in-flight insert.
//   - Capture buffers only error response bodies (status >= 400) so successful
//     large responses are not copied in memory.
package errorlog

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/zerolog/log"
)

// maxConcurrent caps the number of in-flight error-event insert goroutines.
// Events that arrive when all slots are occupied are dropped rather than
// blocking the gateway or opening unbounded DB connections.
const (
	maxConcurrent   = 128
	dbTimeout       = 2 * time.Second
	maxMessageBytes = 1024
)

// truncationProbeBytes is the extra byte read past maxBytes to detect whether
// the body exceeds the capture limit without buffering the full body.
const truncationProbeBytes = 1

// ErrorRecorder writes error_events rows asynchronously with a bounded
// goroutine pool. Fields are immutable after New returns; goroutines capture
// the receiver by pointer but never mutate it.
type ErrorRecorder struct {
	db  *pgxpool.Pool
	sem chan struct{}
}

// New returns an ErrorRecorder backed by db. Returns nil when db is nil so
// callers can pass nil safely and rely on the no-op receiver below.
func New(db *pgxpool.Pool) *ErrorRecorder {
	if db == nil {
		return nil
	}
	return &ErrorRecorder{
		db:  db,
		sem: make(chan struct{}, maxConcurrent),
	}
}

// Record queues an async error_events insert. A nil receiver is a safe no-op.
// The call returns immediately regardless of DB load.
// The ctx parameter is accepted for API symmetry but is not forwarded to the
// goroutine — recording uses a fresh background context so a cancelled request
// context does not abort the write.
// requestPayload is stored verbatim (as BYTEA) when non-nil; the caller is
// responsible for bounding its size (see MaybeCaptureRequestBody). It must
// never appear in logs or metric labels — only in the auth-gated dashboard surface.
func (r *ErrorRecorder) Record(
	_ context.Context,
	customerID, apiKeyID uuid.UUID,
	operation, errorCode, requestID, message string,
	httpStatus int,
	requestPayload []byte,
) {
	if r == nil {
		return
	}
	// Acquire a goroutine slot. Non-blocking: drop the event when at capacity
	// rather than queuing unboundedly under a 4xx/5xx traffic spike.
	select {
	case r.sem <- struct{}{}:
	default:
		log.Warn().Str("request_id", requestID).Msg("error event dropped: concurrency limit reached")
		return
	}
	// Copy all fields by value — including receiver fields — before launching
	// the goroutine. Explicit local copies of db and sem mean a future Close()
	// method on ErrorRecorder can nil/close those fields without racing goroutines.
	db, sem := r.db, r.sem
	cid, kid := customerID, apiKeyID
	op, code, rid, msg, status := operation, errorCode, requestID, message, httpStatus
	// Copy the payload slice so the goroutine is not aliased to caller memory.
	// nil stays nil (→ DB NULL); non-nil gets an independent backing array.
	var payloadCopy []byte
	if requestPayload != nil {
		payloadCopy = make([]byte, len(requestPayload))
		copy(payloadCopy, requestPayload)
	}
	go func() {
		defer func() {
			if p := recover(); p != nil {
				log.Error().Str("request_id", rid).Interface("panic", p).Msg("error event goroutine panicked")
			}
			<-sem
		}()
		ctx, cancel := context.WithTimeout(context.Background(), dbTimeout)
		defer cancel()
		// api_key_id is nullable; pass nil for the zero UUID to avoid a spurious FK reference.
		var apiKeyPtr *uuid.UUID
		if kid != uuid.Nil {
			apiKeyPtr = &kid
		}
		_, err := db.Exec(ctx, `
			INSERT INTO error_events
			  (customer_id, api_key_id, operation, error_code, http_status, message, request_id, request_payload)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		`, cid, apiKeyPtr, op, code, status, msg, rid, payloadCopy)
		if err != nil {
			log.Warn().Err(err).Str("request_id", rid).Msg("error event record failed")
		}
	}()
}

// payloadTruncationMarker is appended to payloads that exceed the max byte
// limit so consumers can distinguish a complete body from a truncated one.
const payloadTruncationMarker = " [TRUNCATED]"

// MaybeCaptureRequestBody reads up to maxBytes from r.Body, restores r.Body
// so downstream handlers can still consume it, and returns the (possibly
// truncated) payload as a byte slice suitable for insertion into a BYTEA column.
//
// When maxBytes <= 0 the function is a no-op: it returns nil without touching
// r.Body, performing zero allocations. This is the behaviour when the
// ERROR_PAYLOAD_CAPTURE config flag is off.
//
// When the body exceeds maxBytes the returned slice is
// buf[:maxBytes-len(marker)] + marker so the total stored size never exceeds
// maxBytes bytes (the request body is not assumed to be valid UTF-8 or JSON;
// BYTEA stores it verbatim).
func MaybeCaptureRequestBody(r *http.Request, maxBytes int) []byte {
	if maxBytes <= 0 || r.Body == nil || r.Body == http.NoBody {
		return nil
	}
	// Read at most maxBytes+truncationProbeBytes to detect whether body exceeds
	// maxBytes without buffering the full body.
	// io.LimitReader.Read truncates the caller's buffer to at most remaining bytes
	// before calling the underlying Read, so r.Body cannot advance beyond the limit
	// regardless of io.ReadAll's internal buffer sizing.
	buf, err := io.ReadAll(io.LimitReader(r.Body, int64(maxBytes)+truncationProbeBytes))
	if err != nil {
		// Partial read — restore whatever bytes were consumed before the error
		// so the downstream handler sees the full original body.
		r.Body = io.NopCloser(io.MultiReader(bytes.NewReader(buf), r.Body))
		log.Warn().Err(err).Msg("payload capture: error reading request body")
		return nil
	}
	// Restore r.Body so downstream handlers can still read it.
	// io.MultiReader concatenates the bytes we consumed with whatever remains
	// in the original body (empty when body length <= maxBytes+1).
	r.Body = io.NopCloser(io.MultiReader(bytes.NewReader(buf), r.Body))
	if len(buf) > maxBytes {
		markerBytes := []byte(payloadTruncationMarker)
		truncLen := maxBytes - len(markerBytes)
		if truncLen < 0 {
			// maxBytes is too small to fit the marker; store raw prefix without it
			// so total size is exactly maxBytes and the contract is upheld.
			out := make([]byte, maxBytes)
			copy(out, buf)
			return out
		}
		out := make([]byte, 0, truncLen+len(markerBytes))
		out = append(out, buf[:truncLen]...)
		out = append(out, markerBytes...)
		return out
	}
	out := make([]byte, len(buf))
	copy(out, buf)
	return out
}

// Capture wraps http.ResponseWriter, buffering the response body for error
// responses (status >= 400) so the caller can parse the apierror envelope.
//
// Not goroutine-safe — matches the http.ResponseWriter contract.
// Preserves http.Flusher and http.Hijacker by delegating to the underlying
// writer when those interfaces are supported.
type Capture struct {
	http.ResponseWriter
	status int
	body   bytes.Buffer
	wrote  bool
}

// Compile-time assertions: *Capture must implement optional ResponseWriter interfaces.
var _ http.Flusher = (*Capture)(nil)
var _ http.Hijacker = (*Capture)(nil)

// NewCapture wraps w with a Capture recorder seeded to 200.
func NewCapture(w http.ResponseWriter) *Capture {
	return &Capture{ResponseWriter: w, status: http.StatusOK}
}

// Status returns the recorded HTTP status code.
func (c *Capture) Status() int { return c.status }

// WriteHeader forwards code and records it on the first call.
// Subsequent calls are silently dropped so the wrapper never triggers the
// "superfluous response.WriteHeader" warning on the underlying writer.
func (c *Capture) WriteHeader(code int) {
	if c.wrote {
		return
	}
	c.status = code
	c.wrote = true
	c.ResponseWriter.WriteHeader(code)
}

// Write forwards b and buffers it when status >= 400.
// The underlying http.ResponseWriter.Write handles the implicit 200 WriteHeader
// if no status has been set; Capture mirrors that state without double-calling.
func (c *Capture) Write(b []byte) (int, error) {
	if !c.wrote {
		c.status = http.StatusOK
		c.wrote = true
	}
	if c.status >= 400 {
		c.body.Write(b)
	}
	return c.ResponseWriter.Write(b)
}

// Flush delegates to the underlying writer when it supports http.Flusher,
// preserving the optional interface through the wrapper.
func (c *Capture) Flush() {
	if f, ok := c.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Hijack delegates to the underlying writer when it supports http.Hijacker.
// Returns an error rather than panicking when the underlying writer does not
// implement the interface.
func (c *Capture) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if h, ok := c.ResponseWriter.(http.Hijacker); ok {
		return h.Hijack()
	}
	return nil, nil, fmt.Errorf("errorlog: underlying ResponseWriter does not implement http.Hijacker")
}

// errorEnvelope mirrors the apierror package's JSON shape for unmarshalling only.
type errorEnvelope struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// ParseErrorFields extracts the error code and message from a buffered apierror
// response body. Only the apierror envelope fields are stored; non-JSON bodies
// (HTML from upstreams, plain-text proxy errors, stack traces) are silently
// discarded so customers never see internal diagnostic content.
// Returns code="UNKNOWN" when the body is absent or the JSON envelope has no code.
func (c *Capture) ParseErrorFields() (code, message string) {
	body := c.body.Bytes()
	var env errorEnvelope
	if err := json.Unmarshal(body, &env); err == nil {
		code = env.Error.Code
		message = env.Error.Message
	}
	if code == "" {
		code = "UNKNOWN"
	}
	if len(message) > maxMessageBytes {
		// Walk back to the last valid rune start, then confirm the prefix is
		// valid UTF-8. Invalid bytes in the middle of the string (e.g. from a
		// non-UTF-8 worker response) would otherwise produce an invalid slice.
		i := maxMessageBytes
		for i > 0 && !utf8.RuneStart(message[i]) {
			i--
		}
		if !utf8.ValidString(message[:i]) {
			i = 0
		}
		message = message[:i]
	}
	return code, message
}
