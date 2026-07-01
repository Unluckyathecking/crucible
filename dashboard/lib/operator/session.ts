// Operator session: a short-lived, HMAC-signed cookie distinct from the customer
// NextAuth session. Possessing a valid NextAuth session (a normal customer login)
// grants no access here — issuing this cookie requires presenting OPERATOR_TOKEN
// once at /operator/login. Signed with AUTH_SECRET (already a required, per-deployment
// secret) rather than a new env var; OPERATOR_TOKEN itself is never stored in the
// cookie, so the browser never holds anything derived from it beyond this session marker.
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

export interface OperatorSessionCookie {
  name: string;
  value: string;
  maxAge: number;
}

export async function createOperatorSessionCookie(): Promise<OperatorSessionCookie> {
  const exp = Math.floor(Date.now() / 1000) + SESSION_TTL_SECONDS;
  const payload = String(exp);
  const key = await hmacKey(getSigningSecret());
  const sig = await crypto.subtle.sign("HMAC", key, new TextEncoder().encode(payload));
  const value = `${payload}.${bytesToBase64Url(new Uint8Array(sig))}`;
  return { name: OPERATOR_SESSION_COOKIE, value, maxAge: SESSION_TTL_SECONDS };
}

export async function verifyOperatorSession(cookieValue: string | undefined | null): Promise<boolean> {
  if (!cookieValue) return false;
  const dot = cookieValue.indexOf(".");
  if (dot < 0) return false;
  const payload = cookieValue.slice(0, dot);
  const sigPart = cookieValue.slice(dot + 1);
  const exp = Number(payload);
  if (!Number.isFinite(exp) || exp < Math.floor(Date.now() / 1000)) return false;
  const sigBytes = base64UrlToBytes(sigPart);
  if (!sigBytes) return false;
  const key = await hmacKey(getSigningSecret());
  return crypto.subtle.verify("HMAC", key, sigBytes as BufferSource, new TextEncoder().encode(payload));
}

// constantTimeTokenEquals compares candidate against the configured operator
// token with timing normalized to a fixed-size digest, mirroring the gateway's
// subtle.ConstantTimeCompare (which the gateway applies to the raw bytes; here
// we hash first so comparison time never varies with the candidate's length).
export async function constantTimeTokenEquals(candidate: string, expected: string): Promise<boolean> {
  const [da, db] = await Promise.all([
    crypto.subtle.digest("SHA-256", new TextEncoder().encode(candidate)),
    crypto.subtle.digest("SHA-256", new TextEncoder().encode(expected)),
  ]);
  const ba = new Uint8Array(da);
  const bb = new Uint8Array(db);
  let diff = 0;
  for (let i = 0; i < ba.length; i++) diff |= ba[i] ^ bb[i];
  return diff === 0;
}
