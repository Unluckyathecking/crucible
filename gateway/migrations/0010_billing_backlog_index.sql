-- Index to accelerate the UnbillableUsage reconcile scan.
--
-- BacklogStats queries usage_events WHERE flushed_to_stripe = FALSE; the existing
-- idx_usage_pending_flush (0001_init.sql) covers that filter for fast row location,
-- but the aggregation (SUM, COUNT, MIN) must still visit matching rows.
--
-- UnbillableUsage joins usage_events to customers WHERE stripe_customer_id IS NULL.
-- Without this index Postgres performs a full customers seq-scan for the join;
-- with it Postgres can use an index scan on the customers side and immediately filter
-- to unlinked customers before touching usage_events rows.
--
-- Idempotent: CREATE INDEX IF NOT EXISTS is safe to re-apply on every gateway boot.

BEGIN;

CREATE INDEX IF NOT EXISTS idx_customers_no_stripe
    ON customers(id)
    WHERE stripe_customer_id IS NULL;

COMMIT;
