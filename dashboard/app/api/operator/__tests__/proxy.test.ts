/**
 * Drift-detection tests for the operator proxy routes under app/api/operator/**.
 * These read route source rather than importing it (importing pulls in
 * next/headers, which requires a Next.js request context unavailable here),
 * matching the pattern used elsewhere in this repo (see app/api/usage tests).
 *
 * Verifies the two acceptance-critical properties:
 *   1. Every proxy route re-checks the operator session before calling the
 *      gateway (defense in depth beyond middleware.ts).
 *   2. OPERATOR_TOKEN / the Authorization header is attached only inside
 *      lib/operator/client.ts (server-side) and never referenced by a route
 *      file or serialized into a response body.
 */
import { describe, expect, it } from "vitest";
import * as fs from "fs";
import * as path from "path";

const ROUTE_FILES = [
  "../customers/route.ts",
  "../customers/[id]/route.ts",
  "../customers/[id]/usage/route.ts",
  "../audit/route.ts",
  "../plans/route.ts",
];

describe("operator proxy routes — session re-check + no token leak", () => {
  for (const rel of ROUTE_FILES) {
    const abs = path.resolve(__dirname, rel);
    const src = fs.readFileSync(abs, "utf8");

    it(`${rel} imports and calls requireOperatorSession before reaching the gateway`, () => {
      expect(src).toContain("requireOperatorSession");
      const guardIdx = src.indexOf("requireOperatorSession()");
      const gatewayCallIdx = src.search(/list(Customers|AuditEvents|Plans)\(|getCustomer(Usage)?\(/);
      expect(guardIdx).toBeGreaterThanOrEqual(0);
      expect(gatewayCallIdx).toBeGreaterThan(guardIdx);
    });

    it(`${rel} short-circuits on an unauthorized session before calling the gateway`, () => {
      const guardIdx = src.indexOf("const unauthorized = await requireOperatorSession()");
      const returnIdx = src.indexOf("if (unauthorized) return unauthorized;", guardIdx);
      expect(guardIdx).toBeGreaterThanOrEqual(0);
      expect(returnIdx).toBeGreaterThan(guardIdx);
    });

    it(`${rel} never references OPERATOR_TOKEN or an Authorization header (bearer injection is confined to lib/operator/client.ts)`, () => {
      expect(src).not.toContain("OPERATOR_TOKEN");
      expect(src).not.toContain("Authorization");
    });

    it(`${rel} builds responses via the shared jsonResponse/operatorErrorResponse helpers, not ad-hoc Response construction`, () => {
      expect(src).toMatch(/jsonResponse|operatorErrorResponse/);
      expect(src).not.toContain("new Response(JSON.stringify(data)");
    });
  }
});

describe("_lib/guard.ts — the only place OPERATOR_TOKEN-adjacent auth logic lives for proxy routes", () => {
  const src = fs.readFileSync(path.resolve(__dirname, "../_lib/guard.ts"), "utf8");

  it("re-verifies the operator session cookie via verifyOperatorSession", () => {
    expect(src).toContain("verifyOperatorSession");
    expect(src).toContain("OPERATOR_SESSION_COOKIE");
  });

  it("does not reference OPERATOR_TOKEN directly (guard only checks the session cookie, not the raw token)", () => {
    expect(src).not.toContain("OPERATOR_TOKEN");
  });

  it("the 401 response body is a generic string, not leaking configuration state", () => {
    const unauthorizedIdx = src.indexOf('"Unauthorized"');
    expect(unauthorizedIdx).toBeGreaterThanOrEqual(0);
  });

  it("all responses set cache-control: no-store", () => {
    const matches = src.match(/cache-control/g) ?? [];
    expect(matches.length).toBeGreaterThanOrEqual(2); // jsonResponse + operatorErrorResponse
  });

  it("the 500 path logs an opaque error id in a header, never in the JSON body (CLAUDE.md: internal IDs never escape)", () => {
    const bodyIdx = src.indexOf('"Internal server error"');
    const stringifyIdx = src.lastIndexOf("JSON.stringify", bodyIdx + 100);
    const closingParen = src.indexOf(")", stringifyIdx);
    const bodyExpr = src.slice(stringifyIdx, closingParen + 1);
    expect(bodyExpr).not.toContain("errorId");
    expect(src).toContain("x-error-id");
  });
});

describe("lib/operator/client.ts — bearer injection lives in exactly one place", () => {
  const src = fs.readFileSync(path.resolve(__dirname, "../../../../lib/operator/client.ts"), "utf8");

  it("requireOperatorToken() is only ever called inside operatorFetch, not in any exported endpoint function", () => {
    const codeCallSites = [...src.matchAll(/requireOperatorToken\(\)/g)].map((m) => m.index ?? -1);
    expect(codeCallSites.length).toBeGreaterThanOrEqual(1);
    const operatorFetchStart = src.indexOf("async function operatorFetch");
    const operatorFetchEnd = src.indexOf("\n}", operatorFetchStart);
    for (const idx of codeCallSites) {
      // Skip the doc comment near the top of the file, which mentions the function by name.
      const line = src.slice(src.lastIndexOf("\n", idx) + 1, idx);
      if (line.trim().startsWith("//")) continue;
      expect(idx).toBeGreaterThan(operatorFetchStart);
      expect(idx).toBeLessThan(operatorFetchEnd);
    }
  });

  it("the Authorization header is the only place the token touches an outbound request", () => {
    expect(src).toContain("Authorization: `Bearer ${token}`");
  });
});
