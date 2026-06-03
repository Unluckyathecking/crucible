import { Pool } from "pg";
import { emitAuditEvent } from "@/lib/audit";
import { getRedis } from "@/lib/redis";

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
  void emitAuditEvent(pool, {
    actorType: "customer",
    actorId: customerId,
    action: "api_key.created",
    targetType: "api_key",
    targetId: keyId,
    details: { name: name || null, prefix },
  }).catch((err) => {
    console.error("audit emit failed for api_key.created", { keyId, customerId, error: err instanceof Error ? err.message : String(err) });
  });
  return keyId;
}

// revokeApiKey sets revoked_at on a key that belongs to customerId.
// Returns "revoked" on success, "already_revoked" when the key was already inactive (idempotent),
// "forbidden" when the key exists but belongs to another customer (caller should 403),
// or "not_found" when the key doesn't exist at all.
//
// Redis note: the gateway caches valid keys at "auth:{prefix}" for up to 60 s. The dashboard
// has no Redis connection, so that entry is not explicitly cleared here. Revocation is immediate
// in Postgres (the gateway's source of truth). For instant cache invalidation run:
//   redis-cli DEL auth:{prefix}
// On the gateway side, Store.Revoke() handles this automatically (CLAUDE.md invariant #7).
export async function revokeApiKey(
  keyId: string,
  customerId: string,
): Promise<"revoked" | "already_revoked" | "not_found" | "forbidden"> {
  // Only update if the key is still active, so we can detect true revocation vs idempotent call.
  const r = await pool.query<{ id: string; prefix: string }>(
    `UPDATE api_keys SET revoked_at = NOW()
     WHERE id = $1 AND customer_id = $2 AND revoked_at IS NULL
     RETURNING id, prefix`,
    [keyId, customerId],
  );

  if (r.rows.length > 0) {
    const prefix = r.rows[0].prefix;

    // Best-effort Redis cache invalidation: the gateway caches auth:{prefix} for 60 s.
    // Clearing it here makes revocation effective immediately (CLAUDE.md invariant #7).
    // Fire-and-forget — a Redis failure must not fail the revocation that's already in Postgres.
    const redis = getRedis();
    if (redis) {
      void redis.del(`auth:${prefix}`).catch((err) => {
        console.error("redis cache invalidation failed for revoked key", { prefix, error: err instanceof Error ? err.message : String(err) });
      });
    }

    // Best-effort: same invariant as insertApiKey — revocation is already durable in Postgres;
    // an audit failure must not surface as a 500 to the customer.
    void emitAuditEvent(pool, {
      actorType: "customer",
      actorId: customerId,
      action: "api_key.revoked",
      targetType: "api_key",
      targetId: keyId,
      details: { prefix },
    }).catch((err) => {
      console.error("audit emit failed for api_key.revoked", { keyId, customerId, error: err instanceof Error ? err.message : String(err) });
    });
    return "revoked";
  }

  // Distinguish not-found, forbidden (wrong owner), and already-revoked (owned, idempotent).
  // Query without customer_id filter so we can tell ownership from existence.
  const check = await pool.query<{ customer_id: string }>(
    `SELECT customer_id FROM api_keys WHERE id = $1`,
    [keyId],
  );
  if (check.rows.length === 0) return "not_found";
  if (check.rows[0].customer_id !== customerId) return "forbidden";
  return "already_revoked";
}

// listAuditEvents returns the most recent audit events for a customer:
// events the customer performed (actor_id = customerId) AND events that targeted
// them by UUID (target_id = customerId, e.g. plan changes by admin/system).
// idx_audit_actor (0001) covers the actor branch; idx_audit_target_id (0005) covers the target branch.
export async function listAuditEvents(
  customerId: string,
  limit = 20,
): Promise<AuditEventRow[]> {
  const clampedLimit = Math.max(1, Math.min(limit, 100));
  const r = await pool.query<AuditEventRow>(
    `SELECT id, actor_type, actor_id, action, target_type, target_id, details, created_at
     FROM audit_log
     WHERE actor_id = $1
        OR target_id = $1
     ORDER BY created_at DESC
     LIMIT $2`,
    [customerId, clampedLimit],
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
