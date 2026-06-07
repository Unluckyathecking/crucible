-- 0008_checkout_index.sql
-- Adds a partial index to speed up the customer.created webhook handler, which
-- UPDATEs customers by email WHERE stripe_customer_id IS NULL. Without this
-- index the query falls back to the unique btree on email (which covers all rows),
-- but a focused partial index narrows the scan to unlinked customers only.
-- Idempotent: IF NOT EXISTS guards ensure safe re-runs on every gateway boot.

CREATE INDEX IF NOT EXISTS idx_customers_unlinked_email
    ON customers(email)
    WHERE stripe_customer_id IS NULL;
