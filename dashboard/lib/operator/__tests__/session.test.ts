import { afterEach, beforeEach, describe, expect, it } from "vitest";
import { OPERATOR_SESSION_COOKIE, constantTimeTokenEquals, createOperatorSessionCookie, verifyOperatorSession } from "../session";

const SECRET = "test-auth-secret-that-is-at-least-32-bytes!!";

beforeEach(() => {
  process.env.AUTH_SECRET = SECRET;
});

afterEach(() => {
  delete process.env.AUTH_SECRET;
});

async function signPayload(secret: string, payload: string): Promise<string> {
  const key = await crypto.subtle.importKey("raw", new TextEncoder().encode(secret), { name: "HMAC", hash: "SHA-256" }, false, ["sign"]);
  const sig = await crypto.subtle.sign("HMAC", key, new TextEncoder().encode(payload));
  const bytes = new Uint8Array(sig);
  let binary = "";
  for (let i = 0; i < bytes.length; i++) binary += String.fromCharCode(bytes[i]);
  const b64url = btoa(binary).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
  return `${payload}.${b64url}`;
}

describe("createOperatorSessionCookie / verifyOperatorSession", () => {
  it("uses a distinct cookie name from the customer NextAuth session", () => {
    expect(OPERATOR_SESSION_COOKIE).toBe("operator_session");
  });

  it("round-trips: a freshly created cookie verifies as authorized", async () => {
    const cookie = await createOperatorSessionCookie();
    expect(await verifyOperatorSession(cookie.value)).toBe(true);
  });

  it("rejects an undefined/empty cookie value", async () => {
    expect(await verifyOperatorSession(undefined)).toBe(false);
    expect(await verifyOperatorSession(null)).toBe(false);
    expect(await verifyOperatorSession("")).toBe(false);
  });

  it("rejects a cookie missing the payload.signature separator", async () => {
    expect(await verifyOperatorSession("not-a-valid-cookie")).toBe(false);
  });

  it("rejects a cookie whose signature was tampered with", async () => {
    const cookie = await createOperatorSessionCookie();
    const [payload, sig] = cookie.value.split(".");
    const tampered = `${payload}.${sig.slice(0, -1)}${sig.at(-1) === "A" ? "B" : "A"}`;
    expect(await verifyOperatorSession(tampered)).toBe(false);
  });

  it("rejects a cookie signed with a different secret", async () => {
    const cookie = await signPayload("a-completely-different-secret-32-bytes!!", String(Math.floor(Date.now() / 1000) + 3600));
    expect(await verifyOperatorSession(cookie)).toBe(false);
  });

  it("rejects an expired session even with a valid signature", async () => {
    const expiredPayload = String(Math.floor(Date.now() / 1000) - 10);
    const cookie = await signPayload(SECRET, expiredPayload);
    expect(await verifyOperatorSession(cookie)).toBe(false);
  });

  it("rejects a non-numeric payload", async () => {
    const cookie = await signPayload(SECRET, "not-a-number");
    expect(await verifyOperatorSession(cookie)).toBe(false);
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
