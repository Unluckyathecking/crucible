// webhook.go provides VerifyWebhook, a signature verifier for Crucible gateway webhook
// deliveries. It complements the generated client in client.go (same package, crucible).
package crucible

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"
	"time"
)

// maxSigCandidates caps how many v1= values we parse and compare. Mirrors the
// gateway verifySignature defense: an attacker cannot force unbounded HMAC
// comparisons by stuffing the header with many candidates.
const maxSigCandidates = 8

// maxHeaderParts caps the total comma-separated segments to prevent unbounded
// iteration over attacker-controlled input before the v1 candidate cap applies.
const maxHeaderParts = 16

// DefaultTolerance is the maximum webhook age accepted by VerifyWebhook.
// Equals the gateway's 5-minute inbound replay window.
const DefaultTolerance = 5 * time.Minute

// SignatureHeader is the HTTP header that carries the t= timestamp and v1= HMAC digest.
const SignatureHeader = "X-Crucible-Signature"

// TimestampHeader is the HTTP header that carries the delivery Unix timestamp.
// It is provided for logging and tracing; do NOT pass it to VerifyWebhook —
// the timestamp used for replay protection is extracted from SignatureHeader.
const TimestampHeader = "X-Crucible-Timestamp"

// WebhookEventIDHeader is the HTTP header carrying the delivery UUID.
// Use it to deduplicate at-least-once deliveries before processing an event.
const WebhookEventIDHeader = "X-Webhook-Event-ID"

// WebhookEventTypeHeader is the HTTP header carrying the event type string (e.g. "invoice.paid").
const WebhookEventTypeHeader = "X-Webhook-Event-Type"

// WebhookError is returned when X-Crucible-Signature verification fails.
type WebhookError struct {
	msg string
}

func (e *WebhookError) Error() string   { return "crucible webhook: " + e.msg }
func (e *WebhookError) Message() string { return e.msg }

// VerifyWebhook verifies the X-Crucible-Signature on a webhook delivery from the gateway.
// secretHex is the hex-encoded signing secret shown on the dashboard endpoint page.
// sigHeader is the raw value of the X-Crucible-Signature header (format: t=<ts>,v1=<hex>).
// body is the unmodified request body bytes.
// tolerance is the maximum accepted age; pass 0 to use DefaultTolerance (Go-specific
// sentinel — the zero Duration value; in the TypeScript SDK, omit/undefined serves this
// role, and explicit 0 means zero-width tolerance instead).
// All errors are *WebhookError. A non-nil error means the payload must not be trusted.
func VerifyWebhook(secretHex, sigHeader string, body []byte, tolerance time.Duration) error {
	// Capture clock as the very first action — before all validation and attacker-controlled
	// header parsing — so the sampled instant is not shifted by processing time.
	now := time.Now()

	if tolerance == 0 {
		tolerance = DefaultTolerance
	} else if tolerance < 0 {
		return &WebhookError{"negative tolerance not allowed"}
	}
	if len(secretHex) == 0 || len(secretHex)%2 != 0 {
		return &WebhookError{"invalid secretHex: must be non-empty even-length hex string"}
	}
	secret, hexErr := hex.DecodeString(secretHex)
	if hexErr != nil {
		return &WebhookError{"invalid secretHex: contains non-hex characters"}
	}

	timestamp, sigs, parseErr := parseSignatureHeader(sigHeader)
	if parseErr != nil {
		return parseErr
	}

	// Bound length to match the TypeScript /^\d{1,15}$/ guard: 15 digits covers
	// all real Unix timestamps and prevents ParseInt from processing monster strings.
	if len(timestamp) > 15 {
		return &WebhookError{"bad timestamp in signature header"}
	}
	// Reject leading zeros: multi-digit timestamps starting with '0' (e.g. "01234")
	// diverge from the gateway's output and TypeScript's ts.toString() round-trip.
	// Single-digit "0" (Unix epoch) is allowed because len==1 fails the len>1 condition.
	if len(timestamp) > 1 && timestamp[0] == '0' {
		return &WebhookError{"bad timestamp in signature header"}
	}
	// Reject non-digit leading character: ParseInt("+123") returns 123 without error,
	// but TypeScript's /^\d{1,15}$/ rejects '+'. Enforce digit-only for cross-language parity.
	// Defense-in-depth: parseSignatureHeader currently rejects empty timestamps, but this
	// len==0 guard prevents a panic if the parser contract ever relaxes in the future.
	if len(timestamp) == 0 || timestamp[0] < '0' || timestamp[0] > '9' {
		return &WebhookError{"bad timestamp in signature header"}
	}
	ts, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil {
		return &WebhookError{"bad timestamp in signature header"}
	}
	tsTime := time.Unix(ts, 0)
	// Explicit future-timestamp rejection before computing age, so age is
	// always non-negative and the direction of the comparison is unambiguous.
	if now.Before(tsTime) {
		return &WebhookError{"webhook timestamp in the future"}
	}
	age := now.Sub(tsTime) // always >= 0 because now >= tsTime
	if age > tolerance {
		return &WebhookError{"webhook timestamp too old (replay protection)"}
	}

	mac := hmac.New(sha256.New, secret)
	// hash.Hash.Write never returns an error; blank identifiers are explicit acknowledgement.
	_, _ = mac.Write([]byte(timestamp))
	_, _ = mac.Write([]byte("."))
	_, _ = mac.Write(body)
	expected := mac.Sum(nil)

	for _, sigHex := range sigs {
		// Length guard mirrors the TypeScript sig.length !== 64 check: reject
		// wrong-length inputs on the fast path before calling the decoder.
		if len(sigHex) != sha256.Size*2 {
			continue
		}
		candidate, hexErr := hex.DecodeString(sigHex)
		if hexErr != nil || len(candidate) != sha256.Size {
			continue
		}
		if hmac.Equal(candidate, expected) {
			return nil
		}
	}
	return &WebhookError{"no matching v1 signature"}
}

