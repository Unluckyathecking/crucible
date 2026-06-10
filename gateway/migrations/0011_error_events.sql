BEGIN;

CREATE TABLE IF NOT EXISTS error_events (
  id          BIGSERIAL PRIMARY KEY,
  customer_id UUID NOT NULL REFERENCES customers(id) ON DELETE CASCADE,
  api_key_id  UUID REFERENCES api_keys(id),
  operation   TEXT NOT NULL,
  error_code  TEXT NOT NULL,
  http_status INTEGER NOT NULL,
  message     TEXT NOT NULL,
  request_id  TEXT NOT NULL,
  created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Supports customer-scoped newest-first queries used by both the dashboard and
-- the API route's default ordering.
CREATE INDEX IF NOT EXISTS idx_error_events_customer_created
  ON error_events(customer_id, created_at DESC);

COMMIT;
