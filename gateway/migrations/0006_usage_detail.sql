-- Covering index for per-operation usage detail queries.
-- Column order: customer_id (equality) → created_at (range) → operation (trailing filter).
-- This lets the planner seek to the time range directly for the common unfiltered case
-- (WHERE customer_id = $1 AND created_at >= $2 AND created_at < $3), then optionally
-- apply AND operation = $4 using the trailing column without an extra filter step.
-- No explicit transaction wrapper: single DDL statements auto-commit in PostgreSQL.
CREATE INDEX IF NOT EXISTS idx_usage_detail ON usage_events(customer_id, created_at, operation);
