// API key generation and hashing — MUST match gateway/internal/auth/keys.go on the byte level.
// The gateway and dashboard read/write the same api_keys table; if their hash functions diverge,
// dashboard-issued keys will not authenticate.
// NOTE: Validation for API key names lives in dashboard/app/api/keys/route.ts
import { randomBytes, createHash } from "crypto";

// MUST match auth.PrefixLen in gateway/internal/auth/keys.go.
const PREFIX_LEN = 24;
const BASE32_ALPHABET = "ABCDEFGHIJKLMNOPQRSTUVWXYZ234567";

/**
 * Generate a new API key. Returns the full key (shown to the customer once)
 * and the displayable prefix (stored alongside the hash for O(1) lookup).
 */
export function generateKey(productPrefix: string): { full: string; prefix: string } {
  const raw = randomBytes(24); // 192 bits, matches Go's auth.Generate
  const suffix = encodeBase32(raw);
  const full = `${productPrefix}live_${suffix}`;
  const prefix = full.slice(0, PREFIX_LEN);
  return { full, prefix };
}

/**
 * SHA-256(salt || key). Must match gateway/internal/auth/keys.go:Hash() byte-for-byte.
 */
export function hashKey(salt: string, key: string): Buffer {
  const h = createHash("sha256");
  h.update(salt);
  h.update(key);
  return h.digest();
}

// Standard base32 (RFC 4648), no padding — same alphabet Go's encoding/base32 uses.
function encodeBase32(bytes: Buffer): string {
  let bits = 0;
  let value = 0;
  let output = "";
  for (const byte of bytes) {
    value = (value << 8) | byte;
    bits += 8;
    while (bits >= 5) {
      output += BASE32_ALPHABET[(value >>> (bits - 5)) & 31];
      bits -= 5;
    }
  }
  if (bits > 0) {
    output += BASE32_ALPHABET[(value << (5 - bits)) & 31];
  }
  return output;
}
