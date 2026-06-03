import { Pool } from "pg";
import { emitAuditEvent } from "@/lib/audit";

declare global {
  // eslint-disable-next-line no-var
  var _crucible_pool: Pool | undefined;
}

const pool: Pool =
  global._crucible_pool ?? new Pool({ connectionString: process.env.DATABASE_URL });
if (process.env.NODE_ENV !== "production") global._crucible_pool = pool;

export interface Customer {
  id: string;
  email: string;
  plan_id: string;
}

export interface ApiKeyRow {
  id: string;
  prefix: string;
  name: string | null;
  last_used_at: Date | null;
}

export interface AuditEventRow {
  id: string;
  actor_type: string;
  actor_id: string | null;
  action: string;
  target_type: string | null;
  target_id: string | null;
  details: Record<string, unknown> | null;
  created_at: Date;
}

/**
 * Look up the customer by email, creating a free-tier row if missing.
 * Idempotent — safe to call on every dashboard page render.
 */
export async function ensureCustomer(email: string): Promise<Customer> {
  // Fast path: avoid ON CONFLICT write if customer already exists.
  let result = await pool.query<Customer>(
    `SELECT id, email, plan_id FROM customers WHERE email = $1`,
    [email],
  );

  if (result.rows.length > 0) {
    return result.rows[0];
  }

  // If the user doesn't exist, insert them. Use ON CONFLICT DO UPDATE as a safe fallback
  // against race conditions if two concurrent requests try to create the same user.
  // By using DO UPDATE SET email = EXCLUDED.email, we guarantee a row is returned via RETURNING
  // in a single round-trip, avoiding the race window of a DO NOTHING + secondary SELECT.
  // We only hit this write path if the first SELECT missed, keeping MVCC bloat ~0 on the hot path.
  result = await pool.query<Customer>(
    `INSERT INTO customers (email, plan_id) VALUES ($1, 'free')
     ON CONFLICT (email) DO UPDATE SET email = EXCLUDED.email
     RETURNING id, email, plan_id`,
    [email],
  );

  return result.rows[0];
}

export async function listKeys(customerId: string): Promise<ApiKeyRow[]> {
  const r = await pool.query<ApiKeyRow>(
    `SELECT id, prefix, name, last_used_at FROM api_keys
     WHERE customer_id = $1 AND revoked_at IS NULL
     ORDER BY created_at DESC`,
    [customerId],
  );
  return r.rows;
}

export async function insertApiKey(
  customerId: string,
  prefix: string,
  hash: Buffer,
  name: string,
): Promise<string> {
  const r = await pool.query<{ id: string }>(
    `INSERT INTO api_keys (customer_id, prefix, hash, name)
     VALUES ($1, $2, $3, $4) RETURNING id`,
    [customerId, prefix, hash, name],
  );
  const keyId = r.rows[0].id;
  // Best-effort: audit emit must not fail the key creation.
  // The key is already persisted; if audit fails the customer still gets their key.
  emitAuditEvent(pool, {
    actorType: "customer",
    actorId: customerId,
    action: "api_key.created",
    targetType: "api_key",
    targetId: keyId,
    details: { name: name || null, prefix },
  }).catch(() => {});
  return keyId;
}

// revokeApiKey sets revoked_at on a key that belongs to customerId.
// Returns "revoked" on success, "already_revoked" when the key was already inactive (idempotent),
// or "not_found" when the key doesn't exist or belongs to another customer.
//
// Gateway Redis hot cache: the gateway caches the key at "auth:{prefix}" with a 60 s TTL.
// That cache entry will expire naturally — revocation is immediately durable in Postgres
// (the gateway's source of truth) but may take up to 60 s to propagate through the cache.
// This matches the documented behaviour in CLAUDE.md invariant #7.
export async function revokeApiKey(
  keyId: string,
  customerId: string,
): Promise<"revoked" | "already_revoked" | "not_found"> {
  // Only update if the key is still active, so we can detect true revocation vs idempotent call.
  const r = await pool.query<{ id: string; prefix: string }>(
    `UPDATE api_keys SET revoked_at = NOW()
     WHERE id = $1 AND customer_id = $2 AND revoked_at IS NULL
     RETURNING id, prefix`,
    [keyId, customerId],
  );

  if (r.rows.length > 0) {
    await emitAuditEvent(pool, {
      actorType: "customer",
      actorId: customerId,
      action: "api_key.revoked",
      targetType: "api_key",
      targetId: keyId,
      details: { prefix: r.rows[0].prefix },
    });
    return "revoked";
  }

  // Distinguish already-revoked (owned, idempotent → 200) from not-found/not-owned (→ 404).
  const check = await pool.query<{ id: string }>(
    `SELECT id FROM api_keys WHERE id = $1 AND customer_id = $2`,
    [keyId, customerId],
  );
  return check.rows.length > 0 ? "already_revoked" : "not_found";
}

// listAuditEvents returns the most recent audit events attributed to customerId.
export async function listAuditEvents(
  customerId: string,
  limit = 20,
): Promise<AuditEventRow[]> {
  const r = await pool.query<AuditEventRow>(
    `SELECT id, actor_type, actor_id, action, target_type, target_id, details, created_at
     FROM audit_log
     WHERE actor_type = 'customer' AND actor_id = $1
     ORDER BY created_at DESC
     LIMIT $2`,
    [customerId, limit],
  );
  return r.rows;
}

export async function sumUsage(customerId: string, days: number): Promise<number> {
  const r = await pool.query<{ units: string }>(
    `SELECT COALESCE(SUM(billable_units), 0)::text AS units
     FROM usage_events
     WHERE customer_id = $1 AND created_at >= NOW() - INTERVAL '1 day' * $2`,
    [customerId, days],
  );
  return Number(r.rows[0].units);
}
