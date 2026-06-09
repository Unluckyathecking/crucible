// Package apierror provides the canonical JSON error-response envelope for the Crucible gateway.
// Handlers should emit errors through Write to ensure a consistent envelope shape.
package apierror

import (
	"encoding/json"
	"fmt"
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
// NOT safe for t.Parallel() — mutates package-level state.
var marshalJSON = json.Marshal

// Write unconditionally sets Content-Type: application/json and Cache-Control: no-store
// (overriding any pre-existing values for those two headers), writes status, and
// encodes the standard error envelope. All other response headers already set by the
// caller are not modified. requestID is passed as a plain string by each call site so
// this package needs no context or middleware import.
// Uses json.Marshal (not json.Encoder) so the body has no trailing newline.
// Marshal and fallback both run before WriteHeader so a body is always available
// when the status is committed; callers never see a headers-only response.
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
		if ferr != nil {
			// Both marshalJSON calls failed (only reachable via test injection).
			// fmt.Sprintf with %q uses Go string quoting rules, which are a superset
			// of JSON string escaping for the ASCII subset used by our error codes and
			// messages. This path cannot itself fail, so b is always a valid-JSON body.
			b = []byte(fmt.Sprintf(
				`{"error":{"code":%q,"message":%q,"retryable":%t,"request_id":%q}}`,
				code, message, retryable, requestID,
			))
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = w.Write(b) // client disconnects are unrecoverable; ignore write errors
}
