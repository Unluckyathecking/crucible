import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { createHmac } from "node:crypto";
import {
  verifyWebhook,
  WebhookVerificationError,
  DEFAULT_TOLERANCE_MS,
  SIGNATURE_HEADER,
  TIMESTAMP_HEADER,
  WEBHOOK_EVENT_ID_HEADER,
  WEBHOOK_EVENT_TYPE_HEADER,
} from "../src/webhook";

// testSign replicates gateway/internal/webhookout.Sign — MUST be kept in sync.
// Any change to the gateway signing algorithm requires updating this helper and
// the known-good reference vector in the hardcoded-vector test above.
// Three chained .update() calls mirror the production signing algorithm exactly,
// consistent with the Go testSign helper.
function testSign(secret: Buffer, timestamp: string, body: Buffer): string {
  return createHmac("sha256", secret)
    .update(timestamp)
    .update(".")
    .update(body)
    .digest("hex");
}

// 10 seconds in the past absorbs event-loop descheduling and minor clock skew
// without approaching the 5-minute tolerance used by the tests.
function nowTs(): string {
  return Math.floor((Date.now() - 10000) / 1000).toString();
}

function expectWebhookError(fn: () => void, messageSubstring?: string): void {
  let thrown: unknown;
  try {
    fn();
  } catch (e) {
    thrown = e;
  }
  if (thrown === undefined) {
    assert.fail("expected WebhookVerificationError, but no error was thrown");
  }
  assert.ok(
    thrown instanceof WebhookVerificationError,
    `expected WebhookVerificationError, got ${thrown}`,
  );
  if (messageSubstring) {
    assert.ok(
      thrown.message.includes(messageSubstring),
      `expected error message to include "${messageSubstring}", got "${thrown.message}"`,
    );
  }
}

/** SHA-256 hex digest length: 32 bytes × 2 hex chars each. */
const SHA256_HEX_LEN = 32 * 2;

const secret = Buffer.alloc(32, 0x42);
const secretHex = secret.toString("hex");
// body is a shared test fixture. No test in this suite mutates its byte values —
// all tests treat it as read-only input. Tests that need a different payload
// declare a local const (e.g. vectorBody, emptyBody, utf8Body) rather than
// modifying this shared buffer, eliminating any cross-test contamination risk.
const body = Buffer.from('{"event":"delivery.succeeded","data":{"id":1}}');

