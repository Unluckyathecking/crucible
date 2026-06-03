import type { Pool } from "pg";

// AuditEvent mirrors audit_log and gateway/internal/audit.Event.
// The column set (actor_type, actor_id, action, target_type, target_id, details) must
// stay in sync with the Go emitter — any rename here requires a matching rename there.
// Optional fields (targetType, targetId, details) insert SQL NULL when absent; both
// the Go emitter (nullStr helper) and this emitter (?? null) agree on NULL semantics.
export interface AuditEvent {
  actorType: "customer" | "admin" | "system";
  actorId: string;
  action: string;      // e.g. "api_key.created" | "api_key.revoked" | "plan.changed"
  targetType?: string; // e.g. "api_key"
  targetId?: string;   // UUID or other stable identifier
  details?: Record<string, unknown>;
}

// emitAuditEvent writes one append-only row to audit_log. Errors from the INSERT
// propagate to the caller; callers are responsible for fire-and-forget semantics
// (void ... .catch()) so that an audit failure does not surface as a user-visible error.
// Takes the pool as a parameter to mirror the Go Emit(ctx, db, event) signature.
export async function emitAuditEvent(pool: Pool, event: AuditEvent): Promise<void> {
  await pool.query(
    `INSERT INTO audit_log (actor_type, actor_id, action, target_type, target_id, details)
     VALUES ($1, $2, $3, $4, $5, $6)`,
    [
      event.actorType,
      event.actorId,
      event.action,
      event.targetType ?? null,
      event.targetId ?? null,
      event.details ?? null,
    ],
  );
}
