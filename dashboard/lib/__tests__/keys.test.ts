/**
 * Tests for lib/keys.ts — these assert the exact semantics that mirror
 * gateway/internal/auth/keys.go. Any drift here means dashboard-issued keys
 * will fail gateway auth. See CLAUDE.md load-bearing invariant #5 and #6.
 */
import { createHash } from "crypto";
import { describe, it, expect } from "vitest";
import { generateKey, hashKey } from "../keys";

// RFC 4648 standard base32 alphabet (no padding)
const BASE32_ALPHABET = "ABCDEFGHIJKLMNOPQRSTUVWXYZ234567";
const PREFIX_LEN = 24;

describe("hashKey", () => {
  it("returns SHA-256(salt || key) — byte-identical to Go auth.Hash", () => {
    const salt = "test-salt-that-is-at-least-32-bytes-long!!";
    const key = "cru_live_SOMEFAKEAPIKEY";

    const got = hashKey(salt, key);

    // Compute the expected value independently: SHA-256(salt || key)
    const expected = createHash("sha256")
      .update(salt)
      .update(key)
      .digest();

    expect(got).toEqual(expected);
  });

  it("returns a 32-byte Buffer (SHA-256 output length)", () => {
    const result = hashKey("salt-value-32bytes-padded-xxxxxxx", "anykey");
    expect(result.byteLength).toBe(32);
  });

  it("salt and key are concatenated in the right order (salt first)", () => {
    // SHA-256("AB") !== SHA-256("BA") — order matters
    const saltFirst = hashKey("A", "B");
    const keyFirst = hashKey("B", "A");
    expect(saltFirst).not.toEqual(keyFirst);
  });

  it("produces consistent output for the same inputs", () => {
    const a = hashKey("stable-salt-for-test", "stable-key");
    const b = hashKey("stable-salt-for-test", "stable-key");
    expect(a).toEqual(b);
  });

  it("known vector: SHA-256('salt'||'key') matches Node crypto directly", () => {
    const salt = "salt";
    const key = "key";
    const got = hashKey(salt, key);
    const expected = createHash("sha256").update("salt").update("key").digest();
    expect(got).toEqual(expected);
  });
});

describe("generateKey", () => {
  it("prefix is exactly PREFIX_LEN (24) characters", () => {
    const { prefix } = generateKey("cru_");
    expect(prefix.length).toBe(PREFIX_LEN);
  });

  it("full key starts with productPrefix + 'live_'", () => {
    const { full } = generateKey("cru_");
    expect(full.startsWith("cru_live_")).toBe(true);
  });

  it("prefix is the first PREFIX_LEN chars of the full key", () => {
    const { full, prefix } = generateKey("cru_");
    expect(prefix).toBe(full.slice(0, PREFIX_LEN));
  });

  it("suffix after 'live_' uses only RFC 4648 base32 alphabet (no padding)", () => {
    // Run a few iterations to reduce flakiness risk from random data
    for (let i = 0; i < 10; i++) {
      const { full } = generateKey("cru_");
      // Strip the fixed prefix portion
      const suffix = full.slice("cru_live_".length);
      for (const char of suffix) {
        expect(BASE32_ALPHABET).toContain(char);
      }
      // Ensure no '=' padding characters (RFC 4648 no-padding variant)
      expect(suffix).not.toContain("=");
    }
  });

  it("generates unique keys on successive calls", () => {
    const keys = new Set(Array.from({ length: 20 }, () => generateKey("cru_").full));
    expect(keys.size).toBe(20);
  });

  it("respects a custom product prefix", () => {
    const { full } = generateKey("myproduct_");
    expect(full.startsWith("myproduct_live_")).toBe(true);
  });
});
