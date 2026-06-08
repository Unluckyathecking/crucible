-- Index for the billing reconcile queries (BacklogStats, UnbillableUsage).
--
-- Both reconcile queries filter usage_events WHERE flushed_to_stripe = FALSE and
-- then join to customers. This partial index lets the planner drive the join from
-- the usage_events side: scan only unflushed rows indexed by customer_id, then look
-- up the customers row. Without it, small customer tables cause a full customers scan
-- with a nested loop into usage_events.
--
-- The existing idx_usage_events_unbatched (0004) already covers the flusher's
-- claim query (batch_id IS NULL AND flushed_to_stripe = FALSE), but that partial
-- condition is too narrow for the reconcile queries which do not filter on batch_id.
--
-- Idempotent: CREATE INDEX IF NOT EXISTS is safe to re-apply on every gateway boot.

CREATE INDEX IF NOT EXISTS idx_usage_unflushed
    ON usage_events(customer_id)
    WHERE flushed_to_stripe = FALSE;
