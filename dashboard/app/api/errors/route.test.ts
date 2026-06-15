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
    const [, params] = mockQuery.mock.calls[0] as [string, unknown[]];
    expect(params[0]).toBe(CUSTOMER_2.id);
    expect(params[0]).not.toBe(CUSTOMER_1.id);
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
    const [, params2] = mockQuery.mock.calls[0] as [string, unknown[]];
    expect(params2[0]).toBe(CUSTOMER_2.id);
    expect(params2[0]).not.toBe(params1[0]);
  });
});
