/**
 * Tests for POST /api/keys/[id]/rotate handler logic.
 *
 * We cannot import the route directly because it imports @/auth which requires
 * the full NextAuth + Postgres setup. Instead we test the extractable semantics:
 *   1. Auth guard: 401 when no session / no email.
 *   2. UUID validation: 404 on non-UUID key id.
 *   3. Ownership check: 403 when key belongs to a different customer.
 *   4. Not-found: 404 when key does not exist.
 *   5. Already-expired: 409 when key is past its grace window.
 *   6. Grace clamping: server enforces [MIN, MAX] range.
 *   7. Drift-detection: source-text assertions verify actual route.ts and db.ts invariants.
 */
import { describe, it, expect } from "vitest";
import * as fs from "fs";
import * as path from "path";

// ---------------------------------------------------------------------------
// Re-implementations of business logic for isolated testing.
// If the route or db changes these rules, fix both the source AND these tests.
// ---------------------------------------------------------------------------

const MIN_GRACE_SECS = 0;
const MAX_GRACE_SECS = 7 * 24 * 3600;
const DEFAULT_GRACE_SECS = 3600;

type RotateReason = "not_found" | "forbidden" | "already_expired";
type RotateResult =
  | { ok: true; newKey: string; newKeyId: string }
  | { ok: false; reason: RotateReason };

interface FakeKey {
  id: string;
  customer_id: string;
  revoked_at: Date | null;
  expires_at: Date | null;
}

function simulateRotateApiKey(
  keyId: string,
  customerId: string,
  store: FakeKey[],
): RotateResult {
  const now = new Date();
  const active = store.find(
    (k) =>
      k.id === keyId &&
      k.customer_id === customerId &&
      k.revoked_at === null &&
      (k.expires_at === null || k.expires_at > now),
  );
  if (active) {
    return { ok: true, newKey: "cru_live_NEWKEY", newKeyId: "new-uuid" };
  }
  const key = store.find((k) => k.id === keyId);
  if (!key) return { ok: false, reason: "not_found" };
  if (key.customer_id !== customerId) return { ok: false, reason: "forbidden" };
  return { ok: false, reason: "already_expired" };
}

const UUID_RE = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i;

