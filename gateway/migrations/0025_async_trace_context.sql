-- Adds a nullable W3C traceparent column to both async outbox tables so an
-- enqueue call executing under an active OTEL span (see
-- gateway/internal/tracing.CaptureTraceparent) can persist it at write time
-- for the background engine to restore as the remote parent at execution
-- time (gateway/internal/tracing.RestoreTraceparent) — see jobs.Store.Enqueue/
-- jobs.Executor.process and webhookout.Emitter.Emit/deliver. NULL when
-- tracing is disabled or the enqueue happened outside any traced request;
-- never enforced NOT NULL. Idempotent (invariant #8): safe to re-run on
-- every gateway boot.

BEGIN;

ALTER TABLE async_jobs ADD COLUMN IF NOT EXISTS traceparent TEXT;
ALTER TABLE webhook_deliveries ADD COLUMN IF NOT EXISTS traceparent TEXT;

COMMIT;
