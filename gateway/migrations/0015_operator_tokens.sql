-- Operator token persistence scaffold. The gateway currently validates operator
-- access against the OPERATOR_TOKEN env var (single static token). This table
-- is reserved for future multi-token operator access with per-token names and
-- revocation. No rows are inserted by this migration; the middleware reads only
-- from the environment until the multi-token path is built.
-- Idempotent: safe to re-run on every gateway boot (invariant #8).
CREATE TABLE IF NOT EXISTS operator_tokens (
  id         UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
  name       TEXT        NOT NULL,
  hash       BYTEA       NOT NULL,   -- SHA-256(salt || token); plaintext never stored
  created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  revoked_at TIMESTAMPTZ
);

-- Support efficient time-range scans on audit_log (used by /v1/admin/audit without customer filter).
CREATE INDEX IF NOT EXISTS idx_audit_log_created_at ON audit_log(created_at DESC);
-- Support efficient lookup of events where the customer is the target.
CREATE INDEX IF NOT EXISTS idx_audit_log_target_id ON audit_log(target_id, created_at DESC);
