# Job brief — webhookout per-customer delivery fairness

Planner brief for the 10X worker. Decomposes the next phase of the outbound-webhook
subsystem: extend the multi-tenant fair-scheduling primitive shipped for async jobs
in #194 (`d5b57e0`) to the `webhookout` delivery-claim path, which has the identical
global-FIFO starvation gap.

## Problem (verified against HEAD 27f3bbc)

`gateway/internal/webhookout/emitter.go` `processDue` (~L211-222) claims due deliveries
with a pure global FIFO — `ORDER BY d.next_attempt_at ASC LIMIT $1 FOR UPDATE OF d SKIP
LOCKED`, `claimPageSize = 50` shared across *all* customers — then delivers serially.
One tenant with a deep `pending` backlog monopolizes every tick's 50 slots and starves
all other tenants. There is no per-customer fairness and no in-flight cap. Tenancy is
reached via `endpoint_id -> webhook_endpoints.customer_id`.

## Approach

Mirror #194's opt-in design (scratch-build in webhookout idioms per CLAUDE.md — do NOT
port the jobs code). Reusable *pattern* from `jobs/store.go`: over-fetch a bounded
candidate window + an in-Go per-customer in-flight cap (`applyInflightCap` analog),
guarded on the fairness path only by a `pg_advisory_xact_lock` with a webhookout-specific
key (distinct from `jobs.fairClaimAdvisoryLockKey`) to close the cross-replica
check-then-act race; count `status='delivering'` rows grouped by customer.

The 429 backlog-admission half of #194 has NO analog here: `Emit` is an internal
fan-out INSERT…SELECT with no synchronous HTTP caller to reject. Scope is the in-flight
delivery cap only.

## Spec

```json
{
  "module": "webhookout-delivery-fairness",
  "scope": [
    "gateway/internal/webhookout/emitter.go",
    "gateway/internal/webhookout/emitter_test.go",
    "gateway/internal/config/config.go",
    "gateway/internal/config/config_test.go",
    "gateway/internal/observability/metrics.go",
    "gateway/migrations/0024_webhook_delivery_fairness.sql",
    "gateway/cmd/gateway/main.go",
    ".env.example"
  ],
  "input": "config.WebhookMaxInflightPerCustomer (env WEBHOOK_MAX_INFLIGHT_PER_CUSTOMER, default 0 = disabled)",
  "output": "per-customer in-flight cap on the webhookout delivery claim; when >0 no single customer's 'delivering' deliveries exceed the cap per claim cycle; deferred rows stay 'pending' for the next tick",
  "acceptance": [
    "default WEBHOOK_MAX_INFLIGHT_PER_CUSTOMER=0 leaves processDue's claim query byte-identical to today's single FIFO path (a 'zero disables' test asserts this)",
    "with cap>0 and one tenant holding a deep backlog, a second tenant's due delivery is claimed within one tick (two-customer fairness test)",
    "the fairness path takes a pg_advisory_xact_lock with a webhookout-specific key != jobs.fairClaimAdvisoryLockKey; a -race test asserts two concurrent emitters never jointly exceed the cap",
    "crucible_webhook_deliveries_throttled_total{reason=\"inflight_cap\"} increments by exactly the number of rows deferred",
    "config rejects negative values; migration 0024 is idempotent; go test -race ./... green in gateway/"
  ],
  "forbidden": [
    "touch gateway/proto/tool.proto or add any proto field (frozen, invariant #1)",
    "touch internal/billing, internal/auth, internal/usage, internal/quota or the billable_units<1 -> 502 check (invariant #2)",
    "change the X-Crucible-Signature contract or any Sign/channelsig path",
    "alter the webhook_deliveries status enum or the default-path FIFO ordering semantics",
    "touch stuckDeliveryAge or the crash-recovery sweep (emitter.go ~L46-48/L191-197) — reserved for a separate follow-on claim to keep this PR parallel-safe",
    "add the 429 enqueue-admission path (no synchronous caller exists — out of scope)"
  ]
}
```

Disjoint from open owner drafts #167 (ee/, monetization, dashboard pricing), #168
(.github/ CI), #188 (OSS docs). ~350-550 LOC. Reusable primitive: extends the same
fairness shape every `AsyncRoutes`/delivery-claim clone can inherit.
