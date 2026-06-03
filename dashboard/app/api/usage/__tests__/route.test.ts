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

  it("500 error response does not include errorId in JSON body (CLAUDE.md: internal IDs never escape)", () => {
    // errorId must only appear in the x-error-id header, never in the response body.
    const errorBodyIdx = routeSrc.indexOf('"Internal server error"');
    expect(errorBodyIdx).toBeGreaterThanOrEqual(0);
    // Find the JSON.stringify call that builds the 500 body
    const stringifyIdx = routeSrc.lastIndexOf("JSON.stringify", errorBodyIdx + 500);
    const closingBrace = routeSrc.indexOf(")", stringifyIdx);
    const bodyExpr = stringifyIdx >= 0 ? routeSrc.slice(stringifyIdx, closingBrace + 1) : "";
    // The body expression must NOT reference errorId — only the header may.
    expect(bodyExpr).not.toContain("errorId");
  });
});

// ---------------------------------------------------------------------------
// Drift-detection smoke tests for db.ts usage functions.
// These read the actual source and verify key SQL invariants so that structural
// regressions (e.g. removing customer_id filter or dropping the LIMIT cap) fail
// a test rather than silently reaching production.
// ---------------------------------------------------------------------------

describe("listUsageEvents in db.ts — drift-detection smoke tests", () => {
  const src = fs.readFileSync(path.resolve(__dirname, "../../../../lib/db.ts"), "utf8");
  const fnStart = src.indexOf("export async function listUsageEvents");
  const nextExport = src.indexOf("\nexport ", fnStart + 1);
  const fnSection = fnStart >= 0 ? src.slice(fnStart, nextExport > 0 ? nextExport : undefined) : "";

  it("listUsageEvents can be extracted from db.ts (extraction guard)", () => {
    expect(fnStart).toBeGreaterThanOrEqual(0);
    expect(fnSection.length).toBeGreaterThan(0);
  });

  it("listUsageEvents SQL scopes by customer_id (cross-customer isolation guard)", () => {
    expect(fnSection).toMatch(/WHERE\s+customer_id\s*=\s*\$1/);
  });

  it("listUsageEvents SQL uses half-open lower bound: created_at >= $N (from inclusive)", () => {
    expect(fnSection).toContain("created_at >= $");
  });

  it("listUsageEvents SQL uses half-open upper bound: created_at < $N (to exclusive)", () => {
    expect(fnSection).toContain("created_at < $");
  });

  it("listUsageEvents orders newest-first and is bounded (ORDER BY created_at DESC LIMIT)", () => {
    expect(fnSection).toContain("ORDER BY created_at DESC");
    expect(fnSection).toContain("LIMIT");
  });
});

describe("usageByOperation in db.ts — drift-detection smoke tests", () => {
  const src = fs.readFileSync(path.resolve(__dirname, "../../../../lib/db.ts"), "utf8");
  const fnStart = src.indexOf("export async function usageByOperation");
  const nextExport = src.indexOf("\nexport ", fnStart + 1);
  const fnSection = fnStart >= 0 ? src.slice(fnStart, nextExport > 0 ? nextExport : undefined) : "";

  it("usageByOperation can be extracted from db.ts (extraction guard)", () => {
    expect(fnStart).toBeGreaterThanOrEqual(0);
    expect(fnSection.length).toBeGreaterThan(0);
  });

  it("usageByOperation SQL scopes by customer_id (cross-customer isolation guard)", () => {
    expect(fnSection).toMatch(/WHERE\s+customer_id\s*=\s*\$1/);
  });

  it("usageByOperation SQL uses half-open interval (created_at >= and created_at <)", () => {
    expect(fnSection).toContain("created_at >= $");
    expect(fnSection).toContain("created_at < $");
  });

  it("usageByOperation SQL aggregates per operation (GROUP BY operation)", () => {
    expect(fnSection).toContain("GROUP BY operation");
  });

  it("usageByOperation results are bounded (LIMIT)", () => {
    expect(fnSection).toContain("LIMIT");
  });
});

describe("sumUsage in db.ts — drift-detection smoke tests", () => {
  const src = fs.readFileSync(path.resolve(__dirname, "../../../../lib/db.ts"), "utf8");
  const fnStart = src.indexOf("export async function sumUsage");
  const nextExport = src.indexOf("\nexport ", fnStart + 1);
  const fnSection = fnStart >= 0 ? src.slice(fnStart, nextExport > 0 ? nextExport : undefined) : "";

  it("sumUsage can be extracted from db.ts (extraction guard)", () => {
    expect(fnStart).toBeGreaterThanOrEqual(0);
    expect(fnSection.length).toBeGreaterThan(0);
  });

  it("sumUsage SQL scopes by customer_id", () => {
    expect(fnSection).toContain("customer_id");
  });

  it("sumUsage SQL uses INTERVAL '1 day' * $N syntax (correct PostgreSQL interval operand order)", () => {
    // PostgreSQL requires INTERVAL '1 day' * $N — not $N * INTERVAL '1 day'.
    // The reversed order has no interval * unknown overload and would cause a query error.
    expect(fnSection).toContain("INTERVAL '1 day' * $");
  });
});
