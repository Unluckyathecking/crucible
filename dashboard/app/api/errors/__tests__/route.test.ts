/**
 * Drift-detection tests for GET /api/errors route.ts and the errors-client.
 * Read the actual source and assert security/validation controls are present
 * so that removing them fails a test rather than silently regressing.
 * Do not execute the route or connect to any external service.
 */
import { describe, it, expect } from "vitest";
import * as fs from "fs";
import * as path from "path";

const routeSrc = fs.readFileSync(path.resolve(__dirname, "../route.ts"), "utf8");
const clientSrc = fs.readFileSync(
  path.resolve(__dirname, "../../../dashboard/errors/errors-client.tsx"),
  "utf8",
);

// ---------------------------------------------------------------------------
// truncateUtf8Buffer — source-level drift-detection
// ---------------------------------------------------------------------------

describe("truncateUtf8Buffer in route.ts — drift-detection", () => {
  const fnStart = routeSrc.indexOf("function truncateUtf8Buffer");
  const fnEnd = routeSrc.indexOf("\n}", fnStart) + 2;
  const fnSrc = fnStart >= 0 ? routeSrc.slice(fnStart, fnEnd) : "";

  it("function is present", () => {
    expect(fnStart).toBeGreaterThanOrEqual(0);
  });

  it("loop checks buf[end - 1] not buf[end] (off-by-one guard)", () => {
    expect(fnSrc).toContain("buf[end - 1]");
    // Must not use buf[end] without -1 — that would read one byte past the slice.
    expect(fnSrc).not.toMatch(/buf\[end\][^-]/);
  });

  it("loop condition has end > 0 guard (prevents reading at index -1)", () => {
    expect(fnSrc).toMatch(/while\s*\(\s*end\s*>\s*0/);
  });

  it("loop body decrements end on every iteration (termination guaranteed)", () => {
    expect(fnSrc).toContain("end--");
  });

  it("continuation byte mask uses 0xc0 / 0x80 (matches 0b11000000 / 0b10000000)", () => {
    // Accept either hex or binary literal for the mask values.
    expect(fnSrc).toMatch(/0xc0|0b11000000/);
    expect(fnSrc).toMatch(/0x80|0b10000000/);
  });

  it("returns buf.toString with 'utf8' encoding and 0-based slice", () => {
    expect(fnSrc).toContain('buf.toString("utf8", 0, end)');
  });
});

// ---------------------------------------------------------------------------
// truncateUtf8Buffer — behavioural correctness (algorithm re-implemented here)
// ---------------------------------------------------------------------------

// This mirrors what route.ts does without importing the module
// (which would pull in next-auth / next/server at runtime).
function truncateUtf8BufferSpec(buf: Buffer, maxBytes: number): string {
  if (buf.length <= maxBytes) return buf.toString("utf8");
  let end = maxBytes;
  while (end > 0 && (buf[end - 1] & 0xc0) === 0x80) {
    end--;
  }
  return buf.toString("utf8", 0, end);
}

describe("truncateUtf8Buffer — behavioural correctness", () => {
  it("returns string unchanged when it fits within maxBytes", () => {
    const buf = Buffer.from("hello", "utf8");
    expect(truncateUtf8BufferSpec(buf, 100)).toBe("hello");
    expect(truncateUtf8BufferSpec(buf, 5)).toBe("hello");
  });

  it("truncates ASCII at byte boundary", () => {
    const buf = Buffer.from("abcde", "utf8");
    expect(truncateUtf8BufferSpec(buf, 3)).toBe("abc");
  });

  it("result is always valid for JSON serialisation (no JSON.stringify throws)", () => {
    const cases: [Buffer, number][] = [
      // ASCII boundary
      [Buffer.from("hello world", "utf8"), 5],
      // 2-byte sequence split — start byte lands at boundary
      [Buffer.from([0x61, 0xc3, 0xa9, 0x62]), 2],
      // 3-byte sequence split — continuation byte at boundary
      [Buffer.from([0xe2, 0x82, 0xac, 0x41]), 2],
      // 4-byte emoji split mid-sequence
      [Buffer.concat([Buffer.from("hi"), Buffer.from("😀", "utf8"), Buffer.from("!")]), 4],
      // All continuation bytes (invalid UTF-8 input)
      [Buffer.alloc(10, 0x80), 5],
    ];
    for (const [buf, maxBytes] of cases) {
      const out = truncateUtf8BufferSpec(buf, maxBytes);
      expect(() => JSON.stringify(out)).not.toThrow();
    }
  });

  it("output never contains a raw isolated continuation byte at the start", () => {
    // Walk-back removes trailing continuations so the first byte of the output
    // slice cannot be a continuation byte (0x80–0xBF) unless the INPUT started
    // with one (which is already invalid UTF-8 and becomes U+FFFD anyway).
    const buf = Buffer.from([0x41, 0xc3, 0xa9, 0x41]); // "AéA"
    const out = truncateUtf8BufferSpec(buf, 2);
    // Output is "A" followed optionally by U+FFFD; it must not start with �.
    expect(out.charCodeAt(0)).toBe(0x41); // 'A'
  });

  it("terminates without infinite loop on a buffer of all continuation bytes", () => {
    const buf = Buffer.alloc(100, 0x80);
    expect(() => truncateUtf8BufferSpec(buf, 50)).not.toThrow();
  });

  it("returns empty string when the entire window is continuation bytes", () => {
    const buf = Buffer.from([0x80, 0x80, 0x80]);
    // end walks all the way to 0; toString("utf8", 0, 0) = ""
    expect(truncateUtf8BufferSpec(buf, 2)).toBe("");
  });

  it("handles an invalid 4-byte start byte (0xF8) without looping", () => {
    // 0xF8 is not a continuation byte so the while-loop exits immediately;
    // there is no seqLen branch to enter — the function returns unconditionally.
    const buf = Buffer.from([0x41, 0xf8, 0x80, 0x80, 0x80, 0x41]);
    expect(() => truncateUtf8BufferSpec(buf, 3)).not.toThrow();
    const out = truncateUtf8BufferSpec(buf, 3);
    expect(() => JSON.stringify(out)).not.toThrow();
  });
});

// ---------------------------------------------------------------------------
// Future-date guard — drift-detection on errors-client.tsx (client component)
// ---------------------------------------------------------------------------

describe("errors-client.tsx — future-date validation drift-detection", () => {
  it("handleApply compares toMs against todayMs to reject future 'to' dates", () => {
    expect(clientSrc).toContain("toMs > todayMs");
  });

  it("todayMs is derived from todayUTC state (not a raw new Date())", () => {
    const todayMsIdx = clientSrc.indexOf("const todayMs");
    expect(todayMsIdx).toBeGreaterThanOrEqual(0);
    const lineEnd = clientSrc.indexOf("\n", todayMsIdx);
    const todayMsLine = clientSrc.slice(todayMsIdx, lineEnd);
    expect(todayMsLine).toContain("todayUTC");
  });

  it("future-date rejection sets a range error message", () => {
    expect(clientSrc).toContain("cannot be in the future");
  });

  it("handleApply also guards 'from' after 'to'", () => {
    expect(clientSrc).toContain("fromMs > toMs");
  });
});

// ---------------------------------------------------------------------------
// Pagination input validation — route.ts drift-detection
// ---------------------------------------------------------------------------

describe("GET /api/errors — pagination validation drift-detection", () => {
  it("page parameter is validated with /^\\d+$/ before parseInt", () => {
    expect(routeSrc).toMatch(/\/\^\\d\+\$\/\.test\(pageStr\)/);
  });

  it("limit parameter is validated with /^\\d+$/ before parseInt", () => {
    expect(routeSrc).toMatch(/\/\^\\d\+\$\/\.test\(limitStr\)/);
  });

  it("has a maximum page size cap (MAX_PAGE_SIZE)", () => {
    expect(routeSrc).toContain("MAX_PAGE_SIZE");
    expect(routeSrc).toContain("Math.min(limitRaw, MAX_PAGE_SIZE)");
  });

  it("guards against DoS via large offset", () => {
    expect(routeSrc).toContain("10_000_000");
  });
});

// ---------------------------------------------------------------------------
// Operation filter regex — route.ts drift-detection
// ---------------------------------------------------------------------------

describe("GET /api/errors — OPERATION_FILTER_RE drift-detection", () => {
  it("OPERATION_FILTER_RE is present", () => {
    expect(routeSrc).toContain("OPERATION_FILTER_RE");
  });

  it("regex uses non-capturing groups to prevent consecutive slashes", () => {
    expect(routeSrc).toContain("OPERATION_FILTER_RE");
    // The pattern must use (?:...) groups (non-capturing) for segment separation.
    expect(routeSrc).toMatch(/OPERATION_FILTER_RE\s*=\s*\/\^\\\/\(\?:/);
  });
});

// ---------------------------------------------------------------------------
// Cross-customer isolation — SQL drift-detection
// ---------------------------------------------------------------------------

describe("GET /api/errors — cross-customer isolation drift-detection", () => {
  it("listErrorEvents SQL scopes rows by customer_id = $1", () => {
    expect(routeSrc).toMatch(/WHERE\s+customer_id\s*=\s*\$1/);
  });

  it("SQL uses parameterised placeholders ($N) for all user inputs", () => {
    expect(routeSrc).toContain("$1");
    expect(routeSrc).toContain("$2");
    expect(routeSrc).toContain("$3");
    expect(routeSrc).toContain("$4");
    expect(routeSrc).toContain("$5");
    expect(routeSrc).toContain("$6");
    expect(routeSrc).toContain("$7");
  });

  it("SQL orders newest-first and uses LIMIT + OFFSET for pagination", () => {
    expect(routeSrc).toContain("ORDER BY created_at DESC");
    expect(routeSrc).toContain("OFFSET");
  });
});

// ---------------------------------------------------------------------------
// CSRF guard — route.ts drift-detection
// ---------------------------------------------------------------------------

describe("GET /api/errors — CSRF guard drift-detection", () => {
  it("route enforces X-Requested-With header before auth", () => {
    expect(routeSrc).toContain("X-Requested-With");
    expect(routeSrc).toContain("xmlhttprequest");
  });

  it("CSRF check returns 403 before auth() is called", () => {
    const csrfIdx = routeSrc.indexOf("X-Requested-With");
    const forbiddenIdx = routeSrc.indexOf('"Forbidden"', csrfIdx);
    const authIdx = routeSrc.indexOf("await auth()", csrfIdx);
    expect(forbiddenIdx).toBeGreaterThan(csrfIdx);
    expect(forbiddenIdx).toBeLessThan(authIdx);
  });

  it("500 error response omits errorId from body (header only)", () => {
    const errorBodyIdx = routeSrc.indexOf('"Internal server error"');
    expect(errorBodyIdx).toBeGreaterThanOrEqual(0);
    const stringifyIdx = routeSrc.lastIndexOf("JSON.stringify", errorBodyIdx + 500);
    const closingParen = routeSrc.indexOf(")", stringifyIdx);
    const bodyExpr = stringifyIdx >= 0 ? routeSrc.slice(stringifyIdx, closingParen + 1) : "";
    expect(bodyExpr).not.toContain("errorId");
  });
});
