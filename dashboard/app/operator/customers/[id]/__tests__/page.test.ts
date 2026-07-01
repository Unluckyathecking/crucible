/**
 * Drift-detection tests for the customer detail page's usage-error handling.
 * We read the source rather than import the page (which pulls in next/navigation
 * and React Server Component internals unavailable in this vitest environment),
 * matching the pattern used elsewhere in this repo (see app/api/usage tests).
 */
import { describe, expect, it } from "vitest";
import * as fs from "fs";
import * as path from "path";

const src = fs.readFileSync(path.resolve(__dirname, "../page.tsx"), "utf8");

describe("customer detail page — customer-not-found vs usage-filter-error separation", () => {
  it("uses Promise.allSettled (not Promise.all) so one call's rejection doesn't fail the other", () => {
    expect(src).toContain("Promise.allSettled");
    expect(src).not.toMatch(/await Promise\.all\(/);
  });

  it("only a customer lookup 404/400 calls notFound()", () => {
    const customerBlockStart = src.indexOf("customerResult.status");
    const notFoundIdx = src.indexOf("notFound()", customerBlockStart);
    expect(customerBlockStart).toBeGreaterThanOrEqual(0);
    expect(notFoundIdx).toBeGreaterThan(customerBlockStart);
  });

  it("only usage errors with status === 400 are converted to the inline usageError message", () => {
    const usageBlockStart = src.indexOf("usageResult.status");
    const conditionIdx = src.indexOf('usageResult.reason.status === 400', usageBlockStart);
    expect(usageBlockStart).toBeGreaterThanOrEqual(0);
    expect(conditionIdx).toBeGreaterThan(usageBlockStart);
  });

  it("re-throws (does not swallow) non-400 usage failures", () => {
    const usageBlockStart = src.indexOf("usageResult.status");
    const elseThrowIdx = src.indexOf("throw usageResult.reason", usageBlockStart);
    expect(elseThrowIdx).toBeGreaterThan(usageBlockStart);
  });
});
