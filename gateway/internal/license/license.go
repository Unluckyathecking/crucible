// SPDX-License-Identifier: MIT
//
// Package license implements Crucible's offline, Ed25519-verified deployment
// license keys. This is open-core edition gating — it answers "did the operator
// buy Crucible Pro/Business/Enterprise" — and is deliberately unrelated to the
// billing `plans` table, which tiers the END customers of a cloned product.
// Nothing here touches Postgres, Redis, or Stripe: a license key verifies
// entirely offline against a compiled-in public key.
//
// Key format: cru1.<base64url-nopad(payload JSON)>.<base64url-nopad(signature)>
// The signature is over the RAW decoded payload bytes — no re-marshalling or
// canonicalization — so verification never depends on JSON key ordering.
package license

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

const (
	keyPrefix   = "cru1"
	GracePeriod = 14 * 24 * time.Hour
)

// Feature name constants. Wave-2 modules gate on these exact strings.
const (
	FeatureSSO            = "sso"
	FeatureOperatorTokens = "operator_tokens"
	FeatureAuditExport    = "audit_export"
)

// Editions.
const (
	EditionPro        = "pro"
	EditionBusiness   = "business"
	EditionEnterprise = "enterprise"
)

// DefaultPublicKeyHex is the compiled-in Ed25519 public key (hex, 32 bytes)
// that verifies license keys when CRUCIBLE_LICENSE_PUBKEY is unset. The private
// half was generated once with `licensegen keygen` and DISCARDED — it is not in
// this repo. The repo owner MUST run `licensegen keygen`, replace this constant
// with their own public key (or set CRUCIBLE_LICENSE_PUBKEY at deploy time), and
// keep the matching private key offline before selling any license. See
// cmd/licensegen/README.md.
const DefaultPublicKeyHex = "318fe9828b97a7f84e6c340e035b27cbd4f027fae78d8674917a286c7f43e387"

// now is the clock, overridable in tests so expiry paths never sleep.
var now = time.Now

// ErrInvalid is returned (wrapped) for any key that fails format, signature, or
// validity checks. Callers that want community fallback can treat every non-nil
// Parse error identically; ErrInvalid lets them errors.Is-match if they care.
var ErrInvalid = errors.New("invalid license")

// License is a verified, in-grace-or-live deployment license.
type License struct {
	ID, Licensee, Email, Edition string
	Features                     []string
	Seats                        int
	IssuedAt, ExpiresAt          time.Time
}

// payload is the on-wire JSON shape. Times are RFC3339 strings so the signed
// bytes are stable and human-readable; License exposes them as time.Time.
type payload struct {
	ID        string   `json:"id"`
	Licensee  string   `json:"licensee"`
	Email     string   `json:"email"`
	Edition   string   `json:"edition"`
	Features  []string `json:"features"`
	Seats     int      `json:"seats"`
	IssuedAt  string   `json:"issued_at"`
	ExpiresAt string   `json:"expires_at"`
}

var b64 = base64.RawURLEncoding

// Parse verifies a license key end to end: format, Ed25519 signature over the
// raw payload bytes, edition validity, and expiry within the grace window. A
// key that has expired but is still inside GracePeriod parses successfully;
// callers observe the state via (*License).InGrace. Past grace is ErrInvalid.
func Parse(raw string, pub ed25519.PublicKey) (*License, error) {
	if len(pub) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("%w: public key must be %d bytes (got %d)", ErrInvalid, ed25519.PublicKeySize, len(pub))
	}
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return nil, fmt.Errorf("%w: expected 3 dot-separated segments, got %d", ErrInvalid, len(parts))
	}
	if parts[0] != keyPrefix {
		return nil, fmt.Errorf("%w: bad prefix %q", ErrInvalid, parts[0])
	}
	payloadBytes, err := b64.DecodeString(parts[1])
	if err != nil {
		return nil, fmt.Errorf("%w: payload not base64url: %v", ErrInvalid, err)
	}
	sig, err := b64.DecodeString(parts[2])
	if err != nil {
		return nil, fmt.Errorf("%w: signature not base64url: %v", ErrInvalid, err)
	}
	if !ed25519.Verify(pub, payloadBytes, sig) {
		return nil, fmt.Errorf("%w: signature verification failed", ErrInvalid)
	}

	var p payload
	if err := json.Unmarshal(payloadBytes, &p); err != nil {
		return nil, fmt.Errorf("%w: payload not valid JSON: %v", ErrInvalid, err)
	}
	features, err := resolveFeatures(p.Edition, p.Features)
	if err != nil {
		return nil, err
	}
	issued, err := time.Parse(time.RFC3339, p.IssuedAt)
	if err != nil {
		return nil, fmt.Errorf("%w: issued_at not RFC3339: %v", ErrInvalid, err)
	}
	expires, err := time.Parse(time.RFC3339, p.ExpiresAt)
	if err != nil {
		return nil, fmt.Errorf("%w: expires_at not RFC3339: %v", ErrInvalid, err)
	}
	if now().After(expires.Add(GracePeriod)) {
		return nil, fmt.Errorf("%w: expired past grace period (expired %s)", ErrInvalid, expires.Format(time.RFC3339))
	}

	return &License{
		ID:        p.ID,
		Licensee:  p.Licensee,
		Email:     p.Email,
		Edition:   p.Edition,
		Features:  features,
		Seats:     p.Seats,
		IssuedAt:  issued,
		ExpiresAt: expires,
	}, nil
}

