-- Functional index on LOWER(email) to speed up the customer.created webhook handler
-- which queries: SELECT id FROM customers WHERE LOWER(email) = LOWER($1)
-- Functional index on LOWER(email) for case-insensitive customer lookup.
-- Used by customer.created webhook handler: SELECT id FROM customers WHERE LOWER(email) = LOWER($1)
-- Distinct from 0008's partial index which optimises UPDATE ... WHERE stripe_customer_id IS NULL.
-- Non-unique: existing rows may have case-variant emails if the OAuth provider ever normalised
-- differently; the application layer (ensureCustomer) is the deduplication boundary.
CREATE INDEX IF NOT EXISTS idx_customers_lower_email ON customers (LOWER(email));
