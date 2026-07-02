-- Supports the operator dead-letter list/replay endpoints (webhookout/replay.go):
-- listing paginates dead_letter rows most-recent-first, and bulk replay filters
-- dead_letter rows by endpoint_id. Partial index keeps it small since dead_letter
-- is expected to be a small fraction of webhook_deliveries at steady state.
-- Idempotent: safe to re-run on every gateway boot (invariant #8).
CREATE INDEX IF NOT EXISTS idx_webhook_deliveries_dead_letter
  ON webhook_deliveries(created_at DESC)
  WHERE status = 'dead_letter';

CREATE INDEX IF NOT EXISTS idx_webhook_deliveries_dead_letter_endpoint
  ON webhook_deliveries(endpoint_id)
  WHERE status = 'dead_letter';
