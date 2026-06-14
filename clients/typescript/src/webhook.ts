import { createHmac, timingSafeEqual } from "node:crypto";

// maxSigCandidates caps parsed v1= values to prevent unbounded HMAC comparisons
// on header-stuffed requests. Mirrors gateway verifySignature.
const MAX_SIG_CANDIDATES = 8;

// SHA-256 produces 32 bytes; hex-encoded that is 64 characters.
const SHA256_BYTE_LEN = 32;
const SHA256_HEX_LEN = SHA256_BYTE_LEN * 2;

// maxHeaderParts caps total comma-separated segments to bound parsing over
// attacker-controlled input before the v1 candidate cap takes effect.
const MAX_HEADER_PARTS = 16;

// Pre-compiled; the {2}+ quantifier ensures non-empty, even-length, and valid hex
// in a single check — eliminates any ordering dependency between the three guards.
const SECRET_HEX_RE = /^([0-9a-fA-F]{2})+$/;

/** Default tolerance in ms: 5 minutes, matching the gateway's inbound replay window. */
export const DEFAULT_TOLERANCE_MS = 5 * 60 * 1000;

/** HTTP header carrying the t= timestamp and v1= HMAC digest. */
export const SIGNATURE_HEADER = "X-Crucible-Signature";

/** HTTP header carrying the delivery Unix timestamp. */
export const TIMESTAMP_HEADER = "X-Crucible-Timestamp";

/** HTTP header carrying the delivery UUID. Use it to deduplicate at-least-once deliveries. */
export const WEBHOOK_EVENT_ID_HEADER = "X-Webhook-Event-ID";

/** HTTP header carrying the event type string (e.g. "invoice.paid"). */
export const WEBHOOK_EVENT_TYPE_HEADER = "X-Webhook-Event-Type";

/** Thrown when X-Crucible-Signature verification fails. */
export class WebhookVerificationError extends Error {
  constructor(message: string) {
    super(message);
    this.name = "WebhookVerificationError";
    Object.setPrototypeOf(this, new.target.prototype);
  }
}

/**
 * verifyWebhook verifies the X-Crucible-Signature on a webhook delivery from the gateway.
 *
 * @param secretHex - hex-encoded signing secret from the dashboard endpoint page
 * @param sigHeader - raw value of the X-Crucible-Signature header (t=<ts>,v1=<hex>)
 * @param body - unmodified request body as Buffer or string
 * @param toleranceMs - maximum age in ms; pass 0 to use DEFAULT_TOLERANCE_MS (5 min)
 * @throws {WebhookVerificationError} when the signature does not match or is expired
 */
