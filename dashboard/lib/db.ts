import { Pool } from "pg";
import { emitAuditEvent } from "@/lib/audit";
import { getRedis } from "@/lib/redis";
import { UUID_RE } from "@/lib/validation";
import { MS_PER_DAY, MAX_USAGE_RANGE_DAYS } from "./constants";
import { generateKey, hashKey } from "@/lib/keys";
export { MAX_USAGE_RANGE_DAYS };

declare global {
  // eslint-disable-next-line no-var
  var _crucible_pool: Pool | undefined;
}

export const pool: Pool =
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
const MAX_USAGE_RANGE_MS = MAX_USAGE_RANGE_DAYS * MS_PER_DAY;

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
  expires_at: Date | null;
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
    `SELECT id, prefix, name, last_used_at, expires_at FROM api_keys
     WHERE customer_id = $1 AND revoked_at IS NULL
       AND (expires_at IS NULL OR expires_at > NOW())
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
  emitAuditEvent(pool, {
    actorType: "customer",
    actorId: customerId,
    action: "api_key.created",
    targetType: "api_key",
    targetId: keyId,
    details: { name, prefix },
  }).catch(() => {});
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
    emitAuditEvent(pool, {
      actorType: "customer",
      actorId: customerId,
      action: "api_key.revoked",
      targetType: "api_key",
      targetId: keyId,
      details: { prefix },
    }).catch(() => {});
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

// Grace period bounds mirror gateway/internal/auth/store.go maxGrace.
const MAX_ROTATE_GRACE_SECS = 7 * 24 * 3600; // 7 days
const DEFAULT_ROTATE_GRACE_SECS = 3600;        // 1 hour

export type RotateResult =
  | { ok: true; newKey: string; newKeyId: string }
  | { ok: false; reason: "not_found" | "forbidden" | "already_expired" };

// rotateApiKey issues a replacement key for keyId owned by customerId.
// The old key's expires_at is set to now + graceSecs (server-clamped) so both keys
// authenticate during the grace window. After the grace window, only the new key works.
//
// CLAUDE.md invariant #7: fires auth: cache DEL for the old prefix after the DB
// transaction commits so the gateway's hot-path enforces the new expires_at immediately.
export async function rotateApiKey(
  keyId: string,
  customerId: string,
  graceSecs = DEFAULT_ROTATE_GRACE_SECS,
): Promise<RotateResult> {
  const clampedGrace = Math.max(
    0,
    Math.min(
      Number.isFinite(graceSecs) ? Math.floor(graceSecs) : DEFAULT_ROTATE_GRACE_SECS,
      MAX_ROTATE_GRACE_SECS,
    ),
  );

  const salt = process.env.API_KEY_HASH_SALT;
  if (!salt || salt.length < 32) {
    throw new Error("API_KEY_HASH_SALT not configured");
  }
  const productPrefix = process.env.API_KEY_PREFIX ?? "cru_";

  const client = await pool.connect();
  try {
    await client.query("BEGIN");

    // Lock the old key for the duration of the transaction so concurrent revocations
    // or double-rotations cannot interleave. Mirrors Store.Rotate in gateway.
    const lockResult = await client.query<{ prefix: string; name: string | null }>(
      `SELECT prefix, name FROM api_keys
       WHERE id = $1 AND customer_id = $2 AND revoked_at IS NULL
         AND expires_at IS NULL
       FOR UPDATE`,
      [keyId, customerId],
    );

    if (lockResult.rows.length === 0) {
      // No active owned key — disambiguate not_found / forbidden / already_expired.
      const check = await client.query<{ customer_id: string }>(
        `SELECT customer_id::text FROM api_keys WHERE id = $1`,
        [keyId],
      );
      await client.query("ROLLBACK");

      if (check.rows.length === 0) return { ok: false, reason: "not_found" };
      if (check.rows[0].customer_id !== customerId) return { ok: false, reason: "forbidden" };
      return { ok: false, reason: "already_expired" };
    }

    const { prefix: oldPrefix, name } = lockResult.rows[0];

    const { full: newFull, prefix: newPrefix } = generateKey(productPrefix);
    const hash = hashKey(salt, newFull);

    const insertResult = await client.query<{ id: string }>(
      `INSERT INTO api_keys (customer_id, prefix, hash, name)
       VALUES ($1, $2, $3, $4) RETURNING id::text AS id`,
      [customerId, newPrefix, hash, name],
    );
    const newKeyId = insertResult.rows[0].id;

    const expiresAt = new Date(Date.now() + clampedGrace * 1000);
    await client.query(
      `UPDATE api_keys SET expires_at = $1 WHERE id = $2`,
      [expiresAt, keyId],
    );

    await client.query("COMMIT");

    // Best-effort cache invalidation: force the gateway to re-read the old key from
    // Postgres on the next request so the new expires_at is cached immediately.
    // Fire-and-forget — a transient Redis failure just means the old key stays cached
    // until the 60s TTL, which is acceptable. (CLAUDE.md invariant #7)
    invalidateAuthCache(oldPrefix);

    // Best-effort audit — failure must never roll back the completed rotation.
    emitAuditEvent(pool, {
      actorType: "customer",
      actorId: customerId,
      action: "api_key.rotated",
      targetType: "api_key",
      targetId: keyId,
      details: { prefix: oldPrefix },
    }).catch(() => {});

    return { ok: true, newKey: newFull, newKeyId };
  } catch (err) {
    try {
      await client.query("ROLLBACK");
    } catch {
      // ignore rollback error; original error is re-thrown
    }
    throw err;
  } finally {
    client.release();
  }
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
  const cutoff = new Date(Date.now() - AUDIT_LOOKBACK_DAYS * MS_PER_DAY);
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
  // Strict greater-than: exactly MAX_USAGE_RANGE_DAYS is allowed (the limit is inclusive).
  if (to.getTime() - from.getTime() > MAX_USAGE_RANGE_MS) {
    throw new Error(`date range exceeds maximum of ${MAX_USAGE_RANGE_DAYS} days`);
  }
  const effectiveOp = operation?.trim() || undefined;
  if (effectiveOp !== undefined && [...effectiveOp].length > MAX_OPERATION_LENGTH) {
    throw new Error(`operation too long (max ${MAX_OPERATION_LENGTH} characters)`);
  }
  return { effectiveOp };
}

function saturateBigIntString(value: string): number {
  const cap = BigInt(Number.MAX_SAFE_INTEGER);
  const n = BigInt(value);
  return n > cap ? Number.MAX_SAFE_INTEGER : Number(n);
}

// Aggregate row returned by usageByOperation. No id field — this groups across
// many rows, unlike UsageEventRow which maps 1-to-1 with usage_events pk.
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
  const mapRow = (row: Row): UsageOperationRow => ({
    operation: row.operation,
    total_billable_units: saturateBigIntString(row.total_billable_units),
    event_count: saturateBigIntString(row.event_count),
  });
  if (effectiveOp) {
    // created_at >= $2 (from inclusive) AND created_at < $3 (to exclusive): half-open [from, to).
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
  // created_at >= $2 (from inclusive) AND created_at < $3 (to exclusive): half-open [from, to).
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
  /** id::text cast in SQL; BIGSERIAL pk so always a non-empty decimal string. isRawEvent validates id.length > 0. */
  id: string;
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
  // id::text: usage_events.id is BIGSERIAL (bigint). Without ::text the value
  // serialises as a JSON number, which fails isRawEvent's typeof r.id !== "string" check.
  type Row = { id: string; operation: string; billable_units: string; created_at: Date };
  const mapRow = (row: Row): UsageEventRow => ({
    id: row.id,
    operation: row.operation,
    billable_units: saturateBigIntString(row.billable_units),
    created_at: row.created_at,
  });
  if (effectiveOp) {
    // created_at >= $2 (from inclusive) AND created_at < $3 (to exclusive): half-open [from, to).
    const r = await pool.query<Row>(
      `SELECT id::text AS id, operation, COALESCE(billable_units, 0)::text AS billable_units, created_at
       FROM usage_events
       WHERE customer_id = $1 AND created_at >= $2 AND created_at < $3 AND operation = $4
       ORDER BY created_at DESC LIMIT $5`,
      [customerId, from, to, effectiveOp, MAX_USAGE_EVENTS_LIMIT],
    );
    return r.rows.map(mapRow);
  }
  // created_at >= $2 (from inclusive) AND created_at < $3 (to exclusive): half-open [from, to).
  const r = await pool.query<Row>(
    `SELECT id::text AS id, operation, COALESCE(billable_units, 0)::text AS billable_units, created_at
     FROM usage_events
     WHERE customer_id = $1 AND created_at >= $2 AND created_at < $3
     ORDER BY created_at DESC LIMIT $4`,
    [customerId, from, to, MAX_USAGE_EVENTS_LIMIT],
  );
  return r.rows.map(mapRow);
}

export async function getStripeCustomerId(customerId: string): Promise<string | null> {
  const r = await pool.query<{ stripe_customer_id: string | null }>(
    "SELECT stripe_customer_id FROM customers WHERE id = $1",
    [customerId],
  );
  return r.rows[0]?.stripe_customer_id ?? null;
}

export async function sumUsage(customerId: string, days: number): Promise<number> {
  const r = await pool.query<{ units: string }>(
    `SELECT COALESCE(SUM(billable_units), 0)::text AS units
     FROM usage_events
     WHERE customer_id = $1 AND created_at >= NOW() - INTERVAL '1 day' * $2`,
    [customerId, days],
  );
  const units = r.rows[0]?.units;
  if (units === undefined || units === null) return 0;
  return saturateBigIntString(units);
}

// ─── Outbound Webhooks ────────────────────────────────────────────────────────

/**
 * WEBHOOK_EVENT_TYPES mirrors gateway/internal/events.AllEventTypes. The events
 * package is Go-only, so the dashboard can't import it directly — keep both
 * lists in sync manually. The gateway's own registration-time check lives at
 * webhookout.ValidateSubscribedEvents; this is the TypeScript-side equivalent.
 */
export const WEBHOOK_EVENT_TYPES = [
  "subscription.updated",
  "subscription.deleted",
  "quota.exceeded",
  "api_key.rotated",
  "api_key.revoked",
] as const;

export type WebhookEventType = (typeof WEBHOOK_EVENT_TYPES)[number];

export function isValidWebhookEventType(t: string): t is WebhookEventType {
  return (WEBHOOK_EVENT_TYPES as readonly string[]).includes(t);
}

/**
 * Parses the subscribed_events field of a webhook registration/update request.
 * undefined/null means "subscribe to every event" (stored as SQL NULL); any
 * other value must be an array of strings, each a member of WEBHOOK_EVENT_TYPES.
 * Rejects arrays longer than the catalogue itself (only possible via repeats,
 * since every entry must be a distinct catalogue member) so a caller can't
 * bloat the stored TEXT[] — deduped on top of that, since ANY(subscribed_events)
 * is rescanned on every Emit.
 */
export function parseSubscribedEvents(
  value: unknown,
): { ok: true; events: string[] | null } | { ok: false; error: string } {
  if (value === undefined || value === null) return { ok: true, events: null };
  if (!Array.isArray(value)) {
    return { ok: false, error: "subscribed_events must be an array of strings" };
  }
  if (value.length > WEBHOOK_EVENT_TYPES.length) {
    return { ok: false, error: `subscribed_events must not exceed ${WEBHOOK_EVENT_TYPES.length} entries` };
  }
  const seen = new Set<string>();
  for (const v of value) {
    if (typeof v !== "string") {
      return { ok: false, error: "subscribed_events must contain only strings" };
    }
    if (!isValidWebhookEventType(v)) {
      return { ok: false, error: `unknown event type: ${v}` };
    }
    seen.add(v);
  }
  return { ok: true, events: [...seen] };
}

export interface WebhookEndpointRow {
  id: string;
  url: string;
  active: boolean;
  created_at: Date;
  // null means subscribed to every event type (backward-compatible default).
  subscribed_events: string[] | null;
}

export interface WebhookEndpointCreated extends WebhookEndpointRow {
  // secret_hex is returned exactly once on creation and never again.
  secret_hex: string;
}

export interface WebhookDeliveryRow {
  id: string;
  event_id: string;
  endpoint_id: string;
  endpoint_url: string;
  status: string;
  attempts: number;
  last_response_code: number | null;
  created_at: Date;
}

const MAX_WEBHOOK_DELIVERIES = 200;

/**
 * Insert a new webhook endpoint. The secret is generated by Postgres using
 * gen_random_bytes(32) and returned as a hex string exactly once via RETURNING.
 * Subsequent calls to listWebhookEndpoints never expose the secret.
 * subscribedEvents null (the default) subscribes the endpoint to every event type.
 */
export async function insertWebhookEndpoint(
  customerId: string,
  url: string,
  subscribedEvents: string[] | null = null,
): Promise<WebhookEndpointCreated> {
  const r = await pool.query<WebhookEndpointCreated>(
    `INSERT INTO webhook_endpoints (customer_id, url, secret, subscribed_events)
     VALUES ($1, $2, gen_random_bytes(32), $3)
     RETURNING id::text AS id, url, active, created_at, subscribed_events,
               encode(secret, 'hex') AS secret_hex`,
    [customerId, url, subscribedEvents],
  );
  return r.rows[0];
}

/** List active webhook endpoints for a customer. Never returns the secret. */
export async function listWebhookEndpoints(
  customerId: string,
): Promise<WebhookEndpointRow[]> {
  const r = await pool.query<WebhookEndpointRow>(
    `SELECT id::text AS id, url, active, created_at, subscribed_events
     FROM webhook_endpoints
     WHERE customer_id = $1 AND active = TRUE
     ORDER BY created_at DESC`,
    [customerId],
  );
  return r.rows;
}

/**
 * Deactivate a webhook endpoint. Returns true if the row was found and owned
 * by the customer, false if not found or forbidden (wrong customer).
 */
export async function revokeWebhookEndpoint(
  endpointId: string,
  customerId: string,
): Promise<"ok" | "not_found" | "forbidden"> {
  // Two-phase: attempt UPDATE first; if 0 rows affected, disambiguate.
  const update = await pool.query<{ id: string }>(
    `UPDATE webhook_endpoints SET active = FALSE
     WHERE id = $1 AND customer_id = $2 AND active = TRUE
     RETURNING id::text AS id`,
    [endpointId, customerId],
  );
  if (update.rows.length > 0) return "ok";

  const check = await pool.query<{ customer_id: string }>(
    `SELECT customer_id::text FROM webhook_endpoints WHERE id = $1`,
    [endpointId],
  );
  if (check.rows.length === 0) return "not_found";
  return "forbidden";
}

/**
 * Update the subscribed event types for an existing, active endpoint owned by
 * customerId. Passing null clears any explicit subscription, reverting the
 * endpoint to receiving every catalogue event (the pre-0017 default).
 *
 * When narrowing to an explicit list, this also prunes any pending/dead_letter
 * webhook_deliveries rows for event types no longer in that list. Without this,
 * a row queued while subscribed, then orphaned by processDue's claim-time
 * subscription check (emitter.go) after narrowing, would sit in 'pending'
 * forever and silently become deliverable again if the customer later re-adds
 * that event type — reviving an event they'd already opted out of. Rows
 * currently 'delivering' are left alone; an attempt already in flight
 * completes normally.
 */
export async function updateWebhookEndpointSubscription(
  endpointId: string,
  customerId: string,
  subscribedEvents: string[] | null,
): Promise<"ok" | "not_found" | "forbidden"> {
  const update = await pool.query<{ id: string }>(
    `UPDATE webhook_endpoints SET subscribed_events = $3
     WHERE id = $1 AND customer_id = $2 AND active = TRUE
     RETURNING id::text AS id`,
    [endpointId, customerId, subscribedEvents],
  );
  if (update.rows.length > 0) {
    if (subscribedEvents !== null) {
      await pool.query(
        `DELETE FROM webhook_deliveries
         WHERE endpoint_id = $1
           AND status IN ('pending', 'dead_letter')
           AND event_type <> ALL($2)`,
        [endpointId, subscribedEvents],
      );
    }
    return "ok";
  }

  const check = await pool.query<{ customer_id: string }>(
    `SELECT customer_id::text FROM webhook_endpoints WHERE id = $1`,
    [endpointId],
  );
  if (check.rows.length === 0) return "not_found";
  return "forbidden";
}

/** List the most recent webhook deliveries across all endpoints of a customer. */
export async function listWebhookDeliveries(
  customerId: string,
): Promise<WebhookDeliveryRow[]> {
  const r = await pool.query<WebhookDeliveryRow>(
    `SELECT d.id::text AS id, d.event_id, d.endpoint_id::text AS endpoint_id,
            we.url AS endpoint_url, d.status, d.attempts,
            d.last_response_code, d.created_at
     FROM webhook_deliveries d
     JOIN webhook_endpoints we ON we.id = d.endpoint_id
     WHERE we.customer_id = $1
     ORDER BY d.created_at DESC
     LIMIT $2`,
    [customerId, MAX_WEBHOOK_DELIVERIES],
  );
  return r.rows;
}
