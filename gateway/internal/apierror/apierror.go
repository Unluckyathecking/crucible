// Package apierror provides the canonical JSON error-response envelope for the Crucible gateway.
// Every handler emits errors through Write; no ad-hoc JSON literals elsewhere.
package apierror

import (
	"encoding/json"
	"net/http"
)

// Error code constants — byte-identical to the strings customers receive.
const (
	UNAUTHORIZED            = "UNAUTHORIZED"
	INTERNAL                = "INTERNAL"
	RATE_LIMITED            = "RATE_LIMITED"
	QUOTA_EXCEEDED          = "QUOTA_EXCEEDED"
	BAD_REQUEST             = "BAD_REQUEST"
	WORKER_UNREACHABLE      = "WORKER_UNREACHABLE"
	WORKER_BAD_RESPONSE     = "WORKER_BAD_RESPONSE"
	STRIPE_ERROR            = "STRIPE_ERROR"
	NOT_CONFIGURED          = "NOT_CONFIGURED"
	PLAN_NOT_FOUND          = "PLAN_NOT_FOUND"
	NO_STRIPE_CUSTOMER      = "NO_STRIPE_CUSTOMER"
	IDEMPOTENCY_CONFLICT    = "IDEMPOTENCY_CONFLICT"
	IDEMPOTENCY_KEY_REUSE   = "IDEMPOTENCY_KEY_REUSE"
	IDEMPOTENCY_KEY_INVALID = "IDEMPOTENCY_KEY_INVALID"
)

// Error is the inner object inside the {"error":{...}} response envelope.
type Error struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	Retryable bool   `json:"retryable"`
	RequestID string `json:"request_id"`
}

type envelope struct {
	Error Error `json:"error"`
}

// marshalJSON is the JSON serialisation function used by Write.
// Tests may replace it to exercise the defensive fallback path.
var marshalJSON = json.Marshal

// Write sets Content-Type: application/json and Cache-Control: no-store, writes
// status, and encodes the standard error envelope. requestID is passed as a plain
// string by each call site so this package needs no context or middleware import.
// Uses json.Marshal (not json.Encoder) so the body has no trailing newline.
// Marshal runs before WriteHeader so no status is committed on a marshal failure.
func Write(w http.ResponseWriter, requestID string, status int, code, message string, retryable bool) {
	b, err := marshalJSON(envelope{
		Error: Error{
			Code:      code,
			Message:   message,
			Retryable: retryable,
			RequestID: requestID,
		},
	})
	if err != nil {
		// Unreachable with current field types (all string/bool). Guard so a body is
		// always emitted if Error ever gains a failing json.Marshaler field.
		// Use an anonymous struct (not Error) to bypass such a field while preserving
		// the caller's values; requestID escaping is handled by json.Marshal, not
		// string concatenation.
		type fallback struct {
			Code      string `json:"code"`
			Message   string `json:"message"`
			Retryable bool   `json:"retryable"`
			RequestID string `json:"request_id"`
		}
		var ferr error
		b, ferr = marshalJSON(struct {
			Error fallback `json:"error"`
		}{Error: fallback{Code: code, Message: message, Retryable: retryable, RequestID: requestID}})
		if ferr != nil || b == nil {
			b = []byte(`{"error":{"code":"INTERNAL","message":"internal error","retryable":false,"request_id":""}}`)
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = w.Write(b) // client disconnects are unrecoverable; ignore write errors
}
