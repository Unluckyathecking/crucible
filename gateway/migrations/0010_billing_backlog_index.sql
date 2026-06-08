-- Index for the billing reconcile queries (BacklogStats, UnbillableUsage).
--
-- BacklogStats filters usage_events WHERE flushed_to_stripe = FALSE and joins to
-- customers on customer_id. This partial index lets the planner scan only unflushed
-- rows indexed by customer_id rather than performing a full usage_events scan.
--
-- UnbillableUsage uses the same flushed_to_stripe = FALSE filter. For small customer
-- tables the planner may instead drive from customers (stripe_customer_id IS NULL)
-- and nest-loop into usage_events, in which case this index is not used. Useful
-- primarily for BacklogStats on large usage_events tables.
--
-- Idempotent: CREATE INDEX IF NOT EXISTS is safe to re-apply on every gateway boot.

CREATE INDEX IF NOT EXISTS idx_usage_unflushed
    ON usage_events(customer_id)
    WHERE flushed_to_stripe = FALSE;
