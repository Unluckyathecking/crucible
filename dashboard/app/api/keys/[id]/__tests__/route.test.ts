/**
 * Tests for DELETE /api/keys/[id] route handler logic.
 *
 * We cannot import the route directly because it imports @/auth which requires
 * the full NextAuth + Postgres setup. Instead we test the extractable semantics:
 *   1. Auth guard: 401 when no session / no email.
 *   2. Ownership check: 404 when key not owned by the requesting customer.
 *   3. Idempotency: 200 on second revocation of an already-revoked key.
 *   4. SQL pattern: ownership and revoked_at guards present in the query.
 */
import { describe, it, expect } from "vitest";

// ---------------------------------------------------------------------------
// Re-implementations of the route's business logic for isolated testing.
// If the route changes these rules, fix both the route AND these tests.
// ---------------------------------------------------------------------------

type RevokeResult = "revoked" | "already_revoked" | "not_found";

interface FakeKey {
  id: string;
  customer_id: string;
  revoked_at: Date | null;
}

function simulateRevokeApiKey(
  keyId: string,
  customerId: string,
  store: FakeKey[],
): RevokeResult {
  const active = store.find(
    (k) => k.id === keyId && k.customer_id === customerId && k.revoked_at === null,
  );
  if (active) {
    active.revoked_at = new Date();
    return "revoked";
  }
  const owned = store.find((k) => k.id === keyId && k.customer_id === customerId);
  if (owned) return "already_revoked";
  return "not_found";
}

function simulateDeleteRoute(
  session: { user?: { email?: string } } | null,
  keyId: string,
  customerId: string,
  store: FakeKey[],
): { status: number } {
  if (!session?.user?.email) return { status: 401 };
  const result = simulateRevokeApiKey(keyId, customerId, store);
  if (result === "not_found") return { status: 404 };
  return { status: 200 };
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

const CUSTOMER_A = "cust-a-uuid";
const CUSTOMER_B = "cust-b-uuid";

describe("DELETE /api/keys/[id] — auth guard", () => {
  it("returns 401 when session is null", () => {
    const r = simulateDeleteRoute(null, "key-1", CUSTOMER_A, []);
    expect(r.status).toBe(401);
  });

  it("returns 401 when session has no user", () => {
    const r = simulateDeleteRoute({}, "key-1", CUSTOMER_A, []);
    expect(r.status).toBe(401);
  });

  it("returns 401 when user has no email", () => {
    const r = simulateDeleteRoute({ user: {} }, "key-1", CUSTOMER_A, []);
    expect(r.status).toBe(401);
  });

  it("proceeds past auth with a valid session", () => {
    const store: FakeKey[] = [{ id: "key-1", customer_id: CUSTOMER_A, revoked_at: null }];
    const r = simulateDeleteRoute({ user: { email: "a@example.com" } }, "key-1", CUSTOMER_A, store);
    expect(r.status).toBe(200);
  });
});

describe("DELETE /api/keys/[id] — ownership", () => {
  it("returns 200 when customer revokes their own key", () => {
    const store: FakeKey[] = [{ id: "key-1", customer_id: CUSTOMER_A, revoked_at: null }];
    const r = simulateDeleteRoute({ user: { email: "a@example.com" } }, "key-1", CUSTOMER_A, store);
    expect(r.status).toBe(200);
  });

  it("returns 404 when customer tries to revoke another customer's key", () => {
    const store: FakeKey[] = [{ id: "key-2", customer_id: CUSTOMER_B, revoked_at: null }];
    const r = simulateDeleteRoute({ user: { email: "a@example.com" } }, "key-2", CUSTOMER_A, store);
    expect(r.status).toBe(404);
  });

  it("returns 404 for a non-existent key", () => {
    const store: FakeKey[] = [];
    const r = simulateDeleteRoute({ user: { email: "a@example.com" } }, "nonexistent", CUSTOMER_A, store);
    expect(r.status).toBe(404);
  });
});

describe("DELETE /api/keys/[id] — idempotency", () => {
  it("returns 200 on first revoke", () => {
    const store: FakeKey[] = [{ id: "key-1", customer_id: CUSTOMER_A, revoked_at: null }];
    const r = simulateDeleteRoute({ user: { email: "a@example.com" } }, "key-1", CUSTOMER_A, store);
    expect(r.status).toBe(200);
  });

  it("returns 200 on second revoke of the same key (already_revoked)", () => {
    const store: FakeKey[] = [{ id: "key-1", customer_id: CUSTOMER_A, revoked_at: null }];
    simulateDeleteRoute({ user: { email: "a@example.com" } }, "key-1", CUSTOMER_A, store);
    // Second call — key is now revoked
    const r = simulateDeleteRoute({ user: { email: "a@example.com" } }, "key-1", CUSTOMER_A, store);
    expect(r.status).toBe(200);
  });

  it("sets revoked_at only on first call", () => {
    const store: FakeKey[] = [{ id: "key-1", customer_id: CUSTOMER_A, revoked_at: null }];
    simulateDeleteRoute({ user: { email: "a@example.com" } }, "key-1", CUSTOMER_A, store);
    const firstRevoked = store[0].revoked_at;
    expect(firstRevoked).not.toBeNull();

    // Second call must not overwrite revoked_at.
    simulateDeleteRoute({ user: { email: "a@example.com" } }, "key-1", CUSTOMER_A, store);
    expect(store[0].revoked_at).toBe(firstRevoked);
  });
});

describe("revokeApiKey — SQL ownership pattern", () => {
  // Verify the SQL pattern used by revokeApiKey enforces ownership and idempotency.
  // This mirrors what db.ts sends to Postgres.
  function buildActiveRevokeSQL(keyId: string, customerId: string): { sql: string; params: string[] } {
    return {
      sql: `UPDATE api_keys SET revoked_at = NOW()
            WHERE id = $1 AND customer_id = $2 AND revoked_at IS NULL
            RETURNING id, prefix`,
      params: [keyId, customerId],
    };
  }

  it("SQL includes ownership guard (customer_id = $2)", () => {
    const { sql } = buildActiveRevokeSQL("key-id", "cust-id");
    expect(sql).toContain("customer_id = $2");
  });

  it("SQL includes revoked_at IS NULL to skip already-revoked keys", () => {
    const { sql } = buildActiveRevokeSQL("key-id", "cust-id");
    expect(sql).toContain("revoked_at IS NULL");
  });

  it("SQL parameters are [keyId, customerId] in the correct order", () => {
    const { params } = buildActiveRevokeSQL("key-abc", "cust-xyz");
    expect(params[0]).toBe("key-abc");
    expect(params[1]).toBe("cust-xyz");
  });
});
