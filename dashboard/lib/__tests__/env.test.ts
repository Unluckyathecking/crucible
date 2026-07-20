/**
 * Tests for lib/env.ts URL resolution.
 * ALLOWED_ORIGIN and DASHBOARD_BASE_URL are evaluated at module load, so each
 * case sets process.env, resets the module cache, then re-imports.
 */
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

const URL_VARS = ["AUTH_URL", "NEXTAUTH_URL", "DASHBOARD_ORIGIN"] as const;

const saved: Record<string, string | undefined> = {};

beforeEach(() => {
  for (const key of URL_VARS) {
    saved[key] = process.env[key];
    delete process.env[key];
  }
  vi.resetModules();
});

afterEach(() => {
  for (const key of URL_VARS) {
    if (saved[key] === undefined) delete process.env[key];
    else process.env[key] = saved[key];
  }
});

async function loadEnv() {
  vi.resetModules();
  return import("../env");
}

describe("DASHBOARD_BASE_URL resolution", () => {
  it("uses AUTH_URL when it is the only var set (the repo's compose/env default)", async () => {
    process.env.AUTH_URL = "http://localhost:3000";
    const { DASHBOARD_BASE_URL } = await loadEnv();
    // Regression guard: before the fix this fell through to the
    // http://localhost:3001 default, which is Grafana — Stripe redirects broke.
    expect(DASHBOARD_BASE_URL).toBe("http://localhost:3000");
  });

  it("prefers AUTH_URL over NEXTAUTH_URL and DASHBOARD_ORIGIN", async () => {
    process.env.AUTH_URL = "http://auth.example";
    process.env.NEXTAUTH_URL = "http://nextauth.example";
    process.env.DASHBOARD_ORIGIN = "http://origin.example";
    const { DASHBOARD_BASE_URL } = await loadEnv();
    expect(DASHBOARD_BASE_URL).toBe("http://auth.example");
  });

  it("falls back to NEXTAUTH_URL, then DASHBOARD_ORIGIN, then the local default", async () => {
    process.env.NEXTAUTH_URL = "http://nextauth.example";
    process.env.DASHBOARD_ORIGIN = "http://origin.example";
    expect((await loadEnv()).DASHBOARD_BASE_URL).toBe("http://nextauth.example");

    delete process.env.NEXTAUTH_URL;
    expect((await loadEnv()).DASHBOARD_BASE_URL).toBe("http://origin.example");

    delete process.env.DASHBOARD_ORIGIN;
    expect((await loadEnv()).DASHBOARD_BASE_URL).toBe("http://localhost:3001");
  });

  it("reads the same first var as ALLOWED_ORIGIN, so redirects and CSRF origin agree", async () => {
    process.env.AUTH_URL = "http://localhost:3000";
    const { DASHBOARD_BASE_URL, ALLOWED_ORIGIN } = await loadEnv();
    expect(DASHBOARD_BASE_URL).toBe(ALLOWED_ORIGIN);
  });
});
