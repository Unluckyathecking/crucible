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
  const r = await pool.query<{ result: string; prefix: string | null }>(
    `WITH updated AS (
       UPDATE api_keys SET revoked_at = NOW()
       WHERE id = $1 AND customer_id = $2 AND revoked_at IS NULL
       RETURNING prefix
     ),
     found AS (
       SELECT customer_id FROM api_keys WHERE id = $1
     )
     SELECT
       CASE
         WHEN (SELECT prefix FROM updated) IS NOT NULL THEN 'revoked'
         WHEN NOT EXISTS (SELECT 1 FROM found) THEN 'not_found'
         WHEN (SELECT customer_id FROM found) != $2 THEN 'forbidden'
         ELSE 'already_revoked'
       END AS result,
       (SELECT prefix FROM updated) AS prefix`,
    [keyId, customerId],
  );

  const { result: rawResult, prefix } = r.rows[0];

  // Validate at runtime so a SQL change that introduces a new CASE branch
  // is caught immediately instead of silently falling through the cast.
  const VALID_RESULTS = ["revoked", "already_revoked", "not_found", "forbidden"] as const;
  type RevokeResult = (typeof VALID_RESULTS)[number];
  if (!VALID_RESULTS.includes(rawResult as RevokeResult)) {
    throw new Error(`Unexpected revokeApiKey result: ${rawResult}`);
  }
  const result = rawResult as RevokeResult;

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

  return result;
}

// listAuditEvents returns the most recent audit events for a customer:
// events the customer performed (actor_id = customerId) AND events that targeted
// them by UUID (target_id = customerId, e.g. plan changes by admin/system).
//
// UNION gives the planner two separate index scans (idx_audit_actor_id for the
// actor branch, idx_audit_target_id for the target branch) rather than a single
// BitmapOr scan over both indexes. The second branch uses IS DISTINCT FROM so that
// system events (actor_id IS NULL) targeting this customer are included without
// appearing in both branches. UNION (not UNION ALL) deduplicates by row identity;
// duplicates are impossible in practice (the two branches filter on different columns
// and the same event cannot have actor_id = customerId AND target_id = customerId
// simultaneously under current schema conventions), but UNION is a correctness guard
// against future event types that might set both fields to the customer UUID.
export async function listAuditEvents(
  customerId: string,
  limit = 20,
): Promise<AuditEventRow[]> {
  // Guard against NaN/Infinity: Math.max/min propagate NaN silently, which would
  // cause Postgres to receive NaN as the LIMIT parameter and return a query error.
  const safeLimit = Number.isFinite(limit) ? limit : 20;
  const clampedLimit = Math.max(1, Math.min(safeLimit, 100));
  // actor_id = $1 (not IS NOT DISTINCT FROM) so Postgres uses idx_audit_actor_id (b-tree).
  // customerId is always a non-null UUID so the two are semantically equivalent here,
  // but = allows the index seek while IS NOT DISTINCT FROM may force a seq scan.
  // A single outer LIMIT is correct: any event in the top N overall must be in the top N
  // of its branch, so per-branch LIMITs would be redundant and could confuse the planner.
  // UNION (not UNION ALL) deduplicates by row identity, so the planner merges the two
  // branch result sets before sorting. The second branch's IS DISTINCT FROM guard makes
  // duplicates impossible in practice today, but UNION adds a correctness guarantee if a
  // future event type sets both actor_id and target_id to the customer UUID.
  // The dedup overhead is negligible at the ≤100-row scale this function operates at.
  const r = await pool.query<AuditEventRow>(
    `SELECT id, actor_type, actor_id, action, target_type, target_id, details, created_at
     FROM audit_log
     WHERE actor_id = $1
     UNION
     SELECT id, actor_type, actor_id, action, target_type, target_id, details, created_at
     FROM audit_log
     WHERE target_id = $1
       AND actor_id IS DISTINCT FROM $1
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