describe("verifyWebhook", () => {
  it("verifies known-good hardcoded reference vector (independent of testSign)", () => {
    // Pre-computed reference vector — independent of testSign. Catches algorithmic
    // drift between this SDK and the gateway signer.
    // secret=0x00×32, timestamp="1700000000" (2023-11-14), body={"event":"test"}
    // HMAC-SHA256 = 247d0f12bc3bef311cdb44ced37a1192ba82e78ffe8edd22fbf2205a414e94f5
    const vectorSecretHex = "00".repeat(32);
    const vectorBody = Buffer.from('{"event":"test"}');
    const header = `t=1700000000,v1=247d0f12bc3bef311cdb44ced37a1192ba82e78ffe8edd22fbf2205a414e94f5`;
    // If the system clock predates the reference, verifyWebhook rejects the timestamp
    // as "in the future" regardless of tolerance — skip, don't clamp or widen the check.
    const vectorNow = Date.now();
    const vectorTsMs = 1700000000 * 1000;
    if (vectorNow < vectorTsMs) {
      return; // system clock predates reference vector; skip this time-dependent test
    }
    const vectorTolerance = vectorNow - vectorTsMs + 3600_000;
    const result = verifyWebhook(vectorSecretHex, header, vectorBody, vectorTolerance);
    assert.equal(result, undefined);
  });

  it("verifies a valid signature (positive vector, matches gateway Sign)", () => {
    const ts = nowTs();
    const sig = testSign(secret, ts, body);
    const header = `t=${ts},v1=${sig}`;
    const result = verifyWebhook(secretHex, header, body);
    assert.equal(result, undefined);
  });

  it("rejects non-Buffer body (catches common express.raw() misconfiguration)", () => {
    const ts = nowTs();
    const sig = testSign(secret, ts, body);
    const header = `t=${ts},v1=${sig}`;
    expectWebhookError(
      () => verifyWebhook(secretHex, header, '{"event":"test"}' as unknown as Buffer),
      "body must be a Buffer",
    );
  });

  it("accepts body as Buffer created from a UTF-8 string", () => {
    const ts = nowTs();
    const bodyStr = '{"event":"string-body"}';
    const bodyBuf = Buffer.from(bodyStr, "utf8");
    const sig = testSign(secret, ts, bodyBuf);
    const header = `t=${ts},v1=${sig}`;
    const result = verifyWebhook(secretHex, header, bodyBuf);
    assert.equal(result, undefined);
  });

  it("uses DEFAULT_TOLERANCE_MS when toleranceMs is omitted", () => {
    const ts = nowTs();
    const sig = testSign(secret, ts, body);
    const header = `t=${ts},v1=${sig}`;
    // Should not throw: timestamp is current and DEFAULT_TOLERANCE_MS is 5 min
    const result = verifyWebhook(secretHex, header, body);
    assert.equal(result, undefined);
  });

  it("explicit toleranceMs=0 is zero-width tolerance, not a default sentinel", () => {
    // Unlike Go (where tolerance==0 maps to DefaultTolerance via zero-value sentinel),
    // TypeScript uses undefined as the "use default" signal. Explicit 0 must mean
    // zero-width tolerance, rejecting any timestamp that is not exactly current.
    //
    // Distinguishing zero-width (0 ms) from the DEFAULT (5 min = 300,000 ms):
    //   nowTs() is 10 s (10,000 ms) in the past.
    //   - With 0 ms tolerance: 10,000 > 0 → rejected ✓ (what this test asserts)
    //   - With DEFAULT (300,000 ms): 10,000 < 300,000 → accepted → test would FAIL
    //   (no error thrown when one is expected) — so this test distinguishes the two.
    const ts = nowTs(); // 10 seconds in the past
    const sig = testSign(secret, ts, body);
    const header = `t=${ts},v1=${sig}`;
    expectWebhookError(() => verifyWebhook(secretHex, header, body, 0), "too old");
  });

  it("rejects tampered body", () => {
    const ts = nowTs();
    const sig = testSign(secret, ts, body);
    const header = `t=${ts},v1=${sig}`;
    expectWebhookError(
      () => verifyWebhook(secretHex, header, Buffer.from('{"event":"tampered"}')),
      "no matching v1 signature",
    );
  });

  it("rejects wrong secret", () => {
    const ts = nowTs();
    const wrongSecret = Buffer.alloc(32, 0xff);
    const sig = testSign(wrongSecret, ts, body);
    const header = `t=${ts},v1=${sig}`;
    expectWebhookError(() => verifyWebhook(secretHex, header, body), "no matching v1 signature");
  });

  it("rejects expired timestamp", () => {
    const oldTs = Math.floor((Date.now() - 10 * 60 * 1000) / 1000).toString();
    const sig = testSign(secret, oldTs, body);
    const header = `t=${oldTs},v1=${sig}`;
    expectWebhookError(() => verifyWebhook(secretHex, header, body, DEFAULT_TOLERANCE_MS), "too old");
  });

  it("rejects future timestamp", () => {
    const futureTs = Math.floor((Date.now() + 10 * 60 * 1000) / 1000).toString();
    const sig = testSign(secret, futureTs, body);
    const header = `t=${futureTs},v1=${sig}`;
    expectWebhookError(() => verifyWebhook(secretHex, header, body, DEFAULT_TOLERANCE_MS), "future");
  });

  it("accepts second v1= candidate when first is invalid", () => {
    const ts = nowTs();
    const validSig = testSign(secret, ts, body);
    const invalidSig = "a".repeat(SHA256_HEX_LEN);
    const header = `t=${ts},v1=${invalidSig},v1=${validSig}`;
    const result = verifyWebhook(secretHex, header, body);
    assert.equal(result, undefined);
  });

  it("rejects v1 candidate with non-hex characters (all 64 chars are non-hex)", () => {
    const ts = nowTs();
    const nonHexSig = "g".repeat(SHA256_HEX_LEN);
    const header = `t=${ts},v1=${nonHexSig}`;
    expectWebhookError(() => verifyWebhook(secretHex, header, body), "no matching v1 signature");
  });

  it("rejects v1 candidate that is 65 chars (valid hex + trailing non-hex)", () => {
    const ts = nowTs();
    const validSig = testSign(secret, ts, body);
    // Appending a non-hex char makes sig 65 chars; Buffer.from would still decode
    // 32 bytes from the first 64 chars without the explicit sig.length !== 64 guard.
    const header = `t=${ts},v1=${validSig}X`;
    expectWebhookError(() => verifyWebhook(secretHex, header, body), "no matching v1 signature");
  });

  it("rejects valid sig placed past maxSigCandidates bound (9th of 8)", () => {
    const ts = nowTs();
    const validSig = testSign(secret, ts, body);
    const fakeSigs = Array<string>(8)
      .fill("b".repeat(SHA256_HEX_LEN))
      .map((s) => `v1=${s}`)
      .join(",");
    const header = `t=${ts},${fakeSigs},v1=${validSig}`;
    // The valid sig is dropped (not parsed) due to the candidate cap, so no
    // candidate matches — the error must be "no matching v1 signature", not "malformed".
    expectWebhookError(() => verifyWebhook(secretHex, header, body), "no matching v1 signature");
  });

  it("throws on missing header", () => {
    expectWebhookError(() => verifyWebhook(secretHex, "", body));
  });

  it("rejects header with timestamp but no signature", () => {
    const ts = nowTs();
    expectWebhookError(() => verifyWebhook(secretHex, `t=${ts}`, body));
  });

  it("rejects header with signature but no timestamp", () => {
    const ts = nowTs();
    const sig = testSign(secret, ts, body);
    expectWebhookError(() => verifyWebhook(secretHex, `v1=${sig}`, body));
  });

  it("rejects unknown key with empty value (foo=)", () => {
    const ts = nowTs();
    const sig = testSign(secret, ts, body);
    // Mirrors Go's TestVerifyWebhook_unknownKeyEmptyValue: unknown keys with empty
    // values must be rejected, not silently ignored.
    const header = `t=${ts},v1=${sig},foo=`;
    expectWebhookError(() => verifyWebhook(secretHex, header, body), "malformed");
  });

  it("ignores unknown key with non-empty value (forward compatibility)", () => {
    const ts = nowTs();
    const sig = testSign(secret, ts, body);
    // Unknown keys with non-empty values (e.g. future v2=...) must be silently
    // ignored so receivers remain compatible with new gateway fields.
    const header = `t=${ts},v1=${sig},foo=bar`;
    const result = verifyWebhook(secretHex, header, body);
    assert.equal(result, undefined);
  });

  it("rejects part with empty key (=value at position 0)", () => {
    const ts = nowTs();
    const sig = testSign(secret, ts, body);
    // "=foo" has indexOf("=") === 0, which would pass idx < 0 but must fail idx <= 0.
    const header = `t=${ts},v1=${sig},=extra`;
    expectWebhookError(() => verifyWebhook(secretHex, header, body), "malformed");
  });

  it("verifies empty body", () => {
    const ts = nowTs();
    const emptyBody = Buffer.alloc(0);
    const sig = testSign(secret, ts, emptyBody);
    const header = `t=${ts},v1=${sig}`;
    const result = verifyWebhook(secretHex, header, emptyBody);
    assert.equal(result, undefined);
  });

  it("rejects empty secretHex", () => {
    const ts = nowTs();
    const sig = testSign(secret, ts, body);
    const header = `t=${ts},v1=${sig}`;
    expectWebhookError(() => verifyWebhook("", header, body));
  });

  it("rejects odd-length secretHex", () => {
    const ts = nowTs();
    const sig = testSign(secret, ts, body);
    const header = `t=${ts},v1=${sig}`;
    // odd-length hex would silently truncate in Buffer.from; we reject it explicitly
    expectWebhookError(() => verifyWebhook("abc", header, body));
  });

  it("rejects non-hex secretHex", () => {
    const ts = nowTs();
    const sig = testSign(secret, ts, body);
    const header = `t=${ts},v1=${sig}`;
    expectWebhookError(() => verifyWebhook("zzzz", header, body));
  });

  it("exposes DEFAULT_TOLERANCE_MS as 5 minutes", () => {
    assert.equal(DEFAULT_TOLERANCE_MS, 5 * 60 * 1000);
  });

  it("rejects negative toleranceMs", () => {
    const ts = nowTs();
    const sig = testSign(secret, ts, body);
    const header = `t=${ts},v1=${sig}`;
    expectWebhookError(() => verifyWebhook(secretHex, header, body, -1), "negative tolerance");
  });

  it("rejects NaN toleranceMs (would disable replay protection via IEEE 754 comparisons)", () => {
    const ts = nowTs();
    const sig = testSign(secret, ts, body);
    const header = `t=${ts},v1=${sig}`;
    expectWebhookError(() => verifyWebhook(secretHex, header, body, NaN), "finite");
  });

  it("rejects Infinity toleranceMs (would disable replay protection via IEEE 754 comparisons)", () => {
    const ts = nowTs();
    const sig = testSign(secret, ts, body);
    const header = `t=${ts},v1=${sig}`;
    expectWebhookError(() => verifyWebhook(secretHex, header, body, Infinity), "finite");
  });

  it("rejects -Infinity toleranceMs", () => {
    const ts = nowTs();
    const sig = testSign(secret, ts, body);
    const header = `t=${ts},v1=${sig}`;
    expectWebhookError(() => verifyWebhook(secretHex, header, body, -Infinity), "finite");
  });

  it("rejects leading-zero timestamp (round-trip check: parseInt('0123') → 123 → '123' !== '0123')", () => {
    const ts = "0" + nowTs(); // prepend 0 to produce a leading-zero form
    const sig = testSign(secret, ts, body);
    const header = `t=${ts},v1=${sig}`;
    expectWebhookError(() => verifyWebhook(secretHex, header, body), "bad timestamp");
  });

  it("rejects header exceeding MAX_HEADER_PARTS (17 segments)", () => {
    const ts = nowTs();
    const sig = testSign(secret, ts, body);
    // 1 t= + 1 v1= + 15 unknown filler = 17 parts → exceeds MAX_HEADER_PARTS
    const filler = Array.from({ length: 15 }, (_, i) => `x${i}=y`).join(",");
    const header = `t=${ts},v1=${sig},${filler}`;
    expectWebhookError(() => verifyWebhook(secretHex, header, body), "malformed");
  });

  it("accepts header at MAX_HEADER_PARTS boundary (16 segments)", () => {
    const ts = nowTs();
    const sig = testSign(secret, ts, body);
    // 1 t= + 1 v1= + 14 unknown filler = 16 parts → exactly MAX_HEADER_PARTS
    const filler = Array.from({ length: 14 }, (_, i) => `x${i}=y`).join(",");
    const header = `t=${ts},v1=${sig},${filler}`;
    const result = verifyWebhook(secretHex, header, body);
    assert.equal(result, undefined);
  });

  it("accepts uppercase secretHex", () => {
    const ts = nowTs();
    const sig = testSign(secret, ts, body);
    const header = `t=${ts},v1=${sig}`;
    const upperSecret = secretHex.toUpperCase();
    const result = verifyWebhook(upperSecret, header, body);
    assert.equal(result, undefined);
  });

  it("rejects 16-digit timestamp (exceeds 15-char regex bound)", () => {
    const ts = "1000000000000000"; // 16 digits → /^\d{1,15}$/ fails
    const sig = "a".repeat(SHA256_HEX_LEN);
    const header = `t=${ts},v1=${sig}`;
    expectWebhookError(() => verifyWebhook(secretHex, header, body), "bad timestamp");
  });

  it("rejects timestamp where ts*1000 exceeds MAX_SAFE_INTEGER as future (cross-language parity)", () => {
    // 9007199254741 * 1000 = 9007199254741000 > Number.MAX_SAFE_INTEGER (~9.007e15).
    // Such a timestamp is far in the future (~year 2255), so we report "future" to
    // match Go's behavior (time.Unix rejects with "future", not "bad timestamp").
    const ts = "9007199254741";
    const sig = "a".repeat(SHA256_HEX_LEN);
    const header = `t=${ts},v1=${sig}`;
    expectWebhookError(() => verifyWebhook(secretHex, header, body), "future");
  });

  it("rejects empty v1= value (rejected at parse time as malformed)", () => {
    const ts = nowTs();
    // v1= with no value is rejected at parse time — not silently passed to the
    // length guard — so the error is "malformed", not "no matching v1 signature".
    const header = `t=${ts},v1=`;
    expectWebhookError(() => verifyWebhook(secretHex, header, body), "malformed");
  });

  it("rejects duplicate t= keys in header", () => {
    const ts = nowTs();
    const sig = testSign(secret, ts, body);
    // Valid ts comes first; attacker appends a stale one. Duplicate t= must be rejected
    // outright (not last-wins), so the stale appended value cannot bypass the age check.
    const header = `t=${ts},t=999,v1=${sig}`;
    expectWebhookError(() => verifyWebhook(secretHex, header, body), "malformed");
  });

  it("verifies body with multi-byte UTF-8 characters passed as Buffer", () => {
    const ts = nowTs();
    const utf8Body = Buffer.from('{"message":"hello 🎉 你好"}');
    const sig = testSign(secret, ts, utf8Body);
    const header = `t=${ts},v1=${sig}`;
    const result = verifyWebhook(secretHex, header, utf8Body);
    assert.equal(result, undefined);
  });

  it("rejects body with wrong encoding (latin1 bytes vs utf-8 signature)", () => {
    // Sign over the UTF-8 byte sequence; verify with latin1 bytes (different byte
    // values for non-ASCII code points) → HMAC mismatch.
    const ts = nowTs();
    const utf8Body = Buffer.from('{"msg":"hello 🎉"}', "utf8");
    const latin1Body = Buffer.from('{"msg":"hello 🎉"}', "latin1");
    const sig = testSign(secret, ts, utf8Body);
    const header = `t=${ts},v1=${sig}`;
    expectWebhookError(
      () => verifyWebhook(secretHex, header, latin1Body),
      "no matching v1 signature",
    );
  });

  it("exposes SIGNATURE_HEADER constant with correct value", () => {
    assert.equal(SIGNATURE_HEADER, "X-Crucible-Signature");
  });

  it("exposes TIMESTAMP_HEADER constant with correct value", () => {
    assert.equal(TIMESTAMP_HEADER, "X-Crucible-Timestamp");
  });

  it("exposes WEBHOOK_EVENT_ID_HEADER constant with correct value", () => {
    assert.equal(WEBHOOK_EVENT_ID_HEADER, "X-Webhook-Event-ID");
  });

  it("exposes WEBHOOK_EVENT_TYPE_HEADER constant with correct value", () => {
    assert.equal(WEBHOOK_EVENT_TYPE_HEADER, "X-Webhook-Event-Type");
  });

  it("rejects null toleranceMs (exercises !Number.isFinite guard)", () => {
    // null is not caught by the === undefined or === 0 sentinels, so it falls
    // through to the !Number.isFinite check (Number.isFinite(null) === false).
    const ts = nowTs();
    const sig = testSign(secret, ts, body);
    const header = `t=${ts},v1=${sig}`;
    expectWebhookError(
      () => verifyWebhook(secretHex, header, body, null as unknown as number),
      "finite",
    );
  });

  it("rejects v1 candidate shorter than 64 hex chars (32 chars = 16 bytes)", () => {
    const ts = nowTs();
    // 32-char hex string is half the expected SHA-256 output length; rejected by the
    // sig.length !== SHA256_HEX_LEN guard before Buffer.from is called, so timingSafeEqual
    // never sees a length mismatch. Mirrors Go's TestVerifyWebhook_v1TooShort.
    const shortSig = "a".repeat(SHA256_HEX_LEN / 2);
    const header = `t=${ts},v1=${shortSig}`;
    expectWebhookError(() => verifyWebhook(secretHex, header, body), "no matching v1 signature");
  });

  it("rejects v1 candidate longer than 64 hex chars (66 chars)", () => {
    const ts = nowTs();
    // 66-char hex string is 2 chars over the expected SHA-256 hex length; rejected by the
    // sig.length !== SHA256_HEX_LEN guard independently of hex validity. Mirrors Go's
    // TestVerifyWebhook_v1TooLong which tests valid-hex-but-wrong-length candidates.
    const longSig = "a".repeat(SHA256_HEX_LEN + 2);
    const header = `t=${ts},v1=${longSig}`;
    expectWebhookError(() => verifyWebhook(secretHex, header, body), "no matching v1 signature");
  });

  it("rejects part with embedded '=' in value (t=1=2 is structurally invalid)", () => {
    const ts = nowTs();
    const sig = testSign(secret, ts, body);
    // "t=1=2" has an embedded '=' in the timestamp value. The parser rejects any
    // key=value pair where the value itself contains '=', for cross-language parity
    // with Go's strings.Contains(kv[1], "=") guard.
    const header = `t=1=2,v1=${sig}`;
    expectWebhookError(() => verifyWebhook(secretHex, header, body), "malformed");
  });

  it("rejects null body (Buffer.isBuffer(null) returns false, caught by the guard)", () => {
    const ts = nowTs();
    const sig = testSign(secret, ts, body);
    const header = `t=${ts},v1=${sig}`;
    expectWebhookError(
      () => verifyWebhook(secretHex, header, null as unknown as Buffer),
      "body must be a Buffer",
    );
  });

  it("rejects Uint8Array body (Buffer.isBuffer rejects TypedArrays that are not Buffers)", () => {
    const ts = nowTs();
    const sig = testSign(secret, ts, body);
    const header = `t=${ts},v1=${sig}`;
    const uint8 = new Uint8Array(body);
    expectWebhookError(
      () => verifyWebhook(secretHex, header, uint8 as unknown as Buffer),
      "body must be a Buffer",
    );
  });

  it("accepts exactly maxSigCandidates candidates (7 fake + 1 valid = 8)", () => {
    const ts = nowTs();
    const validSig = testSign(secret, ts, body);
    const fakeSigs = Array<string>(7)
      .fill("b".repeat(SHA256_HEX_LEN))
      .map((s) => `v1=${s}`)
      .join(",");
    const header = `t=${ts},${fakeSigs},v1=${validSig}`;
    // 8th candidate (index 7) is within sigs.length < MAX_SIG_CANDIDATES, so accepted.
    const result = verifyWebhook(secretHex, header, body);
    assert.equal(result, undefined);
  });

  it("rejects when valid sig is 9th candidate (beyond maxSigCandidates=8)", () => {
    const ts = nowTs();
    const validSig = testSign(secret, ts, body);
    // 8 fakes consume the full candidate cap; the 9th (valid) sig is dropped.
    const fakeSigs = Array<string>(8)
      .fill("c".repeat(SHA256_HEX_LEN))
      .map((s) => `v1=${s}`)
      .join(",");
    const header = `t=${ts},${fakeSigs},v1=${validSig}`;
    expectWebhookError(() => verifyWebhook(secretHex, header, body), "no matching v1 signature");
  });
});
