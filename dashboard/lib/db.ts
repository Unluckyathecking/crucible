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

// Must match gateway/internal/auth/store.go Redis key format: "auth:<prefix>".
// Both sides changing this independently would silently break cache invalidation.
const AUTH_CACHE_PREFIX = "auth:";

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
// CLAUDE.md invariant #7: revocation must invalidate the gateway's Redis hot-cache entry
// ("auth:{prefix}") so that the key stops working immediately rather than after the 60 s TTL.
// This function does that: after the Postgres UPDATE succeeds it fires a best-effort Redis DEL.
// If REDIS_URL is not configured in the dashboard's environment the DEL is skipped; the key
// remains valid in the gateway's cache until the TTL expires. Set REDIS_URL to share the same
// Redis instance as the gateway to get immediate invalidation.
export async function revokeApiKey(
  keyId: string,
  customerId: string,
): Promise<"revoked" | "already_revoked" | "not_found" | "forbidden"> {
  // Single atomic CTE: UPDATE + ownership classification in one round-trip.
  // The two-query pattern (UPDATE then SELECT) has a TOCTOU race where the key
  // could be deleted or transferred between the queries, causing "forbidden" to
  // incorrectly return "already_revoked". The CTE runs at a single snapshot.
  //
  // The `found` subquery reads api_keys at the pre-UPDATE snapshot (CTE data-modification
  // semantics in Postgres), so it sees the key's original state regardless of the UPDATE result.
  //
  // CLAUDE.md invariant #7: revocation must invalidate the gateway's Redis hot-cache entry
  // ("auth:{prefix}") so that the key stops working immediately rather than after the 60 s TTL.
  //
  // Scalar subqueries over the two CTEs are safe here: `updated` returns 0 or 1 row
  // (UPDATE on PK with WHERE), and `found` returns 0 or 1 row (SELECT on PK).
  // The four cases are mutually exclusive:
  //   1. 'revoked'        — UPDATE succeeded; updated has 1 row.
  //   2. 'not_found'      — key does not exist; found has 0 rows.
  //   3. 'forbidden'      — key exists but belongs to another customer.
  //   4. 'already_revoked'— key exists, owned by caller, but revoked_at was already set.
  const r = await pool.query<{ result: string; prefix: string | null; found_prefix: string | null }>(
    `WITH updated AS (
       UPDATE api_keys SET revoked_at = NOW()
       WHERE id = $1 AND customer_id = $2 AND revoked_at IS NULL
       RETURNING prefix
     ),
     found AS (
       SELECT customer_id, prefix FROM api_keys WHERE id = $1
     )
     SELECT
       CASE
         WHEN (SELECT prefix FROM updated) IS NOT NULL THEN 'revoked'
         WHEN NOT EXISTS (SELECT 1 FROM found) THEN 'not_found'
         WHEN (SELECT customer_id FROM found) != $2 THEN 'forbidden'
         ELSE 'already_revoked'
       END AS result,
       (SELECT prefix FROM updated) AS prefix,
       (SELECT prefix FROM found) AS found_prefix`,
    [keyId, customerId],
  );

  const { result: rawResult, prefix, found_prefix } = r.rows[0];

  // Validate at runtime so a SQL change that introduces a new CASE branch
  // is caught immediately rather than silently falling through to the caller.
  const VALID_RESULTS = ["revoked", "already_revoked", "not_found", "forbidden"] as const;
  type RevokeResult = (typeof VALID_RESULTS)[number];
  // Type guard lets TypeScript narrow rawResult without a cast.
  const isRevokeResult = (s: string): s is RevokeResult =>
    (VALID_RESULTS as readonly string[]).includes(s);
  if (!isRevokeResult(rawResult)) {
    throw new Error(`Unexpected revokeApiKey result: ${rawResult}`);
  }
  const result = rawResult; // narrowed to RevokeResult by the type guard above

  if (result === "revoked" && prefix) {
    // Best-effort Redis cache invalidation: the gateway caches auth:{prefix} for 60 s.
    // Clearing it here makes revocation effective immediately (CLAUDE.md invariant #7).
    // Fire-and-forget — a Redis failure must not fail the revocation that's already in Postgres.
    const redis = getRedis();
    if (redis) {
      void redis.del(`${AUTH_CACHE_PREFIX}${prefix}`).catch((err) => {
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

  if (result === "already_revoked" && found_prefix) {
    // The first revocation may have succeeded in Postgres but transiently failed Redis.
    // Attempt DEL again so a stale cache entry cannot extend the key's validity.
    const alreadyRedis = getRedis();
    if (alreadyRedis) {
      void alreadyRedis.del(`${AUTH_CACHE_PREFIX}${found_prefix}`).catch((err) => {
        console.error("redis cache invalidation failed for already_revoked key", { prefix: found_prefix, error: err instanceof Error ? err.message : String(err) });
      });
    }
    // Emit an audit event so every revocation attempt has an audit trail.
    void emitAuditEvent(pool, {
      actorType: "customer",
      actorId: customerId,
      action: "api_key.revoked",
      targetType: "api_key",
      targetId: keyId,
      details: { prefix: found_prefix },
    }).catch((err) => {
      console.error("audit emit failed for api_key.revoked (already_revoked)", { keyId, customerId, error: err instanceof Error ? err.message : String(err) });
    });
  }

  return result;
}

// listAuditEvents returns the most recent audit events for a customer:
// events the customer performed (actor_id = customerId) AND events that targeted
// them by UUID (target_id = customerId, e.g. plan changes by admin/system).
//
// UNION ALL gives the planner two separate index scans (idx_audit_actor_id for the
// actor branch, idx_audit_target_id for the target branch). Duplicates between the two
// branches are impossible: the WHERE conditions are mutually exclusive — a row satisfying
// `actor_id = $1` cannot satisfy `actor_id IS DISTINCT FROM $1` simultaneously. UNION ALL
// avoids the dedup sort/hash overhead of UNION without any correctness risk.
export async function listAuditEvents(
  customerId: string,
  limit = 20,
): Promise<AuditEventRow[]> {
  // Guard against NaN/Infinity: Math.max/min propagate NaN silently, which would
  // cause Postgres to receive NaN as the LIMIT parameter and return a query error.
  const MAX_AUDIT_LIMIT = 100;
  const safeLimit = Number.isFinite(limit) ? limit : 20;
  const clampedLimit = Math.max(1, Math.min(safeLimit, MAX_AUDIT_LIMIT));
  // Per-branch subquery limits let Postgres use idx_audit_actor_id and idx_audit_target_id
  // for efficient index scans. Without them, Postgres must materialize both branches before
  // the outer LIMIT applies. actor_id = $1 uses = (not IS NOT DISTINCT FROM) so the b-tree
  // index is used directly; customerId is always a non-null UUID.
  const r = await pool.query<AuditEventRow>(
    `SELECT id, actor_type, actor_id, action, target_type, target_id, details, created_at
     FROM (
       SELECT id, actor_type, actor_id, action, target_type, target_id, details, created_at
       FROM audit_log
       WHERE actor_id = $1
       ORDER BY created_at DESC
       LIMIT $2
     ) actor_events
     UNION ALL
     SELECT id, actor_type, actor_id, action, target_type, target_id, details, created_at
     FROM (
       SELECT id, actor_type, actor_id, action, target_type, target_id, details, created_at
       FROM audit_log
       WHERE target_id = $1
         AND actor_id IS DISTINCT FROM $1
       ORDER BY created_at DESC
       LIMIT $2
     ) target_events
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
