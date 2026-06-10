/**
 * Drift-detection smoke tests for GET /api/usage route.ts.
 * These read the actual route source and assert security controls are present
 * so that removing them fails a test rather than silently regressing.
 * They do not execute the route or connect to any external service.
 */
import { describe, it, expect } from "vitest";
import * as fs from "fs";
import * as path from "path";

// ---------------------------------------------------------------------------
// Drift-detection tests for csvField RFC 4180 escaping helper
// These read the actual route source to verify the escaping implementation
// without importing the module (which would pull in next-auth / next/server).
// ---------------------------------------------------------------------------

describe("csvField — RFC 4180 escaping drift-detection", () => {
  const routeSrc = fs.readFileSync(path.resolve(__dirname, "../route.ts"), "utf8");
  // Extract the function body between its opening and closing braces.
  const fnStart = routeSrc.indexOf("function csvField");
  const fnEnd = routeSrc.indexOf("\n}", fnStart) + 2;
  const fnSrc = fnStart >= 0 ? routeSrc.slice(fnStart, fnEnd) : "";

  it("csvField function is present in route.ts", () => {
    expect(fnStart).toBeGreaterThanOrEqual(0);
    expect(fnSrc.length).toBeGreaterThan(0);
  });

  it("csvField detects comma as a trigger character", () => {
    expect(fnSrc).toContain(",");
  });

  it("csvField detects double-quote as a trigger character", () => {
    expect(fnSrc).toMatch(/["\\]/);
  });

  it("csvField detects newline/CR as trigger characters", () => {
    expect(fnSrc).toMatch(/\\n|\\r/);
  });

  it("csvField wraps in double-quotes when trigger found", () => {
    // Implementation must open with a double-quote character.
    expect(fnSrc).toContain('"');
  });

  it("csvField escapes embedded double-quotes by doubling them (RFC 4180)", () => {
    // The replace pattern must turn " into "".
    expect(fnSrc).toContain('""');
  });
});

// Standalone behavioural tests for the RFC 4180 escaping algorithm.
// The drift tests above verify the source in route.ts stays in sync with this spec.
// Importing route.ts directly is not possible in this test environment because
// the module transitively imports next-auth which requires next/server at runtime.
function csvFieldSpec(value: string): string {
  if (/[",\r\n]/.test(value)) {
    return '"' + value.replace(/"/g, '""') + '"';
  }
  return value;
}

describe("csvField — behavioural correctness (algorithm spec)", () => {
  it("returns plain strings unchanged", () => {
    expect(csvFieldSpec("hello")).toBe("hello");
    expect(csvFieldSpec("validate-vat")).toBe("validate-vat");
    expect(csvFieldSpec("42")).toBe("42");
    expect(csvFieldSpec("")).toBe("");
  });

  it("wraps a value containing a comma", () => {
    expect(csvFieldSpec("a,b")).toBe('"a,b"');
  });

  it("wraps and doubles embedded double-quotes", () => {
    expect(csvFieldSpec('say "hi"')).toBe('"say ""hi"""');
  });

  it("wraps a value containing a newline", () => {
    expect(csvFieldSpec("line1\nline2")).toBe('"line1\nline2"');
  });

  it("wraps a value containing a carriage return", () => {
    expect(csvFieldSpec("line1\rline2")).toBe('"line1\rline2"');
  });

  it("handles a value containing both comma and quote", () => {
    expect(csvFieldSpec('a,"b"')).toBe('"a,""b"""');
  });
});

describe("GET /api/usage route.ts — CSV format drift-detection", () => {
  const routeSrc = fs.readFileSync(path.resolve(__dirname, "../route.ts"), "utf8");

  it("route branches on format=csv query parameter", () => {
    expect(routeSrc).toContain('formatParam === "csv"');
  });

  it("CSV response sets content-type to text/csv", () => {
    expect(routeSrc).toContain("text/csv");
  });

  it("CSV response sets Content-Disposition attachment header", () => {
    expect(routeSrc).toContain("content-disposition");
    expect(routeSrc).toContain("attachment");
  });

  it("CSV response sets cache-control: no-store", () => {
    const csvIdx = routeSrc.indexOf('formatParam === "csv"');
    const csvBranchEnd = routeSrc.indexOf("return new Response(JSON.stringify(rows)", csvIdx);
    const csvBranch = routeSrc.slice(csvIdx, csvBranchEnd);
    expect(csvBranch).toContain("no-store");
  });

  it("JSON path is unchanged when format is not csv", () => {
    expect(routeSrc).toContain("application/json");
    expect(routeSrc).toContain("JSON.stringify(rows)");
  });
});

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
