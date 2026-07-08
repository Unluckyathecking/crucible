// Crucible Enterprise Edition (EE) file.
// Licensed under the Crucible Enterprise License — see ee/LICENSE.md.
// Not covered by the repository's MIT license.
//
// Cross-implementation check for the Go<->TS license mirror. The fixtures below
// were minted by the Go `licensegen` CLI (gateway/cmd/licensegen) with a throwaway
// keypair; GO_FIXTURE_PUBKEY_HEX is that keypair's public half. If these verify,
// the TS wire-format decode + Ed25519 verification matches Go byte-for-byte.
//
// To regenerate:
//   cd gateway
//   go run ./cmd/licensegen keygen                    # note priv + pub
//   go run ./cmd/licensegen sign --priv <priv> --licensee "Acme Corp" \
//     --email ops@acme.example --edition enterprise --seats 25 \
//     --expires 2027-01-01 --id lic_xfixture
import { describe, it, expect } from "vitest";
import {
  parse,
  loadLicense,
  hasFeature,
  DEFAULT_PUBLIC_KEY_HEX,
  FEATURE_SSO,
  FEATURE_OPERATOR_TOKENS,
  FEATURE_AUDIT_EXPORT,
} from "@/lib/license";

// Public half of the throwaway keypair used to sign the fixtures below.
const GO_FIXTURE_PUBKEY_HEX = "bc31af831ee52828a43d6af6bffe418deb3c66a4ca4fff174598070c81dee211";

// enterprise, seats 25, features omitted (edition defaults), expires 2027-01-01.
const GO_ENTERPRISE_KEY =
  "cru1.eyJpZCI6ImxpY194Zml4dHVyZSIsImxpY2Vuc2VlIjoiQWNtZSBDb3JwIiwiZW1haWwiOiJvcHNAYWNtZS5leGFtcGxlIiwiZWRpdGlvbiI6ImVudGVycHJpc2UiLCJmZWF0dXJlcyI6bnVsbCwic2VhdHMiOjI1LCJpc3N1ZWRfYXQiOiIyMDI2LTA3LTA4VDIwOjIyOjU0WiIsImV4cGlyZXNfYXQiOiIyMDI3LTAxLTAxVDAwOjAwOjAwWiJ9.cAVhvb3q_XDs5Q5GYMqwPPtfaubkV56kR1VVzqy5FVgELMRFdNQotzaynGvCm41sfVTiItUGtS2uOVwmRcMKBg";

// pro, seats 3, features omitted (edition defaults), expires 2027-01-01.
const GO_PRO_KEY =
  "cru1.eyJpZCI6ImxpY19wcm8iLCJsaWNlbnNlZSI6IkFjbWUgQ29ycCIsImVtYWlsIjoib3BzQGFjbWUuZXhhbXBsZSIsImVkaXRpb24iOiJwcm8iLCJmZWF0dXJlcyI6bnVsbCwic2VhdHMiOjMsImlzc3VlZF9hdCI6IjIwMjYtMDctMDhUMjA6MjI6NTRaIiwiZXhwaXJlc19hdCI6IjIwMjctMDEtMDFUMDA6MDA6MDBaIn0.tUm_8MhLm-GOfKb_wZmRQXKIg9Mq4IQEaBeKBRUm9AuNAMsIzpatVnGxDQNBoNjcfHiKIaWgi7LPZBwd0O8pDg";

// A time comfortably inside the license validity window.
const LIVE = new Date("2026-08-01T00:00:00Z");
const EXPIRES = new Date("2027-01-01T00:00:00Z");

describe("cross-implementation: Go-signed keys verify in TS", () => {
  it("parses a Go-signed enterprise key and resolves edition-default features", () => {
    const lic = parse(GO_ENTERPRISE_KEY, GO_FIXTURE_PUBKEY_HEX, LIVE);
    expect(lic).not.toBeNull();
    expect(lic!.edition).toBe("enterprise");
    expect(lic!.licensee).toBe("Acme Corp");
    expect(lic!.email).toBe("ops@acme.example");
    expect(lic!.seats).toBe(25);
    // features:null on the wire -> edition defaults.
    expect(lic!.features.sort()).toEqual(
      [FEATURE_SSO, FEATURE_OPERATOR_TOKENS, FEATURE_AUDIT_EXPORT].sort(),
    );
    expect(lic!.inGrace).toBe(false);
    expect(hasFeature(lic, FEATURE_SSO)).toBe(true);
  });

  it("pro edition defaults omit SSO", () => {
    const lic = parse(GO_PRO_KEY, GO_FIXTURE_PUBKEY_HEX, LIVE);
    expect(lic).not.toBeNull();
    expect(lic!.edition).toBe("pro");
    expect(lic!.features.sort()).toEqual([FEATURE_OPERATOR_TOKENS, FEATURE_AUDIT_EXPORT].sort());
    expect(hasFeature(lic, FEATURE_SSO)).toBe(false);
    expect(hasFeature(lic, FEATURE_AUDIT_EXPORT)).toBe(true);
  });
});

