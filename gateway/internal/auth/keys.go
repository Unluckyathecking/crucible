// Package auth issues, hashes, looks up, and gates HTTP requests by API key.
package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base32"
	"fmt"
)

// PrefixLen is the visible portion of an issued key (e.g. "cru_live_A3F9NK4M7QHGBVTP").
// Customers see this in the dashboard; it's indexed in Postgres for O(1) lookup before
// the constant-time hash compare gates auth.
//
// With the literal "cru_live_" prefix (9 chars) eating the first slots, PrefixLen=24 leaves
// 15 base32 chars of entropy = 32^15 ≈ 3.5e22 combinations. The active-prefix index is
// UNIQUE, so we'd collide at birthday-paradox ~1e11 keys — billions of years of issuance.
// PrefixLen used to be 12 (only 3 random chars; collisions likely at hundreds of keys).
const PrefixLen = 24

// Generate returns (full_key, displayable_prefix). The full key is shown to the customer
// ONCE at creation; only the prefix and Hash(salt, full_key) are persisted.
//
// keyPrefix is the product-customizable identifier (e.g. "cru_" — set per product clone via API_KEY_PREFIX).
func Generate(keyPrefix string) (full, prefix string, err error) {
	raw := make([]byte, 24) // 192 bits of entropy
	if _, err := rand.Read(raw); err != nil {
		return "", "", fmt.Errorf("read random: %w", err)
	}
	suffix := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(raw)
	full = keyPrefix + "live_" + suffix

	prefix = full
	if len(prefix) > PrefixLen {
		prefix = prefix[:PrefixLen]
	}
	return full, prefix, nil
}

// Hash returns SHA-256(salt || key). Stored as bytea in api_keys.hash.
func Hash(salt, key string) []byte {
	h := sha256.New()
	h.Write([]byte(salt))
	h.Write([]byte(key))
	return h.Sum(nil)
}

// VerifyHash compares two hashes in constant time.
func VerifyHash(a, b []byte) bool {
	return subtle.ConstantTimeCompare(a, b) == 1
}
