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

// overflowProbeBytes: reading maxBytes+1 detects overflow without buffering the full body.
const overflowProbeBytes = 1

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
// requestPayload is stored as BYTEA and must never appear in logs or metric labels.
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
	select {
	case r.sem <- struct{}{}:
	default:
		log.Warn().Str("request_id", requestID).Msg("error event dropped: concurrency limit reached")
		return
	}
	db, sem := r.db, r.sem
	cid, kid := customerID, apiKeyID
	op, code, rid, msg, status := operation, errorCode, requestID, message, httpStatus
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
		// api_key_id is nullable; zero UUID → nil to avoid a spurious FK reference.
		var apiKeyPtr *uuid.UUID
		if kid != (uuid.UUID{}) {
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

// MaybeCaptureRequestBody reads up to maxBytes from r.Body and restores r.Body
// for downstream handlers. Returns nil when maxBytes <= 0 (capture off, zero allocs).
// Truncates with payloadTruncationMarker at a valid UTF-8 boundary when body exceeds maxBytes.
// Returns nil on read error, restoring whatever bytes were read before the failure.
func MaybeCaptureRequestBody(r *http.Request, maxBytes int) []byte {
	if maxBytes <= 0 || r.Body == nil || r.Body == http.NoBody {
		return nil
	}
	originalBody := r.Body
	buf, err := io.ReadAll(io.LimitReader(originalBody, int64(maxBytes)+overflowProbeBytes))
	if err != nil {
		r.Body = io.NopCloser(bytes.NewReader(buf))
		log.Warn().Err(err).Msg("payload capture: error reading request body")
		return nil
	}
	r.Body = io.NopCloser(io.MultiReader(bytes.NewReader(buf), originalBody))
	if len(buf) > maxBytes {
		markerBytes := []byte(payloadTruncationMarker)
		truncLen := maxBytes - len(markerBytes)
		if truncLen < 0 {
			// maxBytes is smaller than the marker; return nil rather than store an
			// ambiguous prefix (config.Load prevents this in production).
			return nil
		}
		// Walk back past UTF-8 continuation bytes (0x80–0xBF) then invalid lead
		// bytes (0xC0–0xC1, 0xF5–0xFF) so the stored prefix is valid UTF-8.
		end := truncLen
		for end > 0 && (buf[end-1]&0xc0) == 0x80 {
			end--
		}
		if end > 0 {
			b := buf[end-1]
			if (b >= 0xc0 && b <= 0xc1) || b >= 0xf5 {
				end--
			}
		}
		out := make([]byte, 0, end+len(markerBytes))
		out = append(out, buf[:end]...)
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
