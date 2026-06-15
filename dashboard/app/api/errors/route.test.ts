// Cross-customer isolation tests for GET /api/errors.
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

function makeRequest(): Request {
  return new Request("http://localhost/api/errors", {
    headers: { "X-Requested-With": "XMLHttpRequest" },
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
    expect(sql).toMatch(/WHERE\s+customer_id\s*=\s*\$1/i);
    expect(params[0]).toBe(CUSTOMER_1.id);
  });

  it("uses the correct customer_id when a different customer is authenticated", async () => {
    mockAuth.mockResolvedValue({ user: { email: "c2@example.com" } } as never);
    mockEnsureCustomer.mockResolvedValue(CUSTOMER_2 as never);
    await GET(makeRequest());
    const [sql, params] = mockQuery.mock.calls[0] as [string, unknown[]];
    expect(params[0]).toBe(CUSTOMER_2.id);
    expect(sql).not.toMatch(/\bUNION\b/i);
    expect((params as unknown[]).filter(p => p === CUSTOMER_1.id)).toHaveLength(0);
  });
});
