import { afterEach, beforeEach, describe, expect, it } from "vitest";
import { OPERATOR_SESSION_COOKIE, constantTimeTokenEquals, createOperatorSessionCookie, verifyOperatorSession } from "../session";

const SECRET = "test-auth-secret-that-is-at-least-32-bytes!!";
const TOKEN_A = "test-operator-token-a-at-least-32-bytes-long";
const TOKEN_B = "test-operator-token-b-at-least-32-bytes-long";

beforeEach(() => {
  process.env.AUTH_SECRET = SECRET;
});

afterEach(() => {
  delete process.env.AUTH_SECRET;
});

function bytesToBase64Url(bytes: Uint8Array): string {
  let binary = "";
  for (let i = 0; i < bytes.length; i++) binary += String.fromCharCode(bytes[i]);
  return btoa(binary).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
}

async function digest(value: string): Promise<string> {
  const bytes = await crypto.subtle.digest("SHA-256", new TextEncoder().encode(value));
  return bytesToBase64Url(new Uint8Array(bytes));
}

async function signPayload(secret: string, payload: string): Promise<string> {
  const key = await crypto.subtle.importKey("raw", new TextEncoder().encode(secret), { name: "HMAC", hash: "SHA-256" }, false, ["sign"]);
  const sig = await crypto.subtle.sign("HMAC", key, new TextEncoder().encode(payload));
  return `${payload}.${bytesToBase64Url(new Uint8Array(sig))}`;
}

async function buildCookie(secret: string, exp: number, token: string): Promise<string> {
  const tokenDigest = await digest(token);
  return signPayload(secret, `${exp}.${tokenDigest}`);
}

describe("createOperatorSessionCookie / verifyOperatorSession", () => {
  it("uses a distinct cookie name from the customer NextAuth session", () => {
    expect(OPERATOR_SESSION_COOKIE).toBe("operator_session");
  });

  it("round-trips: a freshly created cookie verifies as authorized against the same token", async () => {
    const cookie = await createOperatorSessionCookie(TOKEN_A);
    expect(await verifyOperatorSession(cookie.value, TOKEN_A)).toBe(true);
  });

  it("rejects an undefined/empty cookie value", async () => {
    expect(await verifyOperatorSession(undefined, TOKEN_A)).toBe(false);
    expect(await verifyOperatorSession(null, TOKEN_A)).toBe(false);
    expect(await verifyOperatorSession("", TOKEN_A)).toBe(false);
  });

  it("rejects when no current token is supplied (e.g. OPERATOR_TOKEN unset)", async () => {
    const cookie = await createOperatorSessionCookie(TOKEN_A);
    expect(await verifyOperatorSession(cookie.value, undefined)).toBe(false);
  });

  it("rejects a cookie missing the exp.digest.signature structure", async () => {
    expect(await verifyOperatorSession("not-a-valid-cookie", TOKEN_A)).toBe(false);
    expect(await verifyOperatorSession("only.two-parts", TOKEN_A)).toBe(false);
  });

  it("rejects a cookie whose signature was tampered with", async () => {
    const cookie = await createOperatorSessionCookie(TOKEN_A);
    const parts = cookie.value.split(".");
    const sig = parts[2];
    // Tamper a middle character, not the last one: the final base64url char of a
    // 32-byte HMAC carries only 2 significant bits, so flipping it is a no-op on
    // the decoded signature ~1/16 of the time (flaky pass). A middle char is fully
    // significant, so the decoded bytes always change and the cookie always rejects.
    const mid = Math.floor(sig.length / 2);
    parts[2] = `${sig.slice(0, mid)}${sig[mid] === "A" ? "B" : "A"}${sig.slice(mid + 1)}`;
    expect(await verifyOperatorSession(parts.join("."), TOKEN_A)).toBe(false);
  });

  it("rejects a cookie signed with a different secret", async () => {
    const cookie = await buildCookie("a-completely-different-secret-32-bytes!!", Math.floor(Date.now() / 1000) + 3600, TOKEN_A);
    expect(await verifyOperatorSession(cookie, TOKEN_A)).toBe(false);
  });

  it("rejects an expired session even with a valid signature", async () => {
    const cookie = await buildCookie(SECRET, Math.floor(Date.now() / 1000) - 10, TOKEN_A);
    expect(await verifyOperatorSession(cookie, TOKEN_A)).toBe(false);
  });

  it("rejects a non-numeric expiry", async () => {
    const tokenDigest = await digest(TOKEN_A);
    const cookie = await signPayload(SECRET, `not-a-number.${tokenDigest}`);
    expect(await verifyOperatorSession(cookie, TOKEN_A)).toBe(false);
  });

  it("rejects a session issued under a token that has since been rotated (revocation on rotation)", async () => {
    const cookie = await createOperatorSessionCookie(TOKEN_A);
    // OPERATOR_TOKEN rotated in the environment; the cookie's digest no longer matches.
    expect(await verifyOperatorSession(cookie.value, TOKEN_B)).toBe(false);
  });

  it("re-authorizes once the operator logs in again under the new token", async () => {
    const rotated = await createOperatorSessionCookie(TOKEN_B);
    expect(await verifyOperatorSession(rotated.value, TOKEN_B)).toBe(true);
  });
});

describe("constantTimeTokenEquals", () => {
  it("returns true for identical tokens", async () => {
    expect(await constantTimeTokenEquals("abc123", "abc123")).toBe(true);
  });

  it("returns false for different tokens of the same length", async () => {
    expect(await constantTimeTokenEquals("abc123", "abc124")).toBe(false);
  });

  it("returns false for tokens of different lengths", async () => {
    expect(await constantTimeTokenEquals("short", "a-much-longer-candidate-token")).toBe(false);
  });

  it("returns false against an empty candidate", async () => {
    expect(await constantTimeTokenEquals("", "abc123")).toBe(false);
  });
});
