import { createHmac, timingSafeEqual } from "node:crypto";

// maxSigCandidates caps parsed v1= values to prevent unbounded HMAC comparisons
// on header-stuffed requests. Mirrors gateway verifySignature.
const MAX_SIG_CANDIDATES = 8;

/** Default tolerance in ms: 5 minutes, matching the gateway's inbound replay window. */
export const DEFAULT_TOLERANCE_MS = 5 * 60 * 1000;

/** HTTP header carrying the t= timestamp and v1= HMAC digest. */
export const SIGNATURE_HEADER = "X-Crucible-Signature";

/** HTTP header carrying the delivery Unix timestamp. */
export const TIMESTAMP_HEADER = "X-Crucible-Timestamp";

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
 * @param toleranceMs - maximum age in ms; defaults to DEFAULT_TOLERANCE_MS (5 min)
 * @throws {WebhookVerificationError} when the signature does not match or is expired
 */
export function verifyWebhook(
  secretHex: string,
  sigHeader: string,
  body: Buffer | string,
  toleranceMs: number = DEFAULT_TOLERANCE_MS,
): void {
  if (!secretHex || secretHex.length % 2 !== 0 || !/^[0-9a-fA-F]+$/.test(secretHex)) {
    throw new WebhookVerificationError(
      "invalid secretHex: must be non-empty even-length hex string",
    );
  }
  const secret = Buffer.from(secretHex, "hex");

  const { timestamp, sigs } = parseSignatureHeader(sigHeader);

  // Validate before parsing: reject whitespace, hex prefixes, and non-digit chars.
  // /^\d+$/ is consistent with Go's strconv.ParseInt strict decimal parsing.
  if (!/^\d+$/.test(timestamp)) {
    throw new WebhookVerificationError("bad timestamp in signature header");
  }
  const ts = parseInt(timestamp, 10);
  if (!Number.isFinite(ts)) {
    throw new WebhookVerificationError("bad timestamp in signature header");
  }
  const ageMs = Math.abs(Date.now() - ts * 1000);
  if (ageMs > toleranceMs) {
    throw new WebhookVerificationError("webhook timestamp too old (replay protection)");
  }

  const bodyBuf = Buffer.isBuffer(body) ? body : Buffer.from(body);
  const mac = createHmac("sha256", secret);
  mac.update(timestamp);
  mac.update(".");
  mac.update(bodyBuf);
  const expected = mac.digest();

  for (const sig of sigs) {
    if (sig.length !== 64) continue;
    const candidate = Buffer.from(sig, "hex");
    // Non-hex chars cause Buffer.from to produce a shorter buffer; timingSafeEqual
    // throws on length mismatch, so we must filter before calling it.
    if (candidate.length !== 32) continue;
    if (timingSafeEqual(candidate, expected)) return;
  }
  throw new WebhookVerificationError("no matching v1 signature");
}

function parseSignatureHeader(header: string): { timestamp: string; sigs: string[] } {
  if (!header) {
    throw new WebhookVerificationError("missing X-Crucible-Signature header");
  }
  let timestamp = "";
  const sigs: string[] = [];
  for (const part of header.split(",")) {
    const idx = part.indexOf("=");
    if (idx < 0) continue;
    const key = part.slice(0, idx);
    const val = part.slice(idx + 1);
    if (key === "t") {
      timestamp = val;
    } else if (key === "v1" && sigs.length < MAX_SIG_CANDIDATES) {
      sigs.push(val);
    }
  }
  if (!timestamp || sigs.length === 0) {
    throw new WebhookVerificationError("malformed X-Crucible-Signature header");
  }
  return { timestamp, sigs };
}
