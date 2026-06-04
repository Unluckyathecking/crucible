-- Idempotency key store for deduplicating POST /v1/* retries.
--
-- status_code IS NULL means the first request is still in-flight.
-- status_code IS NOT NULL means the response is stored and can be replayed.
-- Retryable non-2xx outcomes are never stored (the row is deleted after the
-- handler returns a failure), so a genuine retry can still succeed.
--
-- UNIQUE(customer_id, idempotency_key) is the concurrency gate: the first
-- INSERT wins and owns the key; any concurrent INSERT returns 0 rows affected
-- and the middleware returns 409 IDEMPOTENCY_CONFLICT.
--
-- fingerprint is SHA-256(request_body), checked on every hit to prevent silent
-- replay of a mismatched request (422 IDEMPOTENCY_KEY_REUSE on mismatch).
CREATE TABLE IF NOT EXISTS idempotency_keys (
  id              BIGSERIAL PRIMARY KEY,
  customer_id     UUID         NOT NULL REFERENCES customers(id) ON DELETE CASCADE,
  idempotency_key TEXT         NOT NULL,
  fingerprint     BYTEA        NOT NULL,
  status_code     INTEGER,
  response_body   BYTEA,
  created_at      TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
  UNIQUE (customer_id, idempotency_key)
);
