# 10xworker:job — async-jobs multi-tenant fair scheduling & per-customer admission

**Module:** `async-jobs-fair-scheduling`
**Base:** origin/main `642ed6a`
**Cycle:** 2026-07-17 sprint planner

## Problem (verified against HEAD)

The durable async-job queue claims work strictly global-FIFO with no per-tenant
concurrency ceiling and no backlog admission:

- `gateway/internal/jobs/store.go:245-253` — `Claim` selects
  `WHERE status='queued' AND next_attempt_at <= NOW() ORDER BY created_at ASC
  LIMIT $1 FOR UPDATE SKIP LOCKED`. Pure created-at ordering, no `customer_id`
  fairness term.
- `grep -rni 'per_customer|inflight|fair|starv' gateway/internal/jobs/` returns
  only unrelated executor-test identifiers — there is **no** per-customer limiting
  anywhere in the package.
- `enqueueAsync` (`gateway/internal/server/routes.go:667`) accepts every enqueue
  up to the customer's per-minute rate limit; there is no per-customer backlog cap.

The worker pool is globally bounded (`PoolSize`). In a multi-tenant metered API —
the framework's entire premise — one customer sustaining enqueues up to their rate
limit builds a `created_at`-ordered backlog that occupies every claim batch until it
drains, starving all other tenants of the shared pool. This is the one gap that
makes the async feature genuinely production-safe multi-tenant.

## Deliverable

An **opt-in**, per-customer-bounded claim scheduler + enqueue admission on the
durable job queue, plus queue-depth/fairness observability. **Every new knob
defaults to today's exact unbounded global-FIFO behaviour** (zero-value = current
behaviour), matching the framework's opt-in discipline (respcache, AsyncRoutes,
idempotency, `jobs.Reaper` are all default-off).

### Spec

```json
{
  "module": "async-jobs-fair-scheduling",
  "scope": [
    "gateway/internal/jobs/**",
    "gateway/internal/server/routes.go",
    "gateway/internal/config/config.go",
    "gateway/internal/observability/metrics.go",
    "gateway/migrations/00XX_async_jobs_fairness.sql",
    ".env.example",
    "docs-internal/**"
  ],
  "input": "The durable async-job queue claims strictly global-FIFO (store.go:245-253) with a globally-bounded PoolSize and zero per-customer concurrency/admission control, so one tenant's backlog starves the shared worker pool.",
  "output": "An opt-in per-customer-bounded claim scheduler + enqueue admission that guarantees no single customer monopolizes the shared worker pool, plus queue-depth/fairness metrics, with zero-value config preserving today's exact global-FIFO behaviour for every existing clone.",
  "acceptance": [
    "jobs.Store.Claim no longer selects purely global-FIFO when the per-customer in-flight cap is enabled: a -race test enqueuing M jobs for customer A then 1 for customer B claims B within the first pool cycle rather than only after A's M-job backlog drains (contrast store.go:245-253). With the cap at its zero-value default, Claim behaves byte-identically to today.",
    "A configurable per-customer in-flight cap is enforced at claim time; a -race unit test proves a single customer never has more than N jobs simultaneously in 'running' state regardless of queue depth, while other customers' jobs progress.",
    "enqueueAsync (routes.go:667) returns 429 with a new stable apierror code (e.g. JOB_BACKLOG_EXCEEDED) when a customer's queued+running backlog exceeds a configurable per-customer ceiling; a handler test asserts the code and that under-ceiling enqueues still return 202 with a job_id. With the ceiling at zero-value, enqueue admits unconditionally as today.",
    "New config knobs (e.g. JOB_MAX_INFLIGHT_PER_CUSTOMER, JOB_MAX_QUEUED_PER_CUSTOMER) exist in config.go with zero-value = today's unbounded behaviour, documented in .env.example; a config test asserts the defaults are unbounded/disabled.",
    "A crucible_jobs_queue_depth gauge and a per-customer-throttle counter are registered in metrics.go and added to the metrics-catalogue comment block (metrics.go:26-33).",
    "go test -race ./... green in gateway/; all pre-existing async-jobs tests pass unchanged with every new knob at its zero-value default (byte-compatible default path)."
  ],
  "forbidden": [
    "Do NOT touch gateway/proto/tool.proto or add per-product fields (invariant #1).",
    "Do NOT weaken the billable_units>=1 contract or the executor completion/retry path that guards against double-execution/double-bill (executor.go).",
    "Do NOT modify gateway/internal/webhookout/** — webhook-delivery fairness (the same FIFO gap at emitter.go:219) is a deliberate follow-on phase that reuses this primitive.",
    "Do NOT change default behaviour: every fairness/admission knob MUST default to today's unbounded global-FIFO (zero-value = current behaviour).",
    "Do NOT introduce a second execution path or bypass the FOR UPDATE SKIP LOCKED multi-replica claim semantics; the fair-claim query must remain a single safe claim under concurrent replicas.",
    "Do NOT edit config.go / .env.example fields that the concurrent durable-table-retention-reaper claim adds — append the fairness knobs in their own block; both PRs are additive-only on these shared files."
  ]
}
```

## Why this is the highest-leverage next phase

The async-jobs subsystem is the newest, highest-investment area (PRs #175–#193 built
submit/poll/requeue/release/cancel + operator + webhook surfaces), yet it ships with
zero multi-tenant safety in its claim path. Fair claim + admission is the load-bearing
primitive that makes the whole subsystem production-safe for the metered multi-tenant
use case the framework exists to serve. It compounds: the same fair-claim/admission
primitive extends directly to `webhookout`'s identical FIFO claim (`emitter.go:219`) in
a later phase, and every clone that opts a route into `AsyncRoutes` inherits multi-tenant
safety for free.

## Parallel-safety

- Disjoint from owner drafts #167 (`ee/**`), #168 (`.github/**`), #188 (OSS docs +
  `.github/**` + Python client).
- Shares only `config.go` + `.env.example` (additive-only) with the same-cycle
  `durable-table-retention-reaper` worker:claim; the two append distinct, non-adjacent
  knob blocks — see the last forbidden item. Neither may reorder existing fields.
- jobs package is ~1.3k LOC; total touched well under the 10k-LOC cap. No proto change.
