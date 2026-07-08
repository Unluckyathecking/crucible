// Crucible Enterprise Edition (EE) file.
// Licensed under the Crucible Enterprise License — see ee/LICENSE.md.
// Not covered by the repository's MIT license.
//
// TypeScript mirror of gateway/internal/license/license.go — the authoritative
// wire format. This is a byte-level mirror invariant (like dashboard/lib/keys.ts
// mirrors gateway/internal/auth/keys.go): a license key signed by the Go
// `licensegen` MUST verify identically here. Never diverge the wire format,
// edition defaults, or grace semantics from the Go side without changing both.
//
// SERVER-ONLY. This module imports Node's `crypto` and reads process.env; it must
// never be bundled into the Edge runtime or shipped to the client. Import it only
// from Node-runtime server code (auth.ts, server components, route handlers) —
// never from auth.config.ts, which the Edge middleware loads.
import { createPublicKey, verify as edVerify, type KeyObject } from "crypto";

const KEY_PREFIX = "cru1";
const GRACE_MS = 14 * 24 * 60 * 60 * 1000; // 14 days, mirrors license.GracePeriod.

// Feature name constants — must equal the Go FeatureX strings.
export const FEATURE_SSO = "sso";
export const FEATURE_OPERATOR_TOKENS = "operator_tokens";
export const FEATURE_AUDIT_EXPORT = "audit_export";
export type Feature = typeof FEATURE_SSO | typeof FEATURE_OPERATOR_TOKENS | typeof FEATURE_AUDIT_EXPORT;

// Editions — must equal the Go EditionX strings.
const EDITION_PRO = "pro";
const EDITION_BUSINESS = "business";
const EDITION_ENTERPRISE = "enterprise";

// Mirror of license.DefaultPublicKeyHex. MUST stay identical to the Go constant;
// verified against gateway/internal/license/license.go.
export const DEFAULT_PUBLIC_KEY_HEX =
  "318fe9828b97a7f84e6c340e035b27cbd4f027fae78d8674917a286c7f43e387";

export interface License {
  id: string;
  licensee: string;
  email: string;
  edition: string;
  features: string[];
  seats: number;
  issuedAt: Date;
  expiresAt: Date;
  inGrace: boolean;
}

// on-wire JSON payload shape. Mirrors the Go `payload` struct. `features` may be
// null (Go marshals an empty slice as `null`), absent, or an array; all three
// mean "use edition defaults".
interface Payload {
  id: string;
  licensee: string;
  email: string;
  edition: string;
  features: string[] | null;
  seats: number;
  issued_at: string;
  expires_at: string;
}

// SPKI DER prefix for an Ed25519 public key (RFC 8410). Node builds a KeyObject
// from a raw 32-byte key only via DER, so we splice the raw key onto this header.
const ED25519_SPKI_PREFIX = Buffer.from("302a300506032b6570032100", "hex");

function publicKeyFromRawHex(hex: string): KeyObject {
  const raw = Buffer.from(hex, "hex");
  if (raw.length !== 32) {
    throw new Error(`license public key must be 32 bytes (got ${raw.length})`);
  }
  return createPublicKey({
    key: Buffer.concat([ED25519_SPKI_PREFIX, raw]),
    format: "der",
    type: "spki",
  });
}

// resolveFeatures mirrors Go's resolveFeatures: declared features win; an empty
// (or null) list falls back to edition defaults; an unknown edition is rejected.
function resolveFeatures(edition: string, declared: string[] | null | undefined): string[] | null {
  if (declared && declared.length > 0) {
    return declared;
  }
  switch (edition) {
    case EDITION_PRO:
      return [FEATURE_OPERATOR_TOKENS, FEATURE_AUDIT_EXPORT];
    case EDITION_BUSINESS:
    case EDITION_ENTERPRISE:
      return [FEATURE_SSO, FEATURE_OPERATOR_TOKENS, FEATURE_AUDIT_EXPORT];
    default:
      return null; // unknown edition -> invalid
  }
}

// parse verifies a license key end to end: format, Ed25519 signature over the raw
// payload bytes, edition validity, and expiry within the grace window. Mirrors
// Go's license.Parse. Returns null for any invalid key (never throws). `at` is
// the clock, injectable so tests can exercise expiry/grace without sleeping.
export function parse(raw: string, pubKeyHex: string, at: Date = new Date()): License | null {
  const pub = publicKeyFromRawHex(pubKeyHex);

  const parts = raw.split(".");
  if (parts.length !== 3) return null;
  if (parts[0] !== KEY_PREFIX) return null;

  const payloadBytes = Buffer.from(parts[1], "base64url");
  const sig = Buffer.from(parts[2], "base64url");
  // Node's base64url decode is lenient; re-encode and compare to reject
  // malformed segments that would otherwise silently decode to garbage.
  if (payloadBytes.toString("base64url") !== parts[1] || sig.toString("base64url") !== parts[2]) {
    return null;
  }

  if (!edVerify(null, payloadBytes, pub, sig)) return null;

  let p: Payload;
  try {
    p = JSON.parse(payloadBytes.toString("utf8")) as Payload;
  } catch {
    return null;
  }

  const features = resolveFeatures(p.edition, p.features);
  if (features === null) return null;

  const issuedAt = new Date(p.issued_at);
  const expiresAt = new Date(p.expires_at);
  if (Number.isNaN(issuedAt.getTime()) || Number.isNaN(expiresAt.getTime())) return null;

  if (at.getTime() > expiresAt.getTime() + GRACE_MS) return null; // past grace

  return {
    id: p.id,
    licensee: p.licensee,
    email: p.email,
    edition: p.edition,
    features,
    seats: p.seats,
    issuedAt,
    expiresAt,
    inGrace: at.getTime() > expiresAt.getTime(),
  };
}

function resolvePublicKeyHex(): string {
  const override = (process.env.CRUCIBLE_LICENSE_PUBKEY ?? "").trim();
  return override !== "" ? override : DEFAULT_PUBLIC_KEY_HEX;
}

// loadLicense reads CRUCIBLE_LICENSE_KEY and returns the verified license, or
// null for community edition / any invalid key. It NEVER throws to callers:
// a misconfigured or tampered key must degrade to community mode, not crash the
// dashboard. Failures are logged server-side only.
export function loadLicense(at: Date = new Date()): License | null {
  const raw = (process.env.CRUCIBLE_LICENSE_KEY ?? "").trim();
  if (raw === "") return null; // community edition
  try {
    const lic = parse(raw, resolvePublicKeyHex(), at);
    if (lic === null) {
      console.warn("[license] CRUCIBLE_LICENSE_KEY present but invalid; running community edition");
    }
    return lic;
  } catch (err) {
    console.warn("[license] failed to evaluate CRUCIBLE_LICENSE_KEY; running community edition:", err);
    return null;
  }
}

// hasFeature reports whether the license grants feature. Null-safe: a null
// license (community edition) has no features. Mirrors Go's (*License).Has.
export function hasFeature(lic: License | null, feature: Feature): boolean {
  if (lic === null) return false;
  return lic.features.includes(feature);
}
