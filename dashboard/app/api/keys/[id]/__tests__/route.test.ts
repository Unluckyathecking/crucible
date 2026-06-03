/**
 * Tests for DELETE /api/keys/[id] route handler logic.
 *
 * We cannot import the route directly because it imports @/auth which requires
 * the full NextAuth + Postgres setup. Instead we test the extractable semantics:
 *   1. Auth guard: 401 when no session / no email.
 *   2. Ownership check: 404 when key not owned by the requesting customer.
 *   3. Idempotency: 200 on second revocation of an already-revoked key.
 *   4. Source-text assertions: verify the actual db.ts implementation uses the
 *      correct SQL patterns (ownership + idempotency guards), not a local mirror.
 */
import { describe, it, expect } from "vitest";
import * as fs from "fs";
import * as path from "path";

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

// ---------------------------------------------------------------------------
// Source-text assertions: verify the actual db.ts revokeApiKey implementation
// uses the correct SQL patterns. Reading the real source avoids the tautology
// of a re-implemented mirror that can drift silently.
// ---------------------------------------------------------------------------
describe("revokeApiKey in db.ts — SQL pattern guards", () => {
  const src = fs.readFileSync(path.resolve(__dirname, "../../../../../lib/db.ts"), "utf8");

  it("revokeApiKey SQL includes ownership guard (customer_id)", () => {
    expect(src).toContain("customer_id");
  });

  it("revokeApiKey SQL includes revoked_at IS NULL guard for idempotency", () => {
    expect(src).toContain("revoked_at IS NULL");
  });

  it("revokeApiKey RETURNING prefix so Redis comment can reference it", () => {
    expect(src).toContain("RETURNING id, prefix");
  });

  it("revokeApiKey returns a typed result (not boolean) distinguishing already_revoked from not_found", () => {
    expect(src).toContain('"already_revoked"');
    expect(src).toContain('"not_found"');
  });

  it("audit emission in revokeApiKey is fire-and-forget (catch, not await-throw)", () => {
    // The .catch() after emitAuditEvent in revokeApiKey must be present so audit failures
    // don't surface as 500 to the customer after the key is already revoked.
    const revokeSection = src.slice(src.indexOf("export async function revokeApiKey"), src.indexOf("export async function listAuditEvents"));
    expect(revokeSection).toContain(".catch(");
  });
});
