BEGIN;

-- Per-endpoint delivery-health accounting: how many consecutive terminal
-- dead-letters an endpoint has accumulated since its last successful
-- delivery, and — once WEBHOOK_ENDPOINT_FAILURE_THRESHOLD is crossed —
-- when and why it was auto-disabled. disabled_at/disabled_reason are NULL
-- for both a never-disabled endpoint and a customer soft-deleted one
-- (DeleteEndpoint, webhookout/endpoints.go) — only the auto-disable path
-- (webhookout/health.go) ever sets them, which is what lets EnableEndpoint
-- distinguish "auto-disabled, re-enable me" from "customer-deleted, leave
-- it alone" even though both states share active = FALSE.
ALTER TABLE webhook_endpoints ADD COLUMN IF NOT EXISTS consecutive_failures INTEGER NOT NULL DEFAULT 0 CHECK (consecutive_failures >= 0);
ALTER TABLE webhook_endpoints ADD COLUMN IF NOT EXISTS disabled_at TIMESTAMPTZ;
ALTER TABLE webhook_endpoints ADD COLUMN IF NOT EXISTS disabled_reason TEXT;

COMMIT;
