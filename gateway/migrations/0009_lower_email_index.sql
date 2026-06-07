-- Functional index on LOWER(email) to speed up the customer.created webhook handler
-- which queries: SELECT id FROM customers WHERE LOWER(email) = LOWER($1)
-- UNIQUE ensures a single canonical row per email regardless of case, which
-- prevents handleCustomerCreated's QueryRow from returning an arbitrary row
-- when two accounts with the same email but different casing somehow exist.
CREATE UNIQUE INDEX IF NOT EXISTS idx_customers_lower_email ON customers (LOWER(email));
