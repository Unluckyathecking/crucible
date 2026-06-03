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

// emitAuditEvent writes one append-only row to audit_log. Mirrors Go validation:
// system events must have empty actorId (they always store NULL); non-system events
// must have non-empty actorId. Validation failures and INSERT errors are both caught
// and logged internally — callers use `void emitAuditEvent(...)` to mark the
// fire-and-forget intent explicitly.
// Takes the pool as a parameter to mirror the Go Emit(ctx, db, event) signature.
export async function emitAuditEvent(pool: Pool, event: AuditEvent): Promise<void> {
  // Mirror Go's symmetric validation: system events must not carry an actorId
  // (background jobs have no individual actor); non-system events must have one.
  if (event.actorType === "system" && event.actorId) {
    console.error("audit emit skipped: actor_id must be empty for system events", { action: event.action });
    return;
  }
  if (event.actorType !== "system" && !event.actorId) {
    console.error("audit emit skipped: actor_id required for non-system events", { action: event.action, actorType: event.actorType });
    return;
  }
  try {
    await pool.query(
      `INSERT INTO audit_log (actor_type, actor_id, action, target_type, target_id, details)
       VALUES ($1, $2, $3, $4, $5, $6)`,
      [
        event.actorType,
        event.actorType === "system" ? null : event.actorId,
        event.action,
        event.targetType ?? null,
        event.targetId ?? null,
        event.details ?? null,
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
