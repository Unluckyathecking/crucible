-- 0008_checkout_index.sql
-- Partial index on customers(email) for rows not yet linked to Stripe.
-- Useful for any scan of unlinked customers (e.g. reconciliation, admin queries).
-- The customer.created webhook handler (handleCustomerCreated) uses LOWER(email)
-- and is served by the functional index in migration 0009, not this one.
-- Idempotent: IF NOT EXISTS guards ensure safe re-runs on every gateway boot.

CREATE INDEX IF NOT EXISTS idx_customers_unlinked_email
    ON customers(email)
    WHERE stripe_customer_id IS NULL;
