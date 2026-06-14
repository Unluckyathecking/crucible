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
// tolerance is the maximum accepted age; pass 0 to use DefaultTolerance.
// All errors are *WebhookError. A non-nil error means the payload must not be trusted.
func VerifyWebhook(secretHex, sigHeader string, body []byte, tolerance time.Duration) error {
	if tolerance == 0 {
		tolerance = DefaultTolerance
	}
	if tolerance < 0 {
		return &WebhookError{"negative tolerance not allowed"}
	}
	if len(secretHex) == 0 || len(secretHex)%2 != 0 {
		return &WebhookError{"invalid secretHex: must be non-empty even-length hex string"}
	}
	secret, hexErr := hex.DecodeString(secretHex)
	if hexErr != nil {
		return &WebhookError{"invalid secretHex: contains non-hex characters"}
	}

	// Capture clock before any attacker-controlled parsing so the sampled instant
	// is not shifted by header-parsing time.
	now := time.Now()

	timestamp, sigs, parseErr := parseSignatureHeader(sigHeader)
	if parseErr != nil {
		return parseErr
	}

	// Bound length to match the TypeScript /^\d{1,15}$/ guard: 15 digits covers
	// all real Unix timestamps and prevents ParseInt from processing monster strings.
	if len(timestamp) > 15 {
		return &WebhookError{"bad timestamp in signature header"}
	}
	ts, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil {
		return &WebhookError{"bad timestamp in signature header"}
	}
	age := now.Sub(time.Unix(ts, 0))
	if age < 0 {
		return &WebhookError{"webhook timestamp in the future"}
	}
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

func parseSignatureHeader(header string) (string, []string, error) {
	var (
		timestamp string
		sigs      []string
	)
	if header == "" {
		return "", nil, &WebhookError{"missing X-Crucible-Signature header"}
	}
	parts := strings.Split(header, ",")
	if len(parts) > maxHeaderParts {
		return "", nil, &WebhookError{"malformed X-Crucible-Signature header"}
	}
	for _, part := range parts {
		kv := strings.SplitN(part, "=", 2)
		if len(kv) != 2 {
			return "", nil, &WebhookError{"malformed X-Crucible-Signature header"}
		}
		switch kv[0] {
		case "t":
			timestamp = kv[1]
		case "v1":
			if len(sigs) >= maxSigCandidates {
				continue
			}
			sigs = append(sigs, kv[1])
		// Unknown keys (e.g. future v2=) are silently ignored for forward compatibility.
		}
	}
	if timestamp == "" || len(sigs) == 0 {
		return "", nil, &WebhookError{"malformed X-Crucible-Signature header"}
	}
	return timestamp, sigs, nil
}