export function verifyWebhook(
  secretHex: string,
  sigHeader: string,
  body: Buffer | string,
  toleranceMs?: number,
): void {
  // Mirror Go's tolerance==0 sentinel: undefined (omitted) and explicit 0 both
  // mean "use default", matching the documented contract across both SDKs.
  if (toleranceMs === undefined || toleranceMs === 0) {
    toleranceMs = DEFAULT_TOLERANCE_MS;
  }
  // NaN and Infinity both bypass the < 0 and > toleranceMs comparisons (IEEE 754
  // comparisons with NaN/Infinity always return false), disabling replay protection.
  // Reject non-finite values before the negative check.
  if (!Number.isFinite(toleranceMs)) {
    throw new WebhookVerificationError("toleranceMs must be a finite number");
  }
  if (toleranceMs < 0) {
    throw new WebhookVerificationError("negative tolerance not allowed");
  }
  // ^([0-9a-fA-F]{2})+$ requires non-empty, even-length, and valid hex in one check.
  if (!SECRET_HEX_RE.test(secretHex)) {
    throw new WebhookVerificationError(
      "invalid secretHex: must be non-empty even-length hex string",
    );
  }
  const secret = Buffer.from(secretHex, "hex");
  // Defense in depth: verify the decoded length matches the hex length.
  // Catches any future Node.js behavior change in the hex codec.
  if (secret.length !== secretHex.length / 2) {
    throw new WebhookVerificationError("invalid secretHex: decode produced unexpected length");
  }

  const { timestamp, sigs } = parseSignatureHeader(sigHeader);

  // /^\d{1,15}$/ rejects non-decimal chars (whitespace, "0x") and bounds the string
  // to 15 digits — well above any real Unix timestamp, comfortably within safe-integer range.
  if (!/^\d{1,15}$/.test(timestamp)) {
    throw new WebhookVerificationError("bad timestamp in signature header");
  }
  const ts = parseInt(timestamp, 10);
  // Defense in depth: isSafeInteger guards precision loss; the round-trip check
  // catches parseInt's silent truncation of leading-zero or whitespace variants.
  if (!Number.isSafeInteger(ts) || ts.toString() !== timestamp) {
    throw new WebhookVerificationError("bad timestamp in signature header");
  }
  const nowMs = Date.now();
  const tsMs = ts * 1000;
  // ts * 1000 can exceed MAX_SAFE_INTEGER for timestamps beyond year ~2255 even when
  // ts itself is safe. Such a timestamp is definitively in the future, so reject with
  // the same error the age comparison would produce — mirrors Go's "future" rejection
  // and avoids silent precision loss in the subsequent comparisons.
  if (!Number.isSafeInteger(tsMs)) {
    throw new WebhookVerificationError("webhook timestamp in the future");
  }
  if (tsMs > nowMs) {
    throw new WebhookVerificationError("webhook timestamp in the future");
  }
  if (nowMs - tsMs > toleranceMs) {
    throw new WebhookVerificationError("webhook timestamp too old (replay protection)");
  }

  // Buffer is used verbatim (zero-copy, raw bytes preserved).
  // WARNING: String inputs are for testing only. Non-ASCII characters may fail
  // verification if the string encoding does not match the gateway's raw bytes
  // exactly. Production webhook handlers must pass the raw Buffer from the HTTP
  // framework (e.g. express.raw()) to avoid any encoding ambiguity.
  const bodyBuf = Buffer.isBuffer(body) ? body : Buffer.from(body, "utf8");
  const mac = createHmac("sha256", secret);
  mac.update(timestamp);
  mac.update(".");
  mac.update(bodyBuf);
  const expected = mac.digest();

  for (const sig of sigs) {
    // Reject wrong-length strings first: a SHA256_HEX_LEN-char hex string with a
    // trailing non-hex char still decodes to 32 bytes, bypassing the candidate.length check.
    if (sig.length !== SHA256_HEX_LEN) continue;
    const candidate = Buffer.from(sig, "hex");
    // Non-hex chars cause Buffer.from to produce a shorter buffer; timingSafeEqual
    // throws on length mismatch, so we must filter before calling it.
    if (candidate.length !== SHA256_BYTE_LEN) continue;
    if (timingSafeEqual(candidate, expected)) return;
  }
  throw new WebhookVerificationError("no matching v1 signature");
}

function parseSignatureHeader(header: string): { timestamp: string; sigs: string[] } {
  if (!header) {
    throw new WebhookVerificationError("missing X-Crucible-Signature header");
  }
  const parts = header.split(",");
  if (parts.length > MAX_HEADER_PARTS) {
    throw new WebhookVerificationError("malformed X-Crucible-Signature header");
  }
  let timestamp = "";
  const sigs: string[] = [];
  for (const part of parts) {
    const idx = part.indexOf("=");
    // idx <= 0 rejects both missing "=" (idx === -1) and empty key ("=value", idx === 0).
    if (idx <= 0) {
      throw new WebhookVerificationError("malformed X-Crucible-Signature header");
    }
    const key = part.slice(0, idx);
    const val = part.slice(idx + 1);
    // Reject empty values universally — mirrors Go's len(kv[1])==0 guard for cross-language
    // parity. Known keys (t=, v1=) also check individually below for defence-in-depth.
    if (val === "") {
      throw new WebhookVerificationError("malformed X-Crucible-Signature header");
    }
    if (key === "t") {
      if (timestamp !== "" || val === "") {
        throw new WebhookVerificationError("malformed X-Crucible-Signature header");
      }
      timestamp = val;
    } else if (key === "v1") {
      if (val === "") throw new WebhookVerificationError("malformed X-Crucible-Signature header");
      if (sigs.length < MAX_SIG_CANDIDATES) sigs.push(val);
    }
    // Unknown keys (e.g. future v2=) are silently ignored for forward compatibility.
  }
  if (!timestamp || sigs.length === 0) {
    throw new WebhookVerificationError("malformed X-Crucible-Signature header");
  }
  return { timestamp, sigs };
}
