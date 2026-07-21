// Package channelsig implements the "t=<unix>,v1=<hex-hmac-sha256>" signature
// primitive shared by the gateway's two HMAC-signed outbound channels: the
// outbound webhook signature (X-Crucible-Signature, gateway/internal/webhookout)
// and the gateway→worker channel signature (X-Worker-Signature,
// gateway/internal/proxy). Both sign HMAC-SHA256(secret, timestamp + "." + body)
// and carry the result as "t=<timestamp>,v1=<hex>"; this package is that scheme's
// single implementation so neither channel hand-rolls its own copy.
package channelsig

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
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
