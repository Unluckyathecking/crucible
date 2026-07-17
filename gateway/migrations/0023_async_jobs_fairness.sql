-- Supporting indexes for the opt-in multi-tenant fair-claim/admission path
-- (see gateway/internal/jobs.Store.Claim/CountActive, gated by
-- JOB_MAX_INFLIGHT_PER_CUSTOMER / JOB_MAX_QUEUED_PER_CUSTOMER). Both knobs
-- default to 0 (disabled), but these indexes are cheap to maintain and make
-- the fairness queries efficient the moment a product clone opts in, without
-- requiring a second migration later. Idempotent (invariant #8): safe to
-- re-run on every gateway boot.

BEGIN;

-- Store.runningCountsByCustomer's GROUP BY customer_id scan, restricted to
-- the same 'running' rows idx_async_jobs_stuck already indexes by claimed_at.
CREATE INDEX IF NOT EXISTS idx_async_jobs_running_customer ON async_jobs(customer_id) WHERE status = 'running';

-- Store.CountActive's per-customer queued+running backlog count, backing
-- enqueueAsync's JOB_MAX_QUEUED_PER_CUSTOMER admission check.
CREATE INDEX IF NOT EXISTS idx_async_jobs_customer_active ON async_jobs(customer_id) WHERE status IN ('queued', 'running');

COMMIT;
