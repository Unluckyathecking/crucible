/**
 * Drift-detection tests for the audit log page's filter-error handling.
 * We read the source rather than import the page (which pulls in next/navigation
 * and React Server Component internals unavailable in this vitest environment),
 * matching the pattern used elsewhere in this repo (see app/api/usage tests and
 * the customer detail page's tests).
 */
import { describe, expect, it } from "vitest";
import * as fs from "fs";
import * as path from "path";

const src = fs.readFileSync(path.resolve(__dirname, "../page.tsx"), "utf8");

describe("audit log page — inline handling of bad date filters", () => {
  it("wraps listAuditEvents in try/catch rather than letting it throw unguarded", () => {
    const callIdx = src.indexOf("await listAuditEvents(");
    const tryIdx = src.lastIndexOf("try {", callIdx);
    expect(callIdx).toBeGreaterThan(0);
    expect(tryIdx).toBeGreaterThanOrEqual(0);
  });

  it("only converts status === 400 to the inline filterError message", () => {
    const catchIdx = src.indexOf("catch (err)");
    const conditionIdx = src.indexOf("err.status === 400", catchIdx);
    expect(catchIdx).toBeGreaterThanOrEqual(0);
    expect(conditionIdx).toBeGreaterThan(catchIdx);
  });

  it("re-throws (does not swallow) non-400 failures", () => {
    const catchIdx = src.indexOf("catch (err)");
    const rethrowIdx = src.indexOf("throw err", catchIdx);
    expect(rethrowIdx).toBeGreaterThan(catchIdx);
  });

  it("offers a reset-filters link when a filter error is shown", () => {
    expect(src).toContain("Reset filters");
    expect(src).toContain('href="/operator/audit"');
  });
});
