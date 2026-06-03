/**
 * Drift-detection smoke tests for GET /api/usage route.ts.
 * These read the actual route source and assert security controls are present
 * so that removing them fails a test rather than silently regressing.
 * They do not execute the route or connect to any external service.
 */
import { describe, it, expect } from "vitest";
import * as fs from "fs";
import * as path from "path";

describe("GET /api/usage route.ts — CSRF guard drift-detection", () => {
  const routeSrc = fs.readFileSync(path.resolve(__dirname, "../route.ts"), "utf8");

  it("route enforces X-Requested-With header as CSRF signal before auth check", () => {
    expect(routeSrc).toContain("X-Requested-With");
    expect(routeSrc).toContain("xmlhttprequest");
  });

  it("CSRF check returns 403 Forbidden before reaching auth logic", () => {
    const csrfIdx = routeSrc.indexOf("X-Requested-With");
    const forbiddenIdx = routeSrc.indexOf('"Forbidden"', csrfIdx);
    const authIdx = routeSrc.indexOf("await auth()", csrfIdx);
    expect(forbiddenIdx).toBeGreaterThan(csrfIdx);
    expect(forbiddenIdx).toBeLessThan(authIdx);
  });
});
