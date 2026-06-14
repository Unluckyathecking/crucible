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

// testSign replicates gateway/internal/webhookout.Sign so tests build the
// positive vector without importing the gateway package tree.
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
  assert.throws(fn, (err: unknown) => {
    assert.ok(
      err instanceof WebhookVerificationError,
      `expected WebhookVerificationError, got ${err}`,
    );
    if (messageSubstring) {
      assert.ok(
        (err as WebhookVerificationError).message.includes(messageSubstring),
        `expected error message to include "${messageSubstring}", got "${(err as WebhookVerificationError).message}"`,
      );
    }
    return true;
  });
}

/** SHA-256 hex digest length: 32 bytes × 2 hex chars each. */
const SHA256_HEX_LEN = 32 * 2;

const secret = Buffer.alloc(32, 0x42);
const secretHex = secret.toString("hex");
const body = Buffer.from('{"event":"delivery.succeeded","data":{"id":1}}');

describe("verifyWebhook", () => {
  it("verifies a valid signature (positive vector, matches gateway Sign)", () => {
    const ts = nowTs();
    const sig = testSign(secret, ts, body);
    const header = `t=${ts},v1=${sig}`;
    const result = verifyWebhook(secretHex, header, body);
    assert.equal(result, undefined);
  });

  it("accepts body as string (auto-encodes to Buffer before hashing)", () => {
    const ts = nowTs();
    const bodyStr = '{"event":"string-body"}';
    const bodyBuf = Buffer.from(bodyStr);
    const sig = testSign(secret, ts, bodyBuf);
    const header = `t=${ts},v1=${sig}`;
    const result = verifyWebhook(secretHex, header, bodyStr);
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

  it("uses DEFAULT_TOLERANCE_MS when toleranceMs is explicitly 0 (mirrors Go tolerance==0 sentinel)", () => {
    const ts = nowTs();
    const sig = testSign(secret, ts, body);
    const header = `t=${ts},v1=${sig}`;
    // tolerance=0 is the cross-language sentinel for "use default", same as Go.
    const result = verifyWebhook(secretHex, header, body, 0);
    assert.equal(result, undefined);
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
    expectWebhookError(() => verifyWebhook(secretHex, header, body));
  });

  it("rejects v1 candidate that is 65 chars (valid hex + trailing non-hex)", () => {
    const ts = nowTs();
    const validSig = testSign(secret, ts, body);
    // Appending a non-hex char makes sig 65 chars; Buffer.from would still decode
    // 32 bytes from the first 64 chars without the explicit sig.length !== 64 guard.
    const header = `t=${ts},v1=${validSig}X`;
    expectWebhookError(() => verifyWebhook(secretHex, header, body));
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

  it("rejects timestamp where ts*1000 exceeds MAX_SAFE_INTEGER (tsMs precision loss guard)", () => {
    // 9007199254741 * 1000 = 9007199254741000 > Number.MAX_SAFE_INTEGER (~9.007e15)
    const ts = "9007199254741";
    const sig = "a".repeat(SHA256_HEX_LEN);
    const header = `t=${ts},v1=${sig}`;
    expectWebhookError(() => verifyWebhook(secretHex, header, body), "bad timestamp");
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
    // Attacker prepends a valid ts then appends a different one; last-wins would bypass age check.
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

  it("verifies body with multi-byte UTF-8 characters passed as string", () => {
    const ts = nowTs();
    const utf8Str = '{"message":"hello 🎉 你好"}';
    const utf8Body = Buffer.from(utf8Str, "utf8");
    const sig = testSign(secret, ts, utf8Body);
    const header = `t=${ts},v1=${sig}`;
    // Buffer.from(str, 'utf8') must produce byte-identical output to the Buffer path.
    const result = verifyWebhook(secretHex, header, utf8Str);
    assert.equal(result, undefined);
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
});
