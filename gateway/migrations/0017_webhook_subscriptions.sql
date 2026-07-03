-- webhook_endpoints.subscribed_events: NULL means "all catalogue events" (the
-- pre-0017 all-or-nothing fan-out behavior), preserved as the default for every
-- pre-existing row and any new endpoint that doesn't opt into a subset. A
-- non-NULL array restricts delivery to just those event types — Emit's
-- INSERT…SELECT (webhookout/emitter.go) filters on it, and registration paths
-- validate entries against events.AllEventTypes before storing them.
-- Idempotent: safe to re-run on every gateway boot (invariant #8).
ALTER TABLE webhook_endpoints
  ADD COLUMN IF NOT EXISTS subscribed_events TEXT[];
