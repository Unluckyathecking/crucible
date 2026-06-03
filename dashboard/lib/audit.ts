import type { Pool } from "pg";

// AuditEvent mirrors audit_log and gateway/internal/audit.Event.
// The column set (actor_type, actor_id, action, target_type, target_id, details) must
// stay in sync with the Go emitter — any rename here requires a matching rename there.
// Optional fields (targetType, targetId, details) insert SQL NULL when absent.
export interface AuditEvent {
  actorType: "customer" | "admin" | "system";
  actorId: string;
  action: string;      // e.g. "api_key.created" | "api_key.revoked" | "plan.changed"
  targetType?: string; // e.g. "api_key"
  targetId?: string;   // UUID or other stable identifier
  details?: Record<string, unknown>;
}

// Keys permitted in audit event details. Any other key is redacted before storage.
// Keeping an explicit allowlist prevents secrets or PII from leaking into the audit
// trail if a caller ever adds sensitive fields (e.g. fullKey, hash) to the details map.
const ALLOWED_DETAIL_KEYS = new Set(["name", "prefix", "plan_id", "attempt"]);

function sanitizeDetails(details: Record<string, unknown>): Record<string, unknown> {
  const sanitized: Record<string, unknown> = {};
  for (const [k, v] of Object.entries(details)) {
    sanitized[k] = ALLOWED_DETAIL_KEYS.has(k) ? v : "[REDACTED]";
  }
  return sanitized;
}

// emitAuditEvent writes one append-only row to audit_log. Mirrors Go validation:
// system events must have empty actorId (they always store NULL); non-system events
// must have non-empty actorId. Validation failures and INSERT errors are both caught
// and logged internally — callers use `void emitAuditEvent(...)` to mark the
// fire-and-forget intent explicitly.
// Takes the pool as a parameter to mirror the Go Emit(ctx, db, event) signature.
export async function emitAuditEvent(pool: Pool, event: AuditEvent): Promise<void> {
  // Mirror Go's symmetric validation: system events must not carry an actorId
  // (background jobs have no individual actor); non-system events must have one.
  const isSystem = event.actorType === "system";
  // Explicit empty-string checks mirror Go's e.ActorID == "" / e.ActorID != "" checks.
  // Truthiness would treat "0" or "false" differently; explicit comparison avoids drift.
  if (isSystem && event.actorId !== "") {
    console.error("audit emit skipped: actor_id must be empty for system events", { action: event.action });
    return;
  }
  if (!isSystem && event.actorId === "") {
    console.error("audit emit skipped: actor_id required for non-system events", { action: event.action, actorType: event.actorType });
    return;
  }
  // JSON.stringify and sanitizeDetails are inside the try block so any serialization
  // error (e.g. circular reference) is caught and logged rather than escaping as an
  // unhandled rejection from this async function.
  try {
    const detailsJSON = event.details != null
      ? JSON.stringify(sanitizeDetails(event.details))
      : null;
    await pool.query(
      `INSERT INTO audit_log (actor_type, actor_id, action, target_type, target_id, details)
       VALUES ($1, $2, $3, $4, $5, $6::jsonb)`,
      [
        event.actorType,
        isSystem ? null : event.actorId,
        event.action,
        event.targetType ?? null,
        event.targetId ?? null,
        detailsJSON,
      ],
    );
  } catch (err) {
    console.error("audit emit failed:", {
      action: event.action,
      actorId: event.actorId,
      targetId: event.targetId,
      error: err instanceof Error ? err.message : String(err),
    });
  }
}