// resolveFeatures returns the payload's features, or the edition defaults when
// the payload leaves features empty. An unknown edition is rejected.
func resolveFeatures(edition string, declared []string) ([]string, error) {
	if len(declared) > 0 {
		return declared, nil
	}
	switch edition {
	case EditionPro:
		return []string{FeatureOperatorTokens, FeatureAuditExport}, nil
	case EditionBusiness, EditionEnterprise:
		return []string{FeatureSSO, FeatureOperatorTokens, FeatureAuditExport}, nil
	default:
		return nil, fmt.Errorf("%w: unknown edition %q", ErrInvalid, edition)
	}
}

// Has reports whether the license grants feature. A nil receiver means the
// gateway is running community edition and never has any feature.
func (l *License) Has(feature string) bool {
	if l == nil {
		return false
	}
	for _, f := range l.Features {
		if f == feature {
			return true
		}
	}
	return false
}

// InGrace reports whether the license is past its expiry but still inside the
// grace window (during which it keeps working). A nil receiver (community
// edition) is never in grace.
func (l *License) InGrace() bool {
	if l == nil {
		return false
	}
	return now().After(l.ExpiresAt)
}

// SignInput describes a license to mint. Used by the licensegen CLI. Feature
// defaults are NOT applied here: an empty Features is signed as empty and
// resolved from Edition at Parse time, so the signed bytes stay minimal.
type SignInput struct {
	ID, Licensee, Email, Edition string
	Features                     []string
	Seats                        int
	IssuedAt, ExpiresAt          time.Time
}

// Sign mints a signed license key. It lives here so the signer and Parse share
// one authoritative wire-format definition and cannot drift apart.
func Sign(in SignInput, priv ed25519.PrivateKey) (string, error) {
	if len(priv) != ed25519.PrivateKeySize {
		return "", fmt.Errorf("private key must be %d bytes (got %d)", ed25519.PrivateKeySize, len(priv))
	}
	switch in.Edition {
	case EditionPro, EditionBusiness, EditionEnterprise:
	default:
		return "", fmt.Errorf("unknown edition %q", in.Edition)
	}
	p := payload{
		ID:        in.ID,
		Licensee:  in.Licensee,
		Email:     in.Email,
		Edition:   in.Edition,
		Features:  in.Features,
		Seats:     in.Seats,
		IssuedAt:  in.IssuedAt.UTC().Format(time.RFC3339),
		ExpiresAt: in.ExpiresAt.UTC().Format(time.RFC3339),
	}
	b, err := json.Marshal(p)
	if err != nil {
		return "", fmt.Errorf("marshal payload: %w", err)
	}
	sig := ed25519.Sign(priv, b)
	return keyPrefix + "." + b64.EncodeToString(b) + "." + b64.EncodeToString(sig), nil
}

// ResolvePublicKey decodes the operator's Ed25519 public key. A non-empty
// override (from CRUCIBLE_LICENSE_PUBKEY via config) wins; otherwise the
// compiled-in DefaultPublicKeyHex is used.
func ResolvePublicKey(overrideHex string) (ed25519.PublicKey, error) {
	h := strings.TrimSpace(overrideHex)
	if h == "" {
		h = DefaultPublicKeyHex
	}
	raw, err := hex.DecodeString(h)
	if err != nil {
		return nil, fmt.Errorf("decode license public key hex: %w", err)
	}
	if len(raw) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("license public key must be %d bytes (got %d)", ed25519.PublicKeySize, len(raw))
	}
	return ed25519.PublicKey(raw), nil
}
