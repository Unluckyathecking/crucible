-- Supporting index for the opt-in multi-tenant fair-claim path (see
-- gateway/internal/webhookout.Emitter.claimDue/deliveringCountsByCustomer,
-- gated by WEBHOOK_MAX_INFLIGHT_PER_CUSTOMER). The knob defaults to 0
-- (disabled), but this index is cheap to maintain and makes the fairness
-- query efficient the moment a product clone opts in, without requiring a
-- second migration later. Mirrors 0023_async_jobs_fairness.sql's
-- idx_async_jobs_running_customer. Idempotent (invariant #8): safe to
-- re-run on every gateway boot.

BEGIN;

-- deliveringCountsByCustomer's join+filter down to in-flight rows before
-- grouping by the joined webhook_endpoints.customer_id.
CREATE INDEX IF NOT EXISTS idx_webhook_deliveries_delivering
  ON webhook_deliveries(endpoint_id)
  WHERE status = 'delivering';

COMMIT;
