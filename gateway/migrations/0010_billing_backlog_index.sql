-- Partial index to help the UnbillableUsage reconcile query find unlinked customers.
--
-- The partial condition (WHERE stripe_customer_id IS NULL) lets Postgres consider a
-- customers-first plan: scan only the subset of customers without a Stripe ID, then
-- join to usage_events. Whether the planner actually chooses this plan depends on
-- table statistics — with small customer tables or mostly-flushed usage_events, the
-- planner is likely to drive from usage_events via idx_usage_pending_flush instead,
-- in which case this index is unused. Run EXPLAIN ANALYZE on production data if
-- UnbillableUsage queries appear in slow-query logs.
--
-- The index is on customers(id) — the PK column. The partial condition is what provides
-- the benefit; without it the index would be a redundant duplicate of the PK index.
--
-- Idempotent: CREATE INDEX IF NOT EXISTS is safe to re-apply on every gateway boot.

CREATE INDEX IF NOT EXISTS idx_customers_no_stripe
    ON customers(id)
    WHERE stripe_customer_id IS NULL;
