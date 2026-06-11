BEGIN;

-- webhook_endpoints: customer-registered HTTPS endpoints for outbound event delivery.
-- secret is stored as raw BYTEA; it is returned to the customer exactly once on creation
-- and is used to sign outgoing POST bodies (HMAC-SHA256 over "timestamp.body").
CREATE TABLE IF NOT EXISTS webhook_endpoints (
  id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  customer_id UUID NOT NULL REFERENCES customers(id) ON DELETE CASCADE,
  url         TEXT NOT NULL,
  secret      BYTEA NOT NULL,
  active      BOOLEAN NOT NULL DEFAULT TRUE,
  created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Supports customer-scoped endpoint list queries.
CREATE INDEX IF NOT EXISTS idx_webhook_endpoints_customer
  ON webhook_endpoints(customer_id, created_at DESC);

-- webhook_deliveries: outbox table for at-least-once delivery.
-- status values are enforced by the CHECK constraint below.
-- claimed_at is set when the worker claims a row (status = 'delivering'); the recovery
-- query uses it to reset rows abandoned by a crashed process, which is the correct
-- measure of how long a row has been in-flight (next_attempt_at is the scheduled time,
-- not the claim time, so it cannot serve this role).
CREATE TABLE IF NOT EXISTS webhook_deliveries (
  id                 BIGSERIAL PRIMARY KEY,
  event_id           TEXT NOT NULL,
  event_type         TEXT NOT NULL DEFAULT '',
  endpoint_id        UUID NOT NULL REFERENCES webhook_endpoints(id) ON DELETE CASCADE,
  payload            JSONB NOT NULL,
  status             TEXT NOT NULL DEFAULT 'pending'
                       CHECK (status IN ('pending', 'delivering', 'delivered', 'dead_letter')),
  attempts           INTEGER NOT NULL DEFAULT 0 CHECK (attempts >= 0),
  claimed_at         TIMESTAMPTZ,
  next_attempt_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  last_response_code INTEGER,
  created_at         TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Supports customer delivery-log queries (join path: delivery → endpoint → customer_id).
CREATE INDEX IF NOT EXISTS idx_webhook_deliveries_endpoint_created
  ON webhook_deliveries(endpoint_id, created_at DESC);

-- Supports the worker's claim-due query: pending rows eligible for delivery.
CREATE INDEX IF NOT EXISTS idx_webhook_deliveries_pending
  ON webhook_deliveries(next_attempt_at)
  WHERE status = 'pending';

COMMIT;
