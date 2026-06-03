import { Pool } from "pg";
import { emitAuditEvent } from "@/lib/audit";
import { getRedis } from "@/lib/redis";
import { UUID_RE } from "@/lib/validation";

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
const MAX_AUDIT_LIMIT = 100;

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
  // Best-effort: errors are caught and logged inside emitAuditEvent; never propagate here.
  void emitAuditEvent(pool, {
    actorType: "customer",
    actorId: customerId,
    action: "api_key.created",
    targetType: "api_key",
    targetId: keyId,
    details: { name, prefix },
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
  // Phase 1: attempt the revocation. If the key exists, belongs to this customer,
  // and is not yet revoked, the UPDATE succeeds and RETURNING gives us the prefix.
  const updateResult = await pool.query<{ prefix: string }>(
    `UPDATE api_keys SET revoked_at = NOW()
     WHERE id = $1 AND customer_id = $2 AND revoked_at IS NULL
     RETURNING prefix`,
    [keyId, customerId],
  );

  if (updateResult.rows.length > 0) {
    const { prefix } = updateResult.rows[0];
    // Best-effort Redis cache invalidation: the gateway caches auth:{prefix} for 60 s.
    // Clearing it here makes revocation effective immediately (CLAUDE.md invariant #7).
    // Fire-and-forget — a Redis failure must not fail the revocation already in Postgres.
    const redis = getRedis();
    if (redis) {
      void redis.del(`${AUTH_CACHE_PREFIX}${prefix}`).catch((err) => {
        console.error("redis cache invalidation failed for revoked key", { prefix, error: err instanceof Error ? err.message : String(err) });
      });
    }
    // Best-effort: errors are caught and logged inside emitAuditEvent; never propagate here.
    void emitAuditEvent(pool, {
      actorType: "customer",
      actorId: customerId,
      action: "api_key.revoked",
      targetType: "api_key",
      targetId: keyId,
      details: { prefix },
    });
    return "revoked";
  }

  // Phase 2: UPDATE touched 0 rows — distinguish not_found / forbidden / already_revoked.
  // A separate query uses a fresh snapshot; concurrent deletions are correctly visible.
  // Does not filter by customer_id so ownership vs non-existence is distinguishable.
  const lookupResult = await pool.query<{ customer_id: string; prefix: string | null }>(
    `SELECT customer_id, prefix FROM api_keys WHERE id = $1`,
    [keyId],
  );

  if (lookupResult.rows.length === 0) {
    return "not_found";
  }

  const { customer_id: foundCustomerId, prefix: foundPrefix } = lookupResult.rows[0];
  if (foundCustomerId !== customerId) {
    return "forbidden";
  }

  // Row exists, owned by caller, but revoked_at IS NOT NULL — idempotent re-revocation.
  if (foundPrefix) {
    // The first revocation may have succeeded in Postgres but transiently failed Redis.
    // Attempt DEL again so a stale cache entry cannot extend the key's validity.
    const alreadyRedis = getRedis();
    if (alreadyRedis) {
      void alreadyRedis.del(`${AUTH_CACHE_PREFIX}${foundPrefix}`).catch((err) => {
        console.error("redis cache invalidation failed for already_revoked key", { prefix: foundPrefix, error: err instanceof Error ? err.message : String(err) });
      });
    }
    // Best-effort: errors are caught and logged inside emitAuditEvent; never propagate here.
    void emitAuditEvent(pool, {
      actorType: "customer",
      actorId: customerId,
      action: "api_key.revoked",
      targetType: "api_key",
      targetId: keyId,
      details: { prefix: foundPrefix, idempotent: true },
    });
  }
  return "already_revoked";
}

// listAuditEvents returns the most recent audit events for a customer:
// events the customer performed (actor_id = customerId) AND events that targeted
// them by UUID (target_id = customerId, e.g. plan changes by admin/system).
//
// UNION ALL gives the planner two separate index scans (idx_audit_actor_id for the
// actor branch, idx_audit_target_id for the target branch). No row can appear in both
// branches: a single actor_id value cannot both equal customerId (actor branch) and be
// DISTINCT from customerId (target branch) at the same time. UNION ALL avoids the
// dedup sort/hash overhead of UNION without any correctness risk.
export async function listAuditEvents(
  customerId: string,
  limit = 20,
): Promise<AuditEventRow[]> {
  // customerId comes from ensureCustomer (trusted DB value), but we validate anyway
  // as defense-in-depth using the shared UUID_RE from @/lib/validation.
  if (!customerId || !UUID_RE.test(customerId)) {
    return [];
  }
  // Guard against non-numbers and NaN/Infinity: Math.max/min propagate NaN silently,
  // which would cause Postgres to receive NaN as the LIMIT parameter and return a query error.
  const safeLimit = typeof limit === "number" && Number.isFinite(limit) ? limit : 20;
  const clampedLimit = Math.max(1, Math.min(safeLimit, MAX_AUDIT_LIMIT));
  // In PostgreSQL 12+, single-reference CTEs are inlined by default (NOT MATERIALIZED),
  // so the planner treats them identically to subqueries and can push the outer LIMIT
  // down through the Append node into each index scan. actor_id = $1 uses = (not IS NOT
  // DISTINCT FROM) so the b-tree index is used directly; customerId is always non-null.
  // No per-branch LIMIT: a single LIMIT on the final UNION ALL is both correct and
  // avoids the subtle over-restriction that per-branch limits introduce when one branch
  // is sparse and the other is dense.
  const r = await pool.query<AuditEventRow>(
    `WITH actor_events AS (
       SELECT id, actor_type, actor_id, action, target_type, target_id, details, created_at
       FROM audit_log
       WHERE actor_id = $1
         AND created_at >= NOW() - INTERVAL '90 days'
     ),
     target_events AS (
       SELECT id, actor_type, actor_id, action, target_type, target_id, details, created_at
       FROM audit_log
       WHERE target_id = $1
         AND actor_id IS DISTINCT FROM $1 -- null-safe <>: includes system events (NULL actor_id)
         AND created_at >= NOW() - INTERVAL '90 days'
     )
     SELECT id, actor_type, actor_id, action, target_type, target_id, details, created_at
     FROM actor_events
     UNION ALL
     SELECT id, actor_type, actor_id, action, target_type, target_id, details, created_at
     FROM target_events
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
