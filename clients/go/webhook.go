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

// DefaultTolerance is the maximum webhook age accepted by VerifyWebhook.
// Equals the gateway's 5-minute inbound replay window.
const DefaultTolerance = 5 * time.Minute

// SignatureHeader is the HTTP header that carries the t= timestamp and v1= HMAC digest.
const SignatureHeader = "X-Crucible-Signature"

// TimestampHeader is the HTTP header that carries the delivery Unix timestamp.
const TimestampHeader = "X-Crucible-Timestamp"

// WebhookError is returned when X-Crucible-Signature verification fails.
type WebhookError struct {
	msg string
}

func (e *WebhookError) Error() string { return "crucible webhook: " + e.msg }

// VerifyWebhook verifies the X-Crucible-Signature on a webhook delivery from the gateway.
// secretHex is the hex-encoded signing secret shown on the dashboard endpoint page.
// sigHeader is the raw value of the X-Crucible-Signature header (format: t=<ts>,v1=<hex>).
// body is the unmodified request body bytes.
// tolerance is the maximum accepted age; pass 0 to use DefaultTolerance.
// All errors are *WebhookError. A non-nil error means the payload must not be trusted.
func VerifyWebhook(secretHex, sigHeader string, body []byte, tolerance time.Duration) error {
	if tolerance <= 0 {
		tolerance = DefaultTolerance
	}
	secret, hexErr := hex.DecodeString(secretHex)
	if hexErr != nil {
		return &WebhookError{"invalid secretHex: " + hexErr.Error()}
	}

	timestamp, sigs, parseErr := parseSignatureHeader(sigHeader)
	if parseErr != nil {
		return parseErr
	}

	ts, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil {
		return &WebhookError{"bad timestamp in signature header"}
	}
	age := time.Since(time.Unix(ts, 0))
	if age < 0 {
		age = -age
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

	for _, sig := range sigs {
		candidate, hexErr := hex.DecodeString(sig)
		if hexErr != nil || len(candidate) != sha256.Size {
			continue
		}
		if hmac.Equal(candidate, expected) {
			return nil
		}
	}
	return &WebhookError{"no matching v1 signature"}
}

func parseSignatureHeader(header string) (timestamp string, sigs []string, err error) {
	if header == "" {
		return "", nil, &WebhookError{"missing X-Crucible-Signature header"}
	}
	for _, part := range strings.Split(header, ",") {
		kv := strings.SplitN(part, "=", 2)
		if len(kv) != 2 {
			continue
		}
		switch kv[0] {
		case "t":
			timestamp = kv[1]
		case "v1":
			if len(sigs) < maxSigCandidates {
				sigs = append(sigs, kv[1])
			}
		}
	}
	if timestamp == "" || len(sigs) == 0 {
		return "", nil, &WebhookError{"malformed X-Crucible-Signature header"}
	}
	return timestamp, sigs, nil
}
