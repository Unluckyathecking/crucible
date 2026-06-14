import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { createHmac } from "node:crypto";
import {
  verifyWebhook,
  WebhookVerificationError,
  DEFAULT_TOLERANCE_MS,
} from "../src/webhook";

// testSign replicates gateway/internal/webhookout.Sign so tests build the
// positive vector without importing the gateway package tree.
function testSign(secret: Buffer, timestamp: string, body: Buffer): string {
  const mac = createHmac("sha256", secret);
  mac.update(timestamp);
  mac.update(".");
  mac.update(body);
  return mac.digest("hex");
}

const secret = Buffer.alloc(32, 0x42);
const secretHex = secret.toString("hex");
const body = Buffer.from('{"event":"delivery.succeeded","data":{"id":1}}');
const ts = Math.floor(Date.now() / 1000).toString();

describe("verifyWebhook", () => {
  it("verifies a valid signature (positive vector, matches gateway Sign)", () => {
    const sig = testSign(secret, ts, body);
    const header = `t=${ts},v1=${sig}`;
    assert.doesNotThrow(() => verifyWebhook(secretHex, header, body));
  });

  it("accepts body as string (auto-encodes to Buffer before hashing)", () => {
    const bodyStr = '{"event":"string-body"}';
    const bodyBuf = Buffer.from(bodyStr);
    const sig = testSign(secret, ts, bodyBuf);
    const header = `t=${ts},v1=${sig}`;
    assert.doesNotThrow(() => verifyWebhook(secretHex, header, bodyStr));
  });

  it("rejects tampered body", () => {
    const sig = testSign(secret, ts, body);
    const header = `t=${ts},v1=${sig}`;
    assert.throws(
      () => verifyWebhook(secretHex, header, Buffer.from('{"event":"tampered"}')),
      (err: unknown) => {
        assert.ok(err instanceof WebhookVerificationError, `expected WebhookVerificationError, got ${err}`);
        return true;
      },
    );
  });

  it("rejects wrong secret", () => {
    const wrongSecret = Buffer.alloc(32, 0xff);
    const sig = testSign(wrongSecret, ts, body);
    const header = `t=${ts},v1=${sig}`;
    assert.throws(
      () => verifyWebhook(secretHex, header, body),
      (err: unknown) => {
        assert.ok(err instanceof WebhookVerificationError);
        return true;
      },
    );
  });

  it("rejects expired timestamp", () => {
    const oldTs = Math.floor((Date.now() - 10 * 60 * 1000) / 1000).toString();
    const sig = testSign(secret, oldTs, body);
    const header = `t=${oldTs},v1=${sig}`;
    assert.throws(
      () => verifyWebhook(secretHex, header, body, 5 * 60 * 1000),
      (err: unknown) => {
        assert.ok(err instanceof WebhookVerificationError);
        return true;
      },
    );
  });

  it("accepts second v1= candidate when first is invalid", () => {
    const validSig = testSign(secret, ts, body);
    const invalidSig = "a".repeat(64);
    const header = `t=${ts},v1=${invalidSig},v1=${validSig}`;
    assert.doesNotThrow(() => verifyWebhook(secretHex, header, body));
  });

  it("rejects valid sig placed past maxSigCandidates bound (9th of 8)", () => {
    const validSig = testSign(secret, ts, body);
    const fakeSigs = Array<string>(8)
      .fill("b".repeat(64))
      .map((s) => `v1=${s}`)
      .join(",");
    const header = `t=${ts},${fakeSigs},v1=${validSig}`;
    assert.throws(
      () => verifyWebhook(secretHex, header, body),
      (err: unknown) => {
        assert.ok(err instanceof WebhookVerificationError);
        return true;
      },
    );
  });

  it("throws on missing header", () => {
    assert.throws(
      () => verifyWebhook(secretHex, "", body),
      (err: unknown) => {
        assert.ok(err instanceof WebhookVerificationError);
        return true;
      },
    );
  });

  it("exposes DEFAULT_TOLERANCE_MS as 5 minutes", () => {
    assert.equal(DEFAULT_TOLERANCE_MS, 5 * 60 * 1000);
  });
});