func parseSignatureHeader(header string) (string, []string, *WebhookError) {
	var (
		timestamp string
		sigs      []string
	)
	if header == "" {
		return "", nil, &WebhookError{"missing X-Crucible-Signature header"}
	}
	// SplitN limit is maxHeaderParts+1 so that a header with exactly maxHeaderParts+1
	// (or more) comma-separated parts produces a slice of length maxHeaderParts+1, which
	// the subsequent len check catches. Using maxHeaderParts alone would silently merge
	// the tail into the last element without triggering the rejection. The +1 pattern
	// bounds allocation while making overflow detectable.
	parts := strings.SplitN(header, ",", maxHeaderParts+1)
	if len(parts) > maxHeaderParts {
		return "", nil, &WebhookError{"malformed X-Crucible-Signature header"}
	}
	for _, part := range parts {
		kv := strings.SplitN(part, "=", 2)
		// Embedded "=" in a value (e.g. "t=1=2") is structurally invalid — none of
		// the current key types (t, v1) allow it, and accepting it creates parser
		// ambiguity for future protocol extensions.
		if len(kv) != 2 || strings.Contains(kv[1], "=") {
			return "", nil, &WebhookError{"malformed X-Crucible-Signature header"}
		}
		// Universal empty-value guard — runs before the switch, so it applies to
		// ALL keys: t=, v1=, AND any unknown future key (e.g. v2=). A part like
		// "foo=" is rejected here; "foo=bar" falls through to the default case and
		// is silently ignored. Mirrors TypeScript's universal val==="" guard.
		if len(kv[0]) == 0 || len(kv[1]) == 0 {
			return "", nil, &WebhookError{"malformed X-Crucible-Signature header"}
		}
		switch kv[0] {
		case "t":
			// Exactly one timestamp per delivery: duplicate t= is invalid.
			if timestamp != "" {
				return "", nil, &WebhookError{"malformed X-Crucible-Signature header"}
			}
			timestamp = kv[1]
		case "v1":
			// Multiple v1= values are accepted intentionally: during secret rotation the
			// gateway may include two signatures (old key + new key) so receivers can
			// verify with whichever key they currently hold. maxSigCandidates bounds the
			// number of HMAC comparisons to prevent header-stuffing DoS. This is the
			// intentional asymmetry with t=, which is always singular.
			//
			// Positive-guard pattern (mirrors TypeScript): append only when under the cap,
			// then fall through so subsequent parts are still validated. We do NOT break
			// here because remaining parts may include a duplicate t= (malformed) or future
			// protocol fields — skipping them silently would hide header integrity issues.
			if len(sigs) < maxSigCandidates {
				sigs = append(sigs, kv[1])
			}
		default:
			// Unknown keys with non-empty values (e.g. future v2=…) are silently ignored
			// for forward compatibility. Empty values are caught by the universal guard above.
		}
	}
	if timestamp == "" || len(sigs) == 0 {
		return "", nil, &WebhookError{"malformed X-Crucible-Signature header"}
	}
	return timestamp, sigs, nil
}
