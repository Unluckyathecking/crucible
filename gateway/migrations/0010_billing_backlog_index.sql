-- Index to accelerate the UnbillableUsage reconcile scan.
--
-- BacklogStats (all unflushed rows) is already covered by the partial index
-- idx_usage_pending_flush(created_at WHERE flushed_to_stripe=FALSE) from 0001_init.sql.
-- UnbillableUsage joins usage_events to customers filtering on stripe_customer_id IS NULL;
-- this index lets Postgres do an index-only scan on the customers side of that join instead
-- of a full customers seq-scan.
--
-- Idempotent: CREATE INDEX IF NOT EXISTS is safe to re-apply on every gateway boot.

BEGIN;

CREATE INDEX IF NOT EXISTS idx_customers_no_stripe
    ON customers(id)
    WHERE stripe_customer_id IS NULL;

COMMIT;
