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

	// UNKNOWN is used as a Prometheus metric label fallback when a worker error
	// response omits the error code. Lowercase to preserve existing dashboard queries.
	// Never emitted in customer-facing responses.
	UNKNOWN = "unknown"
)

// Error is the inner object inside the {"error":{...}} response envelope.
type Error struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	Retryable bool   `json:"retryable"`
	RequestID string `json:"request_id"` // always emitted (empty string is valid per schema)
}

// envelope is the top-level JSON wrapper: {"error":{...}}.
type envelope struct {
	Error Error `json:"error"`
}

// marshalJSON is the JSON serialisation function used by Write.
// Tests may replace it to exercise the defensive fallback path.
// NOT safe for t.Parallel() — mutates package-level state.
// Unexported: only code within this package (including apierror_internal_test.go) can mutate it.
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
		// Use real json.Marshal to bypass any injected marshalJSON (test-only path).
		var ferr error
		b, ferr = json.Marshal(envelope{
			Error: Error{
				Code:      code,
				Message:   message,
				Retryable: retryable,
				RequestID: requestID,
			},
		})
		if ferr != nil {
			// Absolute last resort: preserve caller's values via fmt.Sprintf so b is
			// never nil after WriteHeader. %q produces valid JSON string escaping for
			// all practical inputs (error codes/messages are controlled ASCII). Only
			// reachable if both marshalJSON AND real json.Marshal fail — impossible in
			// production for string/bool fields.
			b = []byte(fmt.Sprintf(`{"error":{"code":%q,"message":%q,"retryable":%t,"request_id":%q}}`,
				code, message, retryable, requestID))
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	if _, werr := w.Write(b); werr != nil {
		return // client disconnected; net/http handles connection cleanup
	}
}
