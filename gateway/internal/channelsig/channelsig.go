// Package channelsig implements the "t=<unix>,v1=<hex-hmac-sha256>" signed-channel
// primitive shared by every HMAC-authenticated channel in the gateway: the outbound
// webhook signature (X-Crucible-Signature, gateway/internal/webhookout) and the
// gateway→worker channel signature (X-Worker-Signature, gateway/internal/proxy).
// Both channels sign HMAC-SHA256(secret, timestamp + "." + body) and carry the result
// as "t=<timestamp>,v1=<hex>"; this package is that scheme's single implementation so
// every future signed channel — in this clone or the next — reuses it instead of
// hand-rolling a sixth copy.
package channelsig

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strconv"
	"strings"
	"time"
)

// Sign computes the lowercase hex HMAC-SHA256 digest over "timestamp.body" using secret.
// timestamp is typically strconv.FormatInt(time.Now().UTC().Unix(), 10).
func Sign(secret []byte, timestamp string, body []byte) string {
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write([]byte(timestamp))
	_, _ = mac.Write([]byte("."))
	_, _ = mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

// Header formats a signature value in the wire format both signed channels use:
// "t=<timestamp>,v1=<hex-digest>".
func Header(timestamp string, digestHex string) string {
	return "t=" + timestamp + ",v1=" + digestHex
}

// Verification failures. Callers should surface only a stable code (e.g. UNAUTHORIZED)
// to the request's caller — these error strings are for gateway-side logs only.
var (
	ErrMissingHeader     = errors.New("channelsig: missing signature header")
	ErrMalformedHeader   = errors.New("channelsig: malformed signature header")
	ErrInvalidTimestamp  = errors.New("channelsig: invalid timestamp in signature header")
	ErrStaleTimestamp    = errors.New("channelsig: timestamp outside allowed window")
	ErrInvalidSignature  = errors.New("channelsig: invalid signature value")
	ErrSignatureMismatch = errors.New("channelsig: signature mismatch")
)

// Verify parses header in "t=<unix>,v1=<hex>" format and checks it against body signed
// with secret. now is the reference time and window is the maximum allowed clock skew
// in either direction (a timestamp more than window in the past or the future is
// rejected). The signature comparison is constant-time (hmac.Equal).
func Verify(secret []byte, header string, body []byte, now time.Time, window time.Duration) error {
	if header == "" {
		return ErrMissingHeader
	}

	var tsStr, sigHex string
	for _, part := range strings.Split(header, ",") {
		switch {
		case strings.HasPrefix(part, "t="):
			tsStr = part[2:]
		case strings.HasPrefix(part, "v1="):
			sigHex = part[3:]
		}
	}
	if tsStr == "" || sigHex == "" {
		return ErrMalformedHeader
	}

	ts, err := strconv.ParseInt(tsStr, 10, 64)
	if err != nil {
		return ErrInvalidTimestamp
	}

	diff := now.Unix() - ts
	if diff < 0 {
		diff = -diff
	}
	if diff > int64(window.Seconds()) {
		return ErrStaleTimestamp
	}

	provided, err := hex.DecodeString(sigHex)
	if err != nil || len(provided) != sha256.Size {
		return ErrInvalidSignature
	}

	expected, err := hex.DecodeString(Sign(secret, tsStr, body))
	if err != nil {
		// Sign always returns a valid lowercase hex string; unreachable in practice.
		return ErrInvalidSignature
	}

	if !hmac.Equal(provided, expected) {
		return ErrSignatureMismatch
	}
	return nil
}
