-- Stable batch identity for Stripe meter_event emission.
--
-- Why: the original flusher derived the Stripe idempotency-key from MIN(id)..MAX(id)
-- of the unflushed rows at SELECT time. If Stripe accepted the event but the
-- mark-flushed UPDATE failed, the next tick saw the same rows PLUS new arrivals,
-- producing a different (min, max) → a different idempotency key → Stripe billed
-- the old rows again. With a persisted batch_id, the idempotency key stays stable
-- across retries no matter how many new rows arrive.

BEGIN;

ALTER TABLE usage_events ADD COLUMN IF NOT EXISTS batch_id UUID;

-- Unbatched, unflushed rows: candidates for fresh batch claim.
CREATE INDEX IF NOT EXISTS idx_usage_events_unbatched
  ON usage_events(customer_id)
  WHERE batch_id IS NULL AND flushed_to_stripe = FALSE;

-- Batched but unflushed: rows whose Stripe call may have succeeded or failed; retry on next tick.
CREATE INDEX IF NOT EXISTS idx_usage_events_batched_unflushed
  ON usage_events(batch_id)
  WHERE batch_id IS NOT NULL AND flushed_to_stripe = FALSE;

COMMIT;
