-- Covering index for per-operation usage detail queries.
-- Column order: customer_id (equality) → created_at (range) → operation (trailing equality/IOS).
-- This allows the planner to satisfy the common unfiltered case
-- (WHERE customer_id = $1 AND created_at >= $2 AND created_at < $3) without touching operation,
-- and to extend to the filtered case by adding AND operation = $4 using the trailing column.
-- Note: no explicit transaction — CREATE INDEX CONCURRENTLY cannot run inside a transaction block.
CREATE INDEX IF NOT EXISTS idx_usage_detail ON usage_events(customer_id, created_at, operation);
