-- Supplemental indexes for audit_log to support dashboard queries.
-- Idempotent: only adds indexes, never changes columns or the actor_type CHECK.
-- The primary actor index (idx_audit_actor) already ships in 0001_init.sql as
-- (actor_type, actor_id, created_at DESC); it requires a leading actor_type
-- predicate. idx_audit_actor_id below covers the bare actor_id = $1 branch
-- used by listAuditEvents, where actor_type is not known at query time.

BEGIN;

-- actor_id-first index: supports "WHERE actor_id = $1" without leading actor_type.
CREATE INDEX IF NOT EXISTS idx_audit_actor_id ON audit_log(actor_id, created_at DESC);

-- Reverse lookup by target type + id: "show all events touching api_key X"
CREATE INDEX IF NOT EXISTS idx_audit_target ON audit_log(target_type, target_id, created_at DESC);

-- Reverse lookup by target id alone: supports "show all events targeting customer Y"
-- where target_type varies (used by listAuditEvents OR target_id = $1 branch).
CREATE INDEX IF NOT EXISTS idx_audit_target_id ON audit_log(target_id, created_at DESC);

-- Filter by action type: "show all api_key.revoked events"
CREATE INDEX IF NOT EXISTS idx_audit_action ON audit_log(action, created_at DESC);

COMMIT;
