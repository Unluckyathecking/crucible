-- Support efficient SQL-level LIMIT/OFFSET pagination for the customer-facing
-- GET /v1/keys and GET /v1/webhooks/endpoints list endpoints, which moved off
-- paging.Slice's in-memory windowing onto real SQL pagination (auth.Store.List,
-- webhookout.ListEndpoints). Idempotent: safe to re-run on every gateway boot
-- (invariant #8).
CREATE INDEX IF NOT EXISTS idx_api_keys_customer_created_at ON api_keys(customer_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_webhook_endpoints_customer_created_at ON webhook_endpoints(customer_id, created_at DESC);
