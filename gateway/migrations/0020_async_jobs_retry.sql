-- Bounded retry/backoff/dead-letter lifecycle for async_jobs (see gateway/internal/jobs).
--
-- Today jobs.Executor.fail() marks a job permanently 'failed' on the FIRST worker
-- error, including a transient WORKER_UNREACHABLE/transport blip from a worker
-- restart. This adds the same retry-with-backoff-then-dead-letter columns
-- webhook_deliveries already has (0012_outbound_webhooks.sql's attempts/
-- next_attempt_at), scoped to async_jobs. attempts/max_attempts/next_attempt_at
-- are added via ADD COLUMN IF NOT EXISTS — mirroring 0004_usage_batches.sql's
-- batch_id add — so this file is safe whether it's applying to a fresh database
-- or one that already ran an earlier revision. Idempotent: safe to re-run on
-- every gateway boot (invariant #8). No version-tracking table.

BEGIN;

-- Number of worker-invocation attempts made so far. Only incremented by a
-- retryable (WORKER_UNREACHABLE / transport) failure's requeue; a
-- deterministic failure (worker structured business error, billable_units<1
-- contract violation) leaves this column untouched — see jobs.Store.Fail.
ALTER TABLE async_jobs ADD COLUMN IF NOT EXISTS attempts INT NOT NULL DEFAULT 0;

-- Snapshot of the attempt ceiling for this row. The executor's own
-- ExecutorConfig.MaxAttempts (JOB_MAX_ATTEMPTS) is the value actually
-- enforced by jobs.Executor at retry-decision time; this column exists for
-- per-row observability/audit, matching webhook_deliveries' shape.
ALTER TABLE async_jobs ADD COLUMN IF NOT EXISTS max_attempts INT NOT NULL DEFAULT 3;

-- Earliest time this row is eligible to be claimed again. Defaults to NOW()
-- so newly-enqueued rows are immediately eligible; a retryable failure pushes
-- this out by the backoff delay (see jobs.Executor.retryOrDeadLetter).
ALTER TABLE async_jobs ADD COLUMN IF NOT EXISTS next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT NOW();

COMMIT;
