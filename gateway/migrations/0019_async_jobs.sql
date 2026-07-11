-- Durable Postgres-backed async job queue (see gateway/internal/jobs).
-- Rows are claimed via SELECT ... FOR UPDATE SKIP LOCKED, mirroring the
-- webhook_deliveries claiming pattern in gateway/internal/webhookout/emitter.go.
-- Idempotent: safe to re-run on every gateway boot (invariant #8) — no
-- version-tracking table.

BEGIN;

CREATE TABLE IF NOT EXISTS async_jobs (
  id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  customer_id    UUID NOT NULL REFERENCES customers(id),
  api_key_id     UUID NOT NULL REFERENCES api_keys(id),
  operation      TEXT NOT NULL,
  request_id     TEXT NOT NULL,
  plan           TEXT NOT NULL DEFAULT '',
  payload        JSONB NOT NULL,
  -- Per-route override of the executor's default job timeout (routes_table.go
  -- AsyncRoutes value, in seconds). 0 means "use the gateway's configured default".
  timeout_seconds INTEGER NOT NULL DEFAULT 0,
  status         TEXT NOT NULL DEFAULT 'queued' CHECK (status IN ('queued', 'running', 'succeeded', 'failed')),
  result         JSONB,
  units_label    TEXT NOT NULL DEFAULT '',
  billable_units BIGINT NOT NULL DEFAULT 0 CHECK (billable_units >= 0),
  error_code     TEXT NOT NULL DEFAULT '',
  error_message  TEXT NOT NULL DEFAULT '',
  -- claimed_by identifies the gateway process instance (Executor.instanceID)
  -- currently owning this row, so a graceful shutdown releases only the
  -- jobs THIS process claimed — never another replica's in-flight work.
  claimed_by     UUID,
  claimed_at     TIMESTAMPTZ,
  created_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  updated_at     TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Added after the initial CREATE TABLE within this same not-yet-released
-- migration; ADD COLUMN IF NOT EXISTS (mirroring 0004_usage_batches.sql's
-- batch_id) rather than folding it into the column list above keeps this
-- file safe to apply against either a fresh database (CREATE TABLE runs,
-- then this is a no-op-shaped add) or one that already ran an earlier
-- revision of this file (CREATE TABLE no-ops, this actually adds the
-- column) — both are real states a not-yet-merged migration can meet.
-- Caller's Idempotency-Key header, when present (NULL otherwise). Lets
-- Store.Enqueue return the existing job instead of inserting a duplicate
-- if idempotency.Middleware's own finalize step fails after this insert
-- already committed and a client retry reaches the handler again — see
-- the unique index below.
ALTER TABLE async_jobs ADD COLUMN IF NOT EXISTS idempotency_key TEXT;

-- Claim scan: oldest queued job first.
CREATE INDEX IF NOT EXISTS idx_async_jobs_queued ON async_jobs(created_at) WHERE status = 'queued';
-- GET /v1/jobs/{id} is scoped by customer_id in the SQL itself (IDOR-safe);
-- this index also serves a future list-by-customer endpoint.
CREATE INDEX IF NOT EXISTS idx_async_jobs_customer ON async_jobs(customer_id, created_at DESC);
-- Crash-recovery sweep: 'running' rows abandoned by a process that died
-- without a graceful shutdown (see Store.Claim's stuck-job reset).
CREATE INDEX IF NOT EXISTS idx_async_jobs_stuck ON async_jobs(claimed_at) WHERE status = 'running';
-- Idempotent enqueue: Store.Enqueue's ON CONFLICT target. Partial (only
-- rows with a real idempotency_key participate), so requests without one
-- never collide with each other.
CREATE UNIQUE INDEX IF NOT EXISTS idx_async_jobs_customer_idempotency_unique
  ON async_jobs(customer_id, idempotency_key) WHERE idempotency_key IS NOT NULL;

COMMIT;
