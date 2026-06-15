// Cross-customer isolation tests for GET /api/errors.
// These tests verify that every query is scoped to the authenticated customer's
// UUID via `WHERE customer_id = $1`, preventing one customer from ever reading
// another customer's error events or request payloads.
import { describe, it, expect, vi, beforeEach } from "vitest";

vi.mock("@/auth", () => ({ auth: vi.fn() }));
vi.mock("@/lib/db", () => ({
  pool: { query: vi.fn() },
  ensureCustomer: vi.fn(),
}));

const { GET } = await import("./route");
const { auth } = await import("@/auth");
const { pool, ensureCustomer } = await import("@/lib/db");

const mockAuth = vi.mocked(auth);
const mockEnsureCustomer = vi.mocked(ensureCustomer);
const mockQuery = vi.mocked(pool.query);

const CUSTOMER_1 = { id: "aaaaaaaa-0000-0000-0000-000000000001" };
const CUSTOMER_2 = { id: "bbbbbbbb-0000-0000-0000-000000000002" };

function makeRequest(customHeaders?: Record<string, string>): Request {
  return new Request("http://localhost/api/errors", {
    headers: { "X-Requested-With": "XMLHttpRequest", ...customHeaders },
  });
}

describe("GET /api/errors — cross-customer isolation", () => {
  beforeEach(() => {
    vi.resetAllMocks();
    mockAuth.mockResolvedValue({ user: { email: "c1@example.com" } } as never);
    mockEnsureCustomer.mockResolvedValue(CUSTOMER_1 as never);
    mockQuery.mockResolvedValue({ rows: [], rowCount: 0 } as never);
  });

  it("returns 401 for unauthenticated requests without touching the DB", async () => {
    mockAuth.mockResolvedValue(null);
    const res = await GET(makeRequest());
    expect(res.status).toBe(401);
    expect(mockQuery).not.toHaveBeenCalled();
  });

  it("always passes the authenticated customer_id as the first SQL parameter", async () => {
    await GET(makeRequest());
    expect(mockQuery).toHaveBeenCalledOnce();
    const [sql, params] = mockQuery.mock.calls[0] as [string, unknown[]];
    // customer_id = $1 is the row-level isolation predicate
    expect(sql).toMatch(/WHERE\s+customer_id\s*=\s*\$1/i);
    expect(params[0]).toBe(CUSTOMER_1.id);
  });

  it("uses a different customer_id when a different customer is authenticated", async () => {
    mockAuth.mockResolvedValue({ user: { email: "c2@example.com" } } as never);
    mockEnsureCustomer.mockResolvedValue(CUSTOMER_2 as never);

    await GET(makeRequest());
    const [sql, params] = mockQuery.mock.calls[0] as [string, unknown[]];
    expect(params[0]).toBe(CUSTOMER_2.id);
    // Verify the SQL has no UNION that could broaden scope beyond the
    // authenticated customer, and CUSTOMER_1.id never appears as any parameter.
    expect(sql).not.toMatch(/\bUNION\b/i);
    expect((params as unknown[]).filter(p => p === CUSTOMER_1.id)).toHaveLength(0);
  });

  it("never mixes rows: each request is scoped to exactly one customer_id", async () => {
    // Run two back-to-back requests as different customers and confirm the
    // SQL parameter is always the currently-authenticated customer's UUID —
    // never the other customer's UUID.
    await GET(makeRequest());
    const [, params1] = mockQuery.mock.calls[0] as [string, unknown[]];
    expect(params1[0]).toBe(CUSTOMER_1.id);

    vi.resetAllMocks();
    mockQuery.mockResolvedValue({ rows: [], rowCount: 0 } as never);
    mockAuth.mockResolvedValue({ user: { email: "c2@example.com" } } as never);
    mockEnsureCustomer.mockResolvedValue(CUSTOMER_2 as never);

    await GET(makeRequest());
    const [sql2, params2] = mockQuery.mock.calls[0] as [string, unknown[]];
    expect(params2[0]).toBe(CUSTOMER_2.id);
    expect(sql2).toMatch(/WHERE\s+customer_id\s*=\s*\$1/i);
    expect((params2 as unknown[]).filter(p => p === CUSTOMER_1.id)).toHaveLength(0);
  });

  it("request_payload rows are scoped to the authenticated customer: customer 2 cannot see customer 1 payloads", async () => {
    // Customer 1 has a row with request_payload populated.
    const customer1Row = {
      id: "row-c1-001",
      operation: "/v1/echo",
      error_code: "RATE_LIMITED",
      http_status: 429,
      message: "too many requests",
      request_id: "req-aaa",
      created_at: new Date("2024-01-15T12:00:00.000Z"),
      request_payload: Buffer.from("c1-secret-payload"),
    };
    mockQuery.mockResolvedValue({ rows: [customer1Row], rowCount: 1 } as never);
    const res1 = await GET(makeRequest());
    expect(res1.status).toBe(200);
    // The WHERE customer_id = $1 predicate with CUSTOMER_1.id isolates these rows.
    const [sql1, params1] = mockQuery.mock.calls[0] as [string, unknown[]];
    expect(sql1).toMatch(/WHERE\s+customer_id\s*=\s*\$1/i);
    expect(params1[0]).toBe(CUSTOMER_1.id);

    // Switching to customer 2: the WHERE clause uses CUSTOMER_2.id, so the
    // DB never returns rows belonging to CUSTOMER_1 regardless of their content.
    vi.resetAllMocks();
    mockQuery.mockResolvedValue({ rows: [], rowCount: 0 } as never);
    mockAuth.mockResolvedValue({ user: { email: "c2@example.com" } } as never);
    mockEnsureCustomer.mockResolvedValue(CUSTOMER_2 as never);

    const res2 = await GET(makeRequest());
    expect(res2.status).toBe(200);
    const [sql2, params2] = mockQuery.mock.calls[0] as [string, unknown[]];
    expect(sql2).toMatch(/WHERE\s+customer_id\s*=\s*\$1/i);
    expect(params2[0]).toBe(CUSTOMER_2.id);
    expect(params2[0]).not.toBe(CUSTOMER_1.id);
  });
});
