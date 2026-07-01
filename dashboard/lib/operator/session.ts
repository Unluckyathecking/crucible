// Operator session: a short-lived, HMAC-signed cookie distinct from the customer
// NextAuth session. Possessing a valid NextAuth session (a normal customer login)
// grants no access here — issuing this cookie requires presenting OPERATOR_TOKEN
// once at /operator/login. Signed with AUTH_SECRET (already a required, per-deployment
// secret) rather than a new env var; OPERATOR_TOKEN itself is never stored in the
// cookie, so the browser never holds anything derived from it beyond a one-way digest.
//
// The payload also binds a SHA-256 digest of OPERATOR_TOKEN at issuance time.
// Verification recomputes the digest of the *current* token and rejects on
// mismatch, so rotating OPERATOR_TOKEN (e.g. after suspected exposure)
// immediately invalidates every previously issued session instead of leaving
// them valid for up to the full TTL. The digest is one-way and safe to carry in
// the cookie: it can't be used to recover or forge the token.
//
// Uses Web Crypto (SubtleCrypto) exclusively, not node:crypto, so the same module
// works in both the Edge middleware and Node route handlers without a runtime split.

export const OPERATOR_SESSION_COOKIE = "operator_session";
const SESSION_TTL_SECONDS = 60 * 60 * 12; // 12h

function bytesToBase64Url(bytes: Uint8Array): string {
  let binary = "";
  for (let i = 0; i < bytes.length; i++) binary += String.fromCharCode(bytes[i]);
  return btoa(binary).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
}

function base64UrlToBytes(b64url: string): Uint8Array | null {
  try {
    const b64 = b64url.replace(/-/g, "+").replace(/_/g, "/");
    const padded = b64 + "=".repeat((4 - (b64.length % 4)) % 4);
    const binary = atob(padded);
    const bytes = new Uint8Array(binary.length);
    for (let i = 0; i < binary.length; i++) bytes[i] = binary.charCodeAt(i);
    return bytes;
  } catch {
    return null;
  }
}

function getSigningSecret(): string {
  const secret = process.env.AUTH_SECRET;
  if (!secret) throw new Error("AUTH_SECRET not configured");
  return secret;
}

async function hmacKey(secret: string): Promise<CryptoKey> {
  return crypto.subtle.importKey(
    "raw",
    new TextEncoder().encode(secret),
    { name: "HMAC", hash: "SHA-256" },
    false,
    ["sign", "verify"],
  );
}

async function digestToken(token: string): Promise<string> {
  const digest = await crypto.subtle.digest("SHA-256", new TextEncoder().encode(token));
  return bytesToBase64Url(new Uint8Array(digest));
}

// constantTimeStringEquals compares two strings with timing normalized to a
// fixed-size digest (mirrors the gateway's subtle.ConstantTimeCompare, which
// operates on raw bytes; hashing first here means comparison time never varies
// with either input's length).
async function constantTimeStringEquals(a: string, b: string): Promise<boolean> {
  const [da, db] = await Promise.all([
    crypto.subtle.digest("SHA-256", new TextEncoder().encode(a)),
    crypto.subtle.digest("SHA-256", new TextEncoder().encode(b)),
  ]);
  const ba = new Uint8Array(da);
  const bb = new Uint8Array(db);
  let diff = 0;
  for (let i = 0; i < ba.length; i++) diff |= ba[i] ^ bb[i];
  return diff === 0;
}

export interface OperatorSessionCookie {
  name: string;
  value: string;
  maxAge: number;
}

export async function createOperatorSessionCookie(operatorToken: string): Promise<OperatorSessionCookie> {
  const exp = Math.floor(Date.now() / 1000) + SESSION_TTL_SECONDS;
  const tokenDigest = await digestToken(operatorToken);
  const payload = `${exp}.${tokenDigest}`;
  const key = await hmacKey(getSigningSecret());
  const sig = await crypto.subtle.sign("HMAC", key, new TextEncoder().encode(payload));
  const value = `${payload}.${bytesToBase64Url(new Uint8Array(sig))}`;
  return { name: OPERATOR_SESSION_COOKIE, value, maxAge: SESSION_TTL_SECONDS };
}

// verifyOperatorSession requires the *current* OPERATOR_TOKEN so it can reject
// sessions issued under a since-rotated token, even though the raw token is
// never itself compared against anything stored client-side.
export async function verifyOperatorSession(
  cookieValue: string | undefined | null,
  currentOperatorToken: string | undefined,
): Promise<boolean> {
  if (!cookieValue || !currentOperatorToken) return false;
  const parts = cookieValue.split(".");
  if (parts.length !== 3) return false;
  const [expPart, tokenDigestPart, sigPart] = parts;
  const payload = `${expPart}.${tokenDigestPart}`;
  const exp = Number(expPart);
  if (!Number.isFinite(exp) || exp < Math.floor(Date.now() / 1000)) return false;
  const sigBytes = base64UrlToBytes(sigPart);
  if (!sigBytes) return false;
  const key = await hmacKey(getSigningSecret());
  const sigValid = await crypto.subtle.verify("HMAC", key, sigBytes as BufferSource, new TextEncoder().encode(payload));
  if (!sigValid) return false;
  const currentDigest = await digestToken(currentOperatorToken);
  return constantTimeStringEquals(tokenDigestPart, currentDigest);
}

// constantTimeTokenEquals compares a login-form candidate against the
// configured operator token. Exported separately from the internal
// constantTimeStringEquals used for digest comparison above so call sites
// document intent (comparing a raw candidate vs. comparing two digests).
export async function constantTimeTokenEquals(candidate: string, expected: string): Promise<boolean> {
  return constantTimeStringEquals(candidate, expected);
}
