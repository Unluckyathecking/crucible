// Isolation invariant: the GET /api/errors handler must pass the authenticated
// customer's own ID as the first SQL parameter ($1). Any row where
// customer_id ≠ $1 is excluded by the WHERE clause before the application sees
// it; no application-level filter is needed and none is applied.
//
// These tests mock the auth module and the DB pool so they run without a live
// database. They verify the API contract — that pool.query is always called
// with the session customer's ID as the first positional parameter — not the
// database's enforcement of that contract (which is a DB-level guarantee).
import { describe, it, expect, vi, beforeEach } from "vitest";

// Mocks must be hoisted before any import that transitively imports these modules.
vi.mock("@/auth", () => ({
  auth: vi.fn(),
}));
vi.mock("@/lib/db", () => ({
  ensureCustomer: vi.fn(),
  pool: { query: vi.fn() },
}));

import { GET } from "./route";
import { auth } from "@/auth";
import { ensureCustomer, pool } from "@/lib/db";

const ALICE_CUSTOMER_ID = "aaaaaaaa-0000-0000-0000-000000000001";
const BOB_CUSTOMER_ID   = "bbbbbbbb-0000-0000-0000-000000000002";

function makeRequest(params: Record<string, string> = {}): Request {
  const url = new URL("http://localhost/api/errors");
  url.searchParams.set("from", params.from ?? "2026-04-01");
  url.searchParams.set("to",   params.to   ?? "2026-06-15");
  for (const [k, v] of Object.entries(params)) {
    if (k !== "from" && k !== "to") url.searchParams.set(k, v);
  }
  return new Request(url.toString(), {
    headers: { "X-Requested-With": "XMLHttpRequest" },
  });
}

describe("GET /api/errors – cross-customer isolation invariant", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    // Default: Alice is authenticated and ensureCustomer resolves her ID.
    vi.mocked(auth).mockResolvedValue({ user: { email: "alice@example.com" } } as never);
    vi.mocked(ensureCustomer).mockResolvedValue({ id: ALICE_CUSTOMER_ID } as never);
    vi.mocked(pool.query).mockResolvedValue({ rows: [] } as never);
  });

  it("passes the authenticated customer ID as the first SQL parameter ($1)", async () => {
    const res = await GET(makeRequest());

    expect(res.status).toBe(200);
    expect(vi.mocked(pool.query)).toHaveBeenCalledOnce();

    const [, params] = vi.mocked(pool.query).mock.calls[0] as [string, unknown[]];
    // $1 in the WHERE clause must be the session customer's own UUID.
    // This is the primary row-level isolation guard.
    expect(params[0]).toBe(ALICE_CUSTOMER_ID);
  });

  it("uses a different customer ID for a different authenticated user", async () => {
    vi.mocked(auth).mockResolvedValue({ user: { email: "bob@example.com" } } as never);
    vi.mocked(ensureCustomer).mockResolvedValue({ id: BOB_CUSTOMER_ID } as never);

    await GET(makeRequest());

    const [, params] = vi.mocked(pool.query).mock.calls[0] as [string, unknown[]];
    // Bob's session must produce Bob's ID in $1, never Alice's.
    expect(params[0]).toBe(BOB_CUSTOMER_ID);
    expect(params[0]).not.toBe(ALICE_CUSTOMER_ID);
  });

  it("does not call pool.query when the session has no email (unauthenticated)", async () => {
    vi.mocked(auth).mockResolvedValue(null as never);

    const res = await GET(makeRequest());

    expect(res.status).toBe(401);
    // No DB query must be issued before authentication is confirmed.
    expect(vi.mocked(pool.query)).not.toHaveBeenCalled();
  });

  it("does not call pool.query when the X-Requested-With header is absent (CSRF guard)", async () => {
    const req = new Request("http://localhost/api/errors?from=2026-01-01&to=2026-06-15");

    const res = await GET(req);

    expect(res.status).toBe(403);
    expect(vi.mocked(pool.query)).not.toHaveBeenCalled();
  });
});
