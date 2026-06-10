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
func (r *ErrorRecorder) Record(
	_ context.Context,
	customerID, apiKeyID uuid.UUID,
	operation, errorCode, requestID, message string,
	httpStatus int,
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
	// Copy all fields by value before launching the goroutine so the caller's
	// stack frame is free to return without aliasing issues.
	cid, kid := customerID, apiKeyID
	op, code, rid, msg, status := operation, errorCode, requestID, message, httpStatus
	go func() {
		defer func() { <-r.sem }()
		ctx, cancel := context.WithTimeout(context.Background(), dbTimeout)
		defer cancel()
		// api_key_id is nullable; pass nil for the zero UUID to avoid a spurious FK reference.
		var apiKeyPtr *uuid.UUID
		if kid != uuid.Nil {
			apiKeyPtr = &kid
		}
		_, err := r.db.Exec(ctx, `
			INSERT INTO error_events
			  (customer_id, api_key_id, operation, error_code, http_status, message, request_id)
			VALUES ($1, $2, $3, $4, $5, $6, $7)
		`, cid, apiKeyPtr, op, code, status, msg, rid)
		if err != nil {
			log.Warn().Err(err).Str("request_id", rid).Msg("error event record failed")
		}
	}()
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
func (c *Capture) WriteHeader(code int) {
	if !c.wrote {
		c.status = code
		c.wrote = true
	}
	c.ResponseWriter.WriteHeader(code)
}

// Write forwards b and buffers it when status >= 400.
// When called before WriteHeader, we record the implicit 200 locally but do NOT
// call WriteHeader on the underlying writer — the first underlying Write triggers
// it automatically, preserving Content-Length / Transfer-Encoding negotiation.
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
// response body. When JSON parsing fails but the body is non-empty, the raw body
// is preserved (capped at maxMessageBytes) so diagnostic information is not lost.
// Returns code="UNKNOWN" when the body is absent or the JSON envelope has no code.
func (c *Capture) ParseErrorFields() (code, message string) {
	body := c.body.Bytes()
	var env errorEnvelope
	if err := json.Unmarshal(body, &env); err == nil {
		code = env.Error.Code
		message = env.Error.Message
	} else if len(body) > 0 {
		message = string(body)
	}
	if code == "" {
		code = "UNKNOWN"
	}
	if len(message) > maxMessageBytes {
		// Walk back to the last valid rune start so we never emit partial UTF-8.
		i := maxMessageBytes
		for i > 0 && !utf8.RuneStart(message[i]) {
			i--
		}
		message = message[:i]
	}
	return code, message
}
