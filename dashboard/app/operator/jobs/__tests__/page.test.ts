/**
 * Drift-detection tests for the jobs admin page's filter-error handling and
 * in-flight-safety caveats. We read the source rather than import the page
 * (which pulls in next/navigation and React Server Component internals
 * unavailable in this vitest environment), matching the pattern used
 * elsewhere in this repo (see the audit page's own tests).
 */
import { describe, expect, it } from "vitest";
import * as fs from "fs";
import * as path from "path";

const pageSrc = fs.readFileSync(path.resolve(__dirname, "../page.tsx"), "utf8");
const actionsSrc = fs.readFileSync(path.resolve(__dirname, "../actions.ts"), "utf8");

describe("jobs admin page — inline handling of a bad status filter", () => {
  it("wraps listAdminJobs in try/catch rather than letting it throw unguarded", () => {
    const callIdx = pageSrc.indexOf("await listAdminJobs(");
    const tryIdx = pageSrc.lastIndexOf("try {", callIdx);
    expect(callIdx).toBeGreaterThan(0);
    expect(tryIdx).toBeGreaterThanOrEqual(0);
  });

  it("only converts status === 400 to the inline filterError message", () => {
    const catchIdx = pageSrc.indexOf("catch (err)");
    const conditionIdx = pageSrc.indexOf("err.status === 400", catchIdx);
    expect(catchIdx).toBeGreaterThanOrEqual(0);
    expect(conditionIdx).toBeGreaterThan(catchIdx);
  });

  it("re-throws (does not swallow) non-400 failures", () => {
    const catchIdx = pageSrc.indexOf("catch (err)");
    const rethrowIdx = pageSrc.indexOf("throw err", catchIdx);
    expect(rethrowIdx).toBeGreaterThan(catchIdx);
  });

  it("offers a reset-filters link when a filter error is shown", () => {
    expect(pageSrc).toContain("Reset filters");
    expect(pageSrc).toContain('href="/operator/jobs"');
  });
});

describe("jobs admin page — in-flight-safety caveats", () => {
  it("states the double-execution risk before offering the requeue action", () => {
    expect(pageSrc).toContain("risks double execution and double billing");
  });

  it("states the double-execution risk before offering the release action", () => {
    expect(pageSrc).toContain("risks a second,");
    expect(pageSrc).toContain("concurrent execution of work that instance is still processing");
  });

  it("only offers requeue for statuses where a worker can't still be running the job", () => {
    expect(pageSrc).toContain('new Set(["running", "failed"])');
  });
});

describe("jobs admin actions — error handling", () => {
  it("requeueJobAction only redirects with ?error= for a caller-fixable (400/404) failure", () => {
    const fnIdx = actionsSrc.indexOf("export async function requeueJobAction");
    const catchIdx = actionsSrc.indexOf("catch (err)", fnIdx);
    const conditionIdx = actionsSrc.indexOf("err.status === 400 || err.status === 404", catchIdx);
    const rethrowIdx = actionsSrc.indexOf("throw err", catchIdx);
    expect(catchIdx).toBeGreaterThan(fnIdx);
    expect(conditionIdx).toBeGreaterThan(catchIdx);
    expect(rethrowIdx).toBeGreaterThan(conditionIdx);
  });

  it("releaseJobsAction only redirects with ?error= for a caller-fixable (400) failure", () => {
    const fnIdx = actionsSrc.indexOf("export async function releaseJobsAction");
    const catchIdx = actionsSrc.indexOf("catch (err)", fnIdx);
    const conditionIdx = actionsSrc.indexOf("err.status === 400", catchIdx);
    const rethrowIdx = actionsSrc.indexOf("throw err", catchIdx);
    expect(catchIdx).toBeGreaterThan(fnIdx);
    expect(conditionIdx).toBeGreaterThan(catchIdx);
    expect(rethrowIdx).toBeGreaterThan(conditionIdx);
  });
});