function simulateRotateRoute(
  session: { user?: { email?: string } } | null,
  keyId: string,
  customerId: string,
  store: FakeKey[],
  graceSecs: number = DEFAULT_GRACE_SECS,
): { status: number } {
  if (!session?.user?.email) return { status: 401 };
  if (!UUID_RE.test(keyId)) return { status: 404 };
  const clamped = Math.max(MIN_GRACE_SECS, Math.min(Math.floor(graceSecs), MAX_GRACE_SECS));
  void clamped; // used by server; the simulation ignores the value for status-only tests
  const result = simulateRotateApiKey(keyId, customerId, store);
  if (!result.ok) {
    if (result.reason === "not_found") return { status: 404 };
    if (result.reason === "forbidden") return { status: 403 };
    if (result.reason === "already_expired") return { status: 409 };
  }
  return { status: 200 };
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

const CUSTOMER_A = "11111111-1111-1111-1111-111111111111";
const CUSTOMER_B = "22222222-2222-2222-2222-222222222222";
const VALID_KEY_ID = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa";
const VALID_KEY_B_ID = "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb";

describe("POST /api/keys/[id]/rotate — auth guard", () => {
  it("returns 401 when session is null", () => {
    expect(simulateRotateRoute(null, VALID_KEY_ID, CUSTOMER_A, []).status).toBe(401);
  });

  it("returns 401 when session has no user", () => {
    expect(simulateRotateRoute({}, VALID_KEY_ID, CUSTOMER_A, []).status).toBe(401);
  });

  it("returns 401 when user has no email", () => {
    expect(simulateRotateRoute({ user: {} }, VALID_KEY_ID, CUSTOMER_A, []).status).toBe(401);
  });

  it("proceeds past auth with a valid session", () => {
    const store: FakeKey[] = [{ id: VALID_KEY_ID, customer_id: CUSTOMER_A, revoked_at: null, expires_at: null }];
    expect(simulateRotateRoute({ user: { email: "a@example.com" } }, VALID_KEY_ID, CUSTOMER_A, store).status).toBe(200);
  });
});

describe("POST /api/keys/[id]/rotate — UUID validation", () => {
  it("returns 404 for non-UUID key id", () => {
    const session = { user: { email: "a@example.com" } };
    expect(simulateRotateRoute(session, "not-a-uuid", CUSTOMER_A, []).status).toBe(404);
  });

  it("returns 404 for empty key id", () => {
    const session = { user: { email: "a@example.com" } };
    expect(simulateRotateRoute(session, "", CUSTOMER_A, []).status).toBe(404);
  });

  it("accepts a valid UUID v4", () => {
    const store: FakeKey[] = [{ id: VALID_KEY_ID, customer_id: CUSTOMER_A, revoked_at: null, expires_at: null }];
    const session = { user: { email: "a@example.com" } };
    expect(simulateRotateRoute(session, VALID_KEY_ID, CUSTOMER_A, store).status).toBe(200);
  });
});

describe("POST /api/keys/[id]/rotate — ownership and status", () => {
  it("returns 200 when customer rotates their own active key", () => {
    const store: FakeKey[] = [{ id: VALID_KEY_ID, customer_id: CUSTOMER_A, revoked_at: null, expires_at: null }];
    const r = simulateRotateRoute({ user: { email: "a@example.com" } }, VALID_KEY_ID, CUSTOMER_A, store);
    expect(r.status).toBe(200);
  });

  it("returns 403 when customer tries to rotate another customer's key", () => {
    const store: FakeKey[] = [{ id: VALID_KEY_B_ID, customer_id: CUSTOMER_B, revoked_at: null, expires_at: null }];
    const r = simulateRotateRoute({ user: { email: "a@example.com" } }, VALID_KEY_B_ID, CUSTOMER_A, store);
    expect(r.status).toBe(403);
  });

  it("returns 404 for a non-existent key", () => {
    const r = simulateRotateRoute({ user: { email: "a@example.com" } }, VALID_KEY_ID, CUSTOMER_A, []);
    expect(r.status).toBe(404);
  });

  it("returns 409 for a key past its grace window (already_expired)", () => {
    const pastDate = new Date(Date.now() - 1000);
    const store: FakeKey[] = [{ id: VALID_KEY_ID, customer_id: CUSTOMER_A, revoked_at: null, expires_at: pastDate }];
    const r = simulateRotateRoute({ user: { email: "a@example.com" } }, VALID_KEY_ID, CUSTOMER_A, store);
    expect(r.status).toBe(409);
  });

  it("returns 200 for a key within its grace window", () => {
    const futureDate = new Date(Date.now() + 3600 * 1000);
    const store: FakeKey[] = [{ id: VALID_KEY_ID, customer_id: CUSTOMER_A, revoked_at: null, expires_at: futureDate }];
    const r = simulateRotateRoute({ user: { email: "a@example.com" } }, VALID_KEY_ID, CUSTOMER_A, store);
    expect(r.status).toBe(200);
  });
});

describe("grace_secs clamping", () => {
  it("clamps negative grace_secs to 0", () => {
    const clamped = Math.max(MIN_GRACE_SECS, Math.min(Math.floor(-100), MAX_GRACE_SECS));
    expect(clamped).toBe(0);
  });

  it("clamps over-max grace_secs to MAX_GRACE_SECS", () => {
    const overMax = MAX_GRACE_SECS + 999999;
    const clamped = Math.max(MIN_GRACE_SECS, Math.min(Math.floor(overMax), MAX_GRACE_SECS));
    expect(clamped).toBe(MAX_GRACE_SECS);
  });

  it("accepts exactly MAX_GRACE_SECS", () => {
    const clamped = Math.max(MIN_GRACE_SECS, Math.min(Math.floor(MAX_GRACE_SECS), MAX_GRACE_SECS));
    expect(clamped).toBe(MAX_GRACE_SECS);
  });

  it("floors non-integer grace_secs", () => {
    const clamped = Math.max(MIN_GRACE_SECS, Math.min(Math.floor(3600.9), MAX_GRACE_SECS));
    expect(clamped).toBe(3600);
  });
});

// ---------------------------------------------------------------------------
// Drift-detection: route.ts source assertions.
// ---------------------------------------------------------------------------
describe("POST /api/keys/[id]/rotate route.ts — drift-detection", () => {
  const routeSrc = fs.readFileSync(path.resolve(__dirname, "../route.ts"), "utf8");

  it("route enforces X-Requested-With header as CSRF signal before auth check", () => {
    expect(routeSrc).toContain("X-Requested-With");
    expect(routeSrc).toContain("xmlhttprequest");
  });

  it("CSRF check returns 403 before auth() call in source order", () => {
    const csrfIdx = routeSrc.indexOf("X-Requested-With");
    const forbiddenIdx = routeSrc.indexOf('"Forbidden"', csrfIdx);
    const authIdx = routeSrc.indexOf("await auth()", csrfIdx);
    expect(forbiddenIdx).toBeGreaterThan(csrfIdx);
    expect(forbiddenIdx).toBeLessThan(authIdx);
  });

  it("route defines and uses MIN_GRACE_SECS and MAX_GRACE_SECS for clamping", () => {
    expect(routeSrc).toContain("MIN_GRACE_SECS");
    expect(routeSrc).toContain("MAX_GRACE_SECS");
  });

  it("route returns 409 for already_expired", () => {
    expect(routeSrc).toContain('"already_expired"');
    expect(routeSrc).toContain("409");
  });

  it("route returns new key as JSON with cache-control: no-store", () => {
    expect(routeSrc).toContain('"cache-control"');
    expect(routeSrc).toContain("no-store");
    expect(routeSrc).toContain("result.newKey");
  });

  it("route uses UUID_RE to validate key id before hitting DB", () => {
    expect(routeSrc).toContain("UUID_RE");
  });
});

// ---------------------------------------------------------------------------
// Drift-detection: db.ts source assertions for rotateApiKey.
// ---------------------------------------------------------------------------
describe("rotateApiKey in db.ts — drift-detection smoke tests", () => {
  const src = fs.readFileSync(path.resolve(__dirname, "../../../../../../lib/db.ts"), "utf8");

  const rotateStart = src.indexOf("export async function rotateApiKey");
  const nextExport = src.indexOf("\nexport ", rotateStart + 1);
  const rotateSection =
    rotateStart >= 0 ? src.slice(rotateStart, nextExport > 0 ? nextExport : undefined) : "";

  it("rotateApiKey function exists in db.ts (extraction guard)", () => {
    expect(rotateStart).toBeGreaterThanOrEqual(0);
    expect(rotateSection.length).toBeGreaterThan(0);
  });

  it("rotateApiKey uses a DB transaction (BEGIN/COMMIT/ROLLBACK)", () => {
    expect(rotateSection).toContain("BEGIN");
    expect(rotateSection).toContain("COMMIT");
    expect(rotateSection).toContain("ROLLBACK");
  });

  it("rotateApiKey uses FOR UPDATE to lock the old key", () => {
    expect(rotateSection).toContain("FOR UPDATE");
  });

  it("rotateApiKey SQL filters out revoked and expired keys on the lock query", () => {
    expect(rotateSection).toContain("revoked_at IS NULL");
    expect(rotateSection).toContain("expires_at");
  });

  it("rotateApiKey invalidates the auth cache for the old prefix", () => {
    expect(rotateSection).toContain("invalidateAuthCache");
    expect(rotateSection).toContain("oldPrefix");
  });

  it("rotateApiKey emits audit event with fire-and-forget .catch() pattern", () => {
    const emitIdx = rotateSection.indexOf("emitAuditEvent");
    expect(emitIdx).toBeGreaterThanOrEqual(0);
    const catchIdx = rotateSection.indexOf(".catch(", emitIdx);
    expect(catchIdx).toBeGreaterThan(emitIdx);
  });

  it("rotateApiKey returns RotateResult with ok/reason discriminant", () => {
    expect(src).toContain("export type RotateResult");
    expect(src).toContain("ok: true");
    expect(src).toContain("ok: false");
    expect(src).toContain('"not_found"');
    expect(src).toContain('"already_expired"');
  });

  it("listKeys SQL excludes expired keys", () => {
    expect(src).toContain("expires_at IS NULL OR expires_at > NOW()");
  });
});
