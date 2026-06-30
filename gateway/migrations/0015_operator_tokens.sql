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
-- idx_audit_target_id ON audit_log(target_id, created_at DESC) already exists from 0005_audit_indexes.sql.
CREATE INDEX IF NOT EXISTS idx_audit_log_created_at ON audit_log(created_at DESC);

-- Support efficient customer list pagination ordered by created_at (used by /v1/admin/customers).
CREATE INDEX IF NOT EXISTS idx_customers_created_at ON customers(created_at DESC);
-- Support efficient plan-filtered customer pagination.
CREATE INDEX IF NOT EXISTS idx_customers_plan_created_at ON customers(plan_id, created_at DESC);
