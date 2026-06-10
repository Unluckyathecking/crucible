// Package errorlog records non-2xx /v1 response events into the durable
// error_events table for customer-facing error-history inspection.
//
// Design invariants:
//   - A nil *ErrorRecorder is always a safe no-op so callers need no nil guard.
//   - Record fires a goroutine and returns immediately; it never alters the HTTP
//     response already committed to the client.
//   - The goroutine uses a fresh background context: a cancelled request context
//     must not abort an in-flight insert.
//   - Capture buffers only error response bodies (status >= 400) so successful
//     large responses are not copied in memory.
package errorlog

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/zerolog/log"
)

// ErrorRecorder writes error_events rows asynchronously.
type ErrorRecorder struct {
	db *pgxpool.Pool
}

// New returns an ErrorRecorder backed by db. Returns nil when db is nil so
// callers can pass nil safely and rely on the no-op receiver below.
func New(db *pgxpool.Pool) *ErrorRecorder {
	if db == nil {
		return nil
	}
	return &ErrorRecorder{db: db}
}

// Record asynchronously inserts one error_events row.
// A nil receiver is a safe no-op.
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
	// Capture all fields by value before the goroutine starts — the caller's
	// stack frame may be gone before the goroutine runs.
	cid := customerID
	kid := apiKeyID
	op, code, rid, msg := operation, errorCode, requestID, message
	status := httpStatus
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_, err := r.db.Exec(ctx, `
			INSERT INTO error_events
			  (customer_id, api_key_id, operation, error_code, http_status, message, request_id)
			VALUES ($1, $2, $3, $4, $5, $6, $7)
		`, cid, kid, op, code, status, msg, rid)
		if err != nil {
			log.Warn().Err(err).Str("request_id", rid).Msg("error event record failed")
		}
	}()
}

// Capture wraps http.ResponseWriter, buffering the response body for error
// responses (status >= 400) so the caller can parse the apierror envelope.
//
// Not goroutine-safe — matches the http.ResponseWriter contract.
type Capture struct {
	http.ResponseWriter
	status int
	body   bytes.Buffer
	wrote  bool
}

// Compile-time assertion: *Capture must implement http.Flusher.
var _ http.Flusher = (*Capture)(nil)

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
// An implicit 200 is recorded if WriteHeader was never called.
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

// errorEnvelope mirrors the apierror package's JSON shape for unmarshalling only.
type errorEnvelope struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// ParseErrorFields extracts the error code and message from a buffered apierror
// response body. Returns ("UNKNOWN", "") when the body is absent or malformed.
func (c *Capture) ParseErrorFields() (code, message string) {
	var env errorEnvelope
	if err := json.Unmarshal(c.body.Bytes(), &env); err == nil {
		code = env.Error.Code
		message = env.Error.Message
	}
	if code == "" {
		code = "UNKNOWN"
	}
	return code, message
}
