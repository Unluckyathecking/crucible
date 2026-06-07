-- Functional index on LOWER(email) to speed up the customer.created webhook handler
-- which queries: SELECT id FROM customers WHERE LOWER(email) = LOWER($1)
CREATE INDEX IF NOT EXISTS idx_customers_lower_email ON customers (LOWER(email));
