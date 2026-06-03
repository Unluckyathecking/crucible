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
const AUDIT_LOOKBACK_DAYS = 90;
const MAX_USAGE_EVENTS_LIMIT = 1000;
// Separate cap for per-operation aggregate rows (distinct operations per customer window).
const MAX_USAGE_OPERATIONS_LIMIT = 1000;
export const MAX_OPERATION_LENGTH = 128;
export const MAX_USAGE_RANGE_DAYS = 90;
const MAX_USAGE_RANGE_MS = MAX_USAGE_RANGE_DAYS * 24 * 60 * 60 * 1000;

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

// invalidateAuthCache fires a best-effort Redis DEL for auth:{prefix}.
// DEL is idempotent: deleting an absent key returns 0, so calling this when the
// cache entry no longer exists is safe. Fire-and-forget — caller must not await.
function invalidateAuthCache(prefix: string): void {
  const redis = getRedis();
  if (!redis) return;
  void redis.del(`${AUTH_CACHE_PREFIX}${prefix}`).catch((err) => {
    console.error("redis cache invalidation failed", { prefix, error: err instanceof Error ? err.message : String(err) });
  });
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
    // Best-effort Redis cache invalidation (CLAUDE.md invariant #7): minimises the stale-cache
    // window after Postgres commits. Not atomic with the UPDATE — a transient Redis failure
    // leaves the key cached until the 60 s TTL. Fire-and-forget by design.
    invalidateAuthCache(prefix);
    // Audit is intentionally fire-and-forget and outside the UPDATE: audit failures must
    // never roll back a completed Postgres revocation. Including the audit INSERT in the
    // same transaction would make audit errors silently undo the revocation — the opposite
    // of what we want. Best-effort: errors caught and logged inside emitAuditEvent.
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
  // Truthiness excludes both null and empty-string prefixes (schema guarantees non-empty, but belt-and-suspenders).
  // Retry DEL in case the original revocation committed to Postgres but its Redis DEL failed transiently.
  if (foundPrefix) {
    invalidateAuthCache(foundPrefix);
  }
  return "already_revoked";
}

// listAuditEvents returns the most recent audit events for a customer:
// events the customer performed (actor_id = customerId) AND events that targeted
// them by UUID (target_id = customerId, e.g. plan changes by admin/system).
//
// Parenthesized subqueries give the planner two separate index scans
// (idx_audit_actor_id for the actor branch, idx_audit_target_id for the target branch).
// No row can appear in both branches: actor_id = $1 in the first branch excludes
// all those rows from the second. UNION ALL avoids the dedup overhead of UNION.
export async function listAuditEvents(
  customerId: string,
  limit = 20,
): Promise<AuditEventRow[]> {
  // customerId is always a UUID produced by ensureCustomer (PostgreSQL gen_random_uuid()).
  // UUID_RE validates the *parameter*, not the database column — defense-in-depth to short-circuit
  // if the caller ever passes a non-UUID (e.g. an email address) before issuing the query.
  if (!UUID_RE.test(customerId)) {
    return [];
  }
  // Guard against non-numbers and NaN/Infinity: Math.max/min propagate NaN silently,
  // which would cause Postgres to receive NaN as the LIMIT parameter and return a query error.
  const safeLimit = typeof limit === "number" && Number.isFinite(limit) ? limit : 20;
  const clampedLimit = Math.max(1, Math.min(safeLimit, MAX_AUDIT_LIMIT));
  // Parameterizing the cutoff (vs. inline INTERVAL) lets the planner use idx_audit_actor_id
  // and idx_audit_target_id with a stable bound rather than re-evaluating NOW() per-plan.
  const cutoff = new Date(Date.now() - AUDIT_LOOKBACK_DAYS * 24 * 60 * 60 * 1000);
  // Per-branch ORDER BY + LIMIT allows each branch to use its index with an index scan + limit,
  // then the outer sort merges at most 2*clampedLimit rows instead of the full table.
  const r = await pool.query<AuditEventRow>(
    `SELECT id, actor_type, actor_id, action, target_type, target_id, details, created_at
     FROM (
       (SELECT id, actor_type, actor_id, action, target_type, target_id, details, created_at
        FROM audit_log
        WHERE actor_id = $1
          AND created_at >= $3
        ORDER BY created_at DESC
        LIMIT $2)
       UNION ALL
       (SELECT id, actor_type, actor_id, action, target_type, target_id, details, created_at
        FROM audit_log
        WHERE target_id = $1
          AND (actor_id IS NULL OR actor_id != $1)
          AND created_at >= $3
        ORDER BY created_at DESC
        LIMIT $2)
     ) combined
     WHERE created_at >= $3
     ORDER BY created_at DESC
     LIMIT $2`,
    [customerId, clampedLimit, cutoff],
  );
  return r.rows;
}

// sumUsage returns the total billable units for a customer over the last `days` days.
export async function sumUsage(customerId: string, days: number): Promise<number> {
  const r = await pool.query<{ units: string }>(
    `SELECT COALESCE(SUM(billable_units), 0)::text AS units
     FROM usage_events
     WHERE customer_id = $1 AND created_at >= NOW() - INTERVAL '1 day' * $2`,
    [customerId, days],
  );
  return Number(r.rows[0].units);
}

