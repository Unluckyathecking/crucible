-- Functional index on LOWER(email) to speed up the customer.created webhook handler
-- which queries: SELECT id FROM customers WHERE LOWER(email) = LOWER($1)
-- Non-unique index on LOWER(email) to speed up the customer.created webhook handler's
-- LOWER(email) = LOWER($1) query. Not unique because existing rows might have
-- case-variant emails if the OAuth provider ever normalised differently; the application
-- layer (ensureCustomer) is the deduplication point.
CREATE INDEX IF NOT EXISTS idx_customers_lower_email ON customers (LOWER(email));
