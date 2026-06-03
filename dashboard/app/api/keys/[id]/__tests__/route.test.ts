/**
 * Tests for DELETE /api/keys/[id] route handler logic.
 *
 * We cannot import the route directly because it imports @/auth which requires
 * the full NextAuth + Postgres setup. Instead we test the extractable semantics:
 *   1. Auth guard: 401 when no session / no email.
 *   2. Ownership check: 403 when key belongs to a different customer.
 *   3. Not-found: 404 when key does not exist.
 *   4. Idempotency: 200 on second revocation of an already-revoked key.
 *   5. Source-text assertions: verify the actual db.ts implementation uses the
 *      correct SQL patterns (ownership + idempotency guards), not a local mirror.
 *
 * Note: the CSRF guard (X-Requested-With: XMLHttpRequest check) is enforced in
 * route.ts before auth. simulateDeleteRoute tests DB-level semantics only; the
 * header check is covered by the drift-detection smoke test at the bottom of this file.
 */
import { describe, it, expect } from "vitest";
import * as fs from "fs";
import * as path from "path";

// ---------------------------------------------------------------------------
// Re-implementations of the route's business logic for isolated testing.
// If the route changes these rules, fix both the route AND these tests.
// ---------------------------------------------------------------------------

type RevokeResult = "revoked" | "already_revoked" | "not_found" | "forbidden";

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
  // No active match — check existence and ownership separately.
  const key = store.find((k) => k.id === keyId);
  if (!key) return "not_found";
  if (key.customer_id !== customerId) return "forbidden";
  return "already_revoked";
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
  if (result === "forbidden") return { status: 403 };
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

  it("returns 403 when customer tries to revoke another customer's key", () => {
    const store: FakeKey[] = [{ id: "key-2", customer_id: CUSTOMER_B, revoked_at: null }];
    const r = simulateDeleteRoute({ user: { email: "a@example.com" } }, "key-2", CUSTOMER_A, store);
    expect(r.status).toBe(403);
  });

  it("returns 403 even when the other customer's key is already revoked", () => {
    const store: FakeKey[] = [{ id: "key-2", customer_id: CUSTOMER_B, revoked_at: new Date() }];
    const r = simulateDeleteRoute({ user: { email: "a@example.com" } }, "key-2", CUSTOMER_A, store);
    expect(r.status).toBe(403);
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
// Drift-detection smoke tests — NOT behavioral guarantees.
// These read the actual db.ts source and assert that specific SQL patterns are
// present so that the re-implementation above cannot silently diverge from the
// real code without a test failure. They do not execute SQL or call the real
// function; they are a lint-like guard against structural drift.
// ---------------------------------------------------------------------------
describe("revokeApiKey in db.ts — drift-detection smoke tests", () => {
  const src = fs.readFileSync(path.resolve(__dirname, "../../../../../lib/db.ts"), "utf8");

  // Use indexOf for extraction rather than `^}` regex so that nested objects or
  // callbacks whose closing brace lands at column 0 don't truncate the match early.
  const revokeStart = src.indexOf("export async function revokeApiKey");
  const nextExport = src.indexOf("\nexport ", revokeStart + 1);
  const revokeSection =
    revokeStart >= 0 ? src.slice(revokeStart, nextExport > 0 ? nextExport : undefined) : "";

  it("revokeApiKey SQL includes ownership guard (customer_id)", () => {
    // Checks revokeSection (not full src) so a missing guard in revokeApiKey
    // is not masked by customer_id references in other functions.
    expect(revokeSection).toContain("customer_id");
  });

  it("revokeApiKey SQL includes revoked_at IS NULL guard for idempotency", () => {
    expect(revokeSection).toContain("revoked_at IS NULL");
  });

  it("revokeApiKey RETURNING prefix so Redis cache invalidation can reference it", () => {
    expect(revokeSection).toContain("RETURNING prefix");
  });

  it("revokeApiKey returns a typed result distinguishing already_revoked, not_found, and forbidden", () => {
    expect(revokeSection).toContain('"already_revoked"');
    expect(revokeSection).toContain('"not_found"');
    expect(revokeSection).toContain('"forbidden"');
  });

  it("audit emission in revokeApiKey is fire-and-forget (void, errors handled inside emitAuditEvent)", () => {
    // emitAuditEvent internally catches errors; callers mark intent with void.
    // Audits failures must not surface as 500 to the customer.
    expect(revokeSection).toMatch(/void\s+emitAuditEvent\s*\(/);
  });

  it("second query in revokeApiKey does not filter by customer_id so ownership vs non-existence is distinguishable", () => {
    // If the lookup adds AND customer_id = $2, it cannot tell whether the key
    // doesn't exist or belongs to a different customer — so the WHERE must be id-only.
    const normalizedSection = revokeSection.replace(/\s+/g, " ");
    // Second query selects customer_id so the caller can distinguish ownership.
    // Using dotAll /s flag + lazy quantifiers so innocent reformatting doesn't break the regex.
    expect(normalizedSection).toMatch(/SELECT\s+[^;]*?customer_id[^;]*?FROM\s+api_keys\s+WHERE\s+id\s*=\s*\$1/s);
    // Must NOT add a customer_id filter in the WHERE clause.
    expect(normalizedSection).not.toMatch(/FROM api_keys WHERE id = \$1 AND customer_id/);
  });
});

// ---------------------------------------------------------------------------
// Drift-detection smoke tests for route.ts — CSRF security control.
// These assert the actual route source contains the guard so that removing it
// fails a test rather than silently regressing. They do not execute the route.
// ---------------------------------------------------------------------------
describe("DELETE /api/keys/[id] route.ts — CSRF guard drift-detection", () => {
  const routeSrc = fs.readFileSync(path.resolve(__dirname, "../route.ts"), "utf8");

  it("route enforces X-Requested-With header as CSRF signal before auth check", () => {
    // The header check must exist in the route source.
    expect(routeSrc).toContain("X-Requested-With");
    // Route uses case-insensitive comparison via .toLowerCase() against "xmlhttprequest".
    expect(routeSrc).toContain("xmlhttprequest");
  });

  it("CSRF check returns 403 before reaching auth logic", () => {
    // The 403 Forbidden response must appear before the auth() call in source order.
    const csrfIdx = routeSrc.indexOf("X-Requested-With");
    const forbiddenIdx = routeSrc.indexOf('"Forbidden"', csrfIdx);
    const authIdx = routeSrc.indexOf("await auth()", csrfIdx);
    expect(forbiddenIdx).toBeGreaterThan(csrfIdx);
    expect(forbiddenIdx).toBeLessThan(authIdx);
  });
});
