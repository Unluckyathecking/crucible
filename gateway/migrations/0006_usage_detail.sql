-- Covering index for per-operation usage detail queries.
-- Supports WHERE customer_id = $1 AND created_at BETWEEN $2 AND $3
-- with optional AND operation = $4, giving the planner index-only access to operation.
BEGIN;

CREATE INDEX IF NOT EXISTS idx_usage_detail ON usage_events(customer_id, operation, created_at);

COMMIT;
