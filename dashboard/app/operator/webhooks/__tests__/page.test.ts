/**
 * Drift-detection tests for the webhooks dead-letter admin page's filter-error
 * handling and endpoint-inactive safety caveats. We read the source rather
 * than import the page (which pulls in next/navigation and React Server
 * Component internals unavailable in this vitest environment), matching the
 * pattern used elsewhere in this repo (see the jobs page's own tests).
 */
import { describe, expect, it } from "vitest";
import * as fs from "fs";
import * as path from "path";

const pageSrc = fs.readFileSync(path.resolve(__dirname, "../page.tsx"), "utf8");
const actionsSrc = fs.readFileSync(path.resolve(__dirname, "../actions.ts"), "utf8");
const navSrc = fs.readFileSync(path.resolve(__dirname, "../../_components/operator-nav.tsx"), "utf8");

describe("webhooks admin page — table render", () => {
  it("renders dead-letter rows via listDeadLetters and the shared Pagination", () => {
    expect(pageSrc).toContain("listDeadLetters(");
    expect(pageSrc).toContain("<Pagination");
    expect(pageSrc).toContain("deadLetters.items.map(");
  });

  it("shows an empty state when there are no dead-letter deliveries", () => {
    expect(pageSrc).toContain("No dead-letter deliveries found.");
    expect(pageSrc).toContain("deadLetters.items.length === 0");
  });

  it("is linked from the operator nav between Plans and Jobs", () => {
    const plansIdx = navSrc.indexOf('href="/operator/plans"');
    const webhooksIdx = navSrc.indexOf('href="/operator/webhooks"');
    const jobsIdx = navSrc.indexOf('href="/operator/jobs"');
    expect(plansIdx).toBeGreaterThan(0);
    expect(webhooksIdx).toBeGreaterThan(plansIdx);
    expect(jobsIdx).toBeGreaterThan(webhooksIdx);
  });
});

describe("webhooks admin page — inline handling of a bad page filter", () => {
  it("wraps listDeadLetters in try/catch rather than letting it throw unguarded", () => {
    const callIdx = pageSrc.indexOf("await listDeadLetters(");
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
    expect(pageSrc).toContain('href="/operator/webhooks"');
  });
});

describe("webhooks admin page — inactive-endpoint safety", () => {
  it("disables both replay actions for a row whose endpoint is inactive", () => {
    expect(pageSrc).toContain("disabled={!delivery.endpoint_active}");
  });

  it("annotates an inactive endpoint's row instead of only disabling the button", () => {
    expect(pageSrc).toContain("endpoint inactive");
  });
});

describe("webhooks admin actions — error handling", () => {
  it("replayDeadLetterAction redirects with ?error= for a caller-fixable (400/404/409) failure", () => {
    const fnIdx = actionsSrc.indexOf("export async function replayDeadLetterAction");
    const catchIdx = actionsSrc.indexOf("catch (err)", fnIdx);
    const conditionIdx = actionsSrc.indexOf('err.status === 400 || err.status === 404 || err.status === 409', catchIdx);
    const rethrowIdx = actionsSrc.indexOf("throw err", catchIdx);
    expect(catchIdx).toBeGreaterThan(fnIdx);
    expect(conditionIdx).toBeGreaterThan(catchIdx);
    expect(rethrowIdx).toBeGreaterThan(conditionIdx);
  });

  it("surfaces the 409 ENDPOINT_INACTIVE case inline rather than letting it 500", () => {
    const fnIdx = actionsSrc.indexOf("export async function replayDeadLetterAction");
    const conditionIdx = actionsSrc.indexOf("err.status === 409", fnIdx);
    expect(conditionIdx).toBeGreaterThan(fnIdx);
  });

  it("replayDeadLetterAction redirects to ?replayed= on success", () => {
    const fnIdx = actionsSrc.indexOf("export async function replayDeadLetterAction");
    const nextFnIdx = actionsSrc.indexOf("export async function replayEndpointAction");
    const successRedirectIdx = actionsSrc.indexOf("`/operator/webhooks?replayed=${requeued}`", fnIdx);
    expect(successRedirectIdx).toBeGreaterThan(fnIdx);
    expect(successRedirectIdx).toBeLessThan(nextFnIdx);
  });

  it("replayEndpointAction only redirects with ?error= for a caller-fixable (400) failure", () => {
    const fnIdx = actionsSrc.indexOf("export async function replayEndpointAction");
    const catchIdx = actionsSrc.indexOf("catch (err)", fnIdx);
    const conditionIdx = actionsSrc.indexOf("err.status === 400", catchIdx);
    const rethrowIdx = actionsSrc.indexOf("throw err", catchIdx);
    expect(catchIdx).toBeGreaterThan(fnIdx);
    expect(conditionIdx).toBeGreaterThan(catchIdx);
    expect(rethrowIdx).toBeGreaterThan(conditionIdx);
  });

  it("replayEndpointAction redirects to ?replayed= on success", () => {
    const fnIdx = actionsSrc.indexOf("export async function replayEndpointAction");
    const successRedirectIdx = actionsSrc.indexOf("`/operator/webhooks?replayed=${requeued}`", fnIdx);
    expect(successRedirectIdx).toBeGreaterThan(fnIdx);
  });
});