describe("signature and format rejection", () => {
  it("rejects a key verified against the wrong public key", () => {
    expect(parse(GO_ENTERPRISE_KEY, DEFAULT_PUBLIC_KEY_HEX, LIVE)).toBeNull();
  });

  it("rejects a tampered payload (signature no longer matches)", () => {
    const parts = GO_ENTERPRISE_KEY.split(".");
    const tampered = JSON.parse(Buffer.from(parts[1], "base64url").toString("utf8"));
    tampered.seats = 9999;
    parts[1] = Buffer.from(JSON.stringify(tampered)).toString("base64url");
    expect(parse(parts.join("."), GO_FIXTURE_PUBKEY_HEX, LIVE)).toBeNull();
  });

  it("rejects a bad prefix", () => {
    const bad = "cru9." + GO_ENTERPRISE_KEY.split(".").slice(1).join(".");
    expect(parse(bad, GO_FIXTURE_PUBKEY_HEX, LIVE)).toBeNull();
  });

  it("rejects the wrong number of segments", () => {
    expect(parse("cru1.onlytwo", GO_FIXTURE_PUBKEY_HEX, LIVE)).toBeNull();
    expect(parse("cru1.a.b.c", GO_FIXTURE_PUBKEY_HEX, LIVE)).toBeNull();
  });
});

describe("expiry and grace window", () => {
  it("is valid and not in grace before expiry", () => {
    const lic = parse(GO_ENTERPRISE_KEY, GO_FIXTURE_PUBKEY_HEX, LIVE);
    expect(lic).not.toBeNull();
    expect(lic!.inGrace).toBe(false);
  });

  it("is valid but flagged inGrace within 14 days past expiry", () => {
    const inGrace = new Date(EXPIRES.getTime() + 10 * 24 * 60 * 60 * 1000);
    const lic = parse(GO_ENTERPRISE_KEY, GO_FIXTURE_PUBKEY_HEX, inGrace);
    expect(lic).not.toBeNull();
    expect(lic!.inGrace).toBe(true);
  });

  it("is invalid past the 14-day grace window", () => {
    const pastGrace = new Date(EXPIRES.getTime() + 15 * 24 * 60 * 60 * 1000);
    expect(parse(GO_ENTERPRISE_KEY, GO_FIXTURE_PUBKEY_HEX, pastGrace)).toBeNull();
  });

  it("treats the exact grace boundary as still valid", () => {
    const boundary = new Date(EXPIRES.getTime() + 14 * 24 * 60 * 60 * 1000);
    expect(parse(GO_ENTERPRISE_KEY, GO_FIXTURE_PUBKEY_HEX, boundary)).not.toBeNull();
  });
});

describe("loadLicense env handling", () => {
  const orig = { ...process.env };

  it("returns null (community) when CRUCIBLE_LICENSE_KEY is unset", () => {
    delete process.env.CRUCIBLE_LICENSE_KEY;
    delete process.env.CRUCIBLE_LICENSE_PUBKEY;
    expect(loadLicense(LIVE)).toBeNull();
    process.env = { ...orig };
  });

  it("returns null (never throws) on a garbage key", () => {
    process.env.CRUCIBLE_LICENSE_KEY = "not-a-license";
    expect(loadLicense(LIVE)).toBeNull();
    process.env = { ...orig };
  });

  it("loads a valid key when the pubkey override matches the signer", () => {
    process.env.CRUCIBLE_LICENSE_KEY = GO_ENTERPRISE_KEY;
    process.env.CRUCIBLE_LICENSE_PUBKEY = GO_FIXTURE_PUBKEY_HEX;
    const lic = loadLicense(LIVE);
    expect(lic).not.toBeNull();
    expect(hasFeature(lic, FEATURE_SSO)).toBe(true);
    process.env = { ...orig };
  });
});

describe("hasFeature null-safety", () => {
  it("returns false for a null license", () => {
    expect(hasFeature(null, FEATURE_SSO)).toBe(false);
    expect(hasFeature(null, FEATURE_AUDIT_EXPORT)).toBe(false);
  });
});