function validateUsageQueryParams(
  customerId: string,
  from: Date,
  to: Date,
  operation?: string,
): { effectiveOp: string | undefined } {
  if (!UUID_RE.test(customerId)) {
    throw new Error("invalid customerId");
  }
  if (!(from instanceof Date) || isNaN(from.getTime()) || !(to instanceof Date) || isNaN(to.getTime())) {
    throw new Error("invalid date range");
  }
  // Strict greater-than: from === to is a valid empty half-open interval [t, t) returning zero rows.
  if (from.getTime() > to.getTime()) {
    throw new Error("from must not be after to");
  }
  if (to.getTime() - from.getTime() > MAX_USAGE_RANGE_MS) {
    throw new Error(`date range exceeds maximum of ${MAX_USAGE_RANGE_DAYS} days`);
  }
  const effectiveOp = operation?.trim() || undefined;
  if (effectiveOp !== undefined && [...effectiveOp].length > MAX_OPERATION_LENGTH) {
    throw new Error(`operation too long (max ${MAX_OPERATION_LENGTH} characters)`);
  }
  return { effectiveOp };
}

export interface UsageOperationRow {
  operation: string;
  /** Saturated at Number.MAX_SAFE_INTEGER if the true sum exceeds it. */
  total_billable_units: number;
  /** Saturated at Number.MAX_SAFE_INTEGER if the true count exceeds it. */
  event_count: number;
}

// usageByOperation returns per-operation aggregates from usage_events for a customer
// over the half-open interval [from, to) — from is inclusive, to is exclusive.
// Pass a non-empty operation to filter to one operation only.
// Uses parameterized $-placeholders; no string interpolation of caller-supplied values.
export async function usageByOperation(
  customerId: string,
  from: Date,
  to: Date,
  operation?: string,
): Promise<UsageOperationRow[]> {
  const { effectiveOp } = validateUsageQueryParams(customerId, from, to, operation);
  type Row = { operation: string; total_billable_units: string; event_count: string };
  const cap = BigInt(Number.MAX_SAFE_INTEGER);
  const mapRow = (row: Row): UsageOperationRow => {
    const rawUnits = BigInt(row.total_billable_units);
    const rawCount = BigInt(row.event_count);
    return {
      operation: row.operation,
      total_billable_units: rawUnits > cap ? Number.MAX_SAFE_INTEGER : Number(rawUnits),
      event_count: rawCount > cap ? Number.MAX_SAFE_INTEGER : Number(rawCount),
    };
  };
  if (effectiveOp) {
    const r = await pool.query<Row>(
      `SELECT operation,
              COALESCE(SUM(billable_units), 0)::text AS total_billable_units,
              COUNT(*)::text AS event_count
       FROM usage_events
       WHERE customer_id = $1 AND created_at >= $2 AND created_at < $3 AND operation = $4
       GROUP BY operation ORDER BY operation LIMIT $5`,
      [customerId, from, to, effectiveOp, MAX_USAGE_OPERATIONS_LIMIT],
    );
    return r.rows.map(mapRow);
  }
  const r = await pool.query<Row>(
    `SELECT operation,
            COALESCE(SUM(billable_units), 0)::text AS total_billable_units,
            COUNT(*)::text AS event_count
     FROM usage_events
     WHERE customer_id = $1 AND created_at >= $2 AND created_at < $3
     GROUP BY operation ORDER BY operation LIMIT $4`,
    [customerId, from, to, MAX_USAGE_OPERATIONS_LIMIT],
  );
  return r.rows.map(mapRow);
}

export interface UsageEventRow {
  operation: string;
  /** Saturated at Number.MAX_SAFE_INTEGER if the true value exceeds it. */
  billable_units: number;
  created_at: Date;
}

// listUsageEvents returns raw usage_events rows for a customer over the half-open
// interval [from, to) — from is inclusive, to is exclusive. Newest first,
// capped at MAX_USAGE_EVENTS_LIMIT rows. Pass a non-empty operation to filter.
// Uses parameterized $-placeholders; no string interpolation of caller-supplied values.
export async function listUsageEvents(
  customerId: string,
  from: Date,
  to: Date,
  operation?: string,
): Promise<UsageEventRow[]> {
  const { effectiveOp } = validateUsageQueryParams(customerId, from, to, operation);
  // pg's OID-1184 (timestamptz) parser always returns a JS Date in UTC regardless of
  // the server's DateStyle setting; no ::text cast or to_char conversion needed.
  type Row = { operation: string; billable_units: string; created_at: Date };
  const cap = BigInt(Number.MAX_SAFE_INTEGER);
  const mapRow = (row: Row): UsageEventRow => {
    const rawUnits = BigInt(row.billable_units);
    const d = row.created_at;
    if (isNaN(d.getTime())) throw new Error("invalid created_at returned from database");
    return {
      operation: row.operation,
      billable_units: rawUnits > cap ? Number.MAX_SAFE_INTEGER : Number(rawUnits),
      created_at: d,
    };
  };
  if (effectiveOp) {
    const r = await pool.query<Row>(
      `SELECT operation, billable_units::text AS billable_units, created_at
       FROM usage_events
       WHERE customer_id = $1 AND created_at >= $2 AND created_at < $3 AND operation = $4
       ORDER BY created_at DESC LIMIT $5`,
      [customerId, from, to, effectiveOp, MAX_USAGE_EVENTS_LIMIT],
    );
    return r.rows.map(mapRow);
  }
  const r = await pool.query<Row>(
    `SELECT operation, billable_units::text AS billable_units, created_at
     FROM usage_events
     WHERE customer_id = $1 AND created_at >= $2 AND created_at < $3
     ORDER BY created_at DESC LIMIT $4`,
    [customerId, from, to, MAX_USAGE_EVENTS_LIMIT],
  );
  return r.rows.map(mapRow);
}
