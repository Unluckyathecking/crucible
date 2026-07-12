# 10X Job Spec — `async-job-completion-webhooks`

Decomposition seed for wiring the durable async-job lifecycle (#175/#176) into
the shipped `webhookout` outbound-delivery system. The downstream 10X worker
implements against the JSON spec in the PR body; this file is the in-repo record.

## Problem

PR #175 (`async-invoke`) shipped durable Postgres-backed async job execution
(`202 {job_id}` + `GET /v1/jobs/{id}` poll); #176 added retry/dead-letter. But
the async executor's terminal transitions
(`gateway/internal/jobs/executor.go`: `Complete` :346, `Fail` :361,
`DeadLetter` :402) only record usage and bump counters — a `grep` for
`Emit`/`webhookout` in `gateway/internal/jobs/**` finds only doc-comment
references, **no actual emission**. So an async customer's *only* completion
signal is polling `GET /v1/jobs/{id}`.

Meanwhile the framework already ships a generic, hardened outbound-webhook
system: `webhookout.Emitter.Emit(ctx, customerID, eventType, payload)`
(`emitter.go:108`) is already consumed by `auth/store.go`, `quota/middleware.go`,
and `billing/webhook.go`, with delivery hardened by `egress.GuardedTransport()`
(SSRF), HMAC signing, retry, dead-letter, and an operator replay surface.
`events.AllEventTypes` (`events.go:27`) is the single drift-locked event
catalogue, enforced against `openapi.webhookEventDescriptors` by a `Build()`-time
panic (`openapi.go:520`) that `TestBuild_WebhookEventCatalogueLocked` asserts.

Every serious async API (Stripe, Replicate, Deepgram, AssemblyAI) pushes a
webhook on completion; poll-only is the conspicuous product gap. The two largest
recent systems already exist and are mature — they are simply disconnected.

## Spec

```json
{
  "module": "async-job-completion-webhooks",
  "scope": [
    "gateway/internal/events/**",
    "gateway/internal/jobs/executor.go",
    "gateway/internal/jobs/executor_test.go",
    "gateway/internal/jobs/notify.go",
    "gateway/internal/jobs/notify_test.go",
    "gateway/internal/openapi/openapi.go",
    "gateway/internal/openapi/openapi_test.go",
    "gateway/cmd/gateway/main.go",
    "gateway/internal/server/routes.go",
    "docs/sprint-plans/20260712-async-job-completion-webhooks.md"
  ],
  "input": "A durable async job reaches a terminal state (succeeded, or failed via structured worker error / billable_units<1 / retry-exhausted dead-letter) inside jobs.Executor; today no outbound webhook is emitted, so poll is the only completion signal.",
  "output": "The executor emits a job.succeeded or job.failed outbound webhook through the existing webhookout.Emitter, delivered with the same retry/dead-letter/HMAC/SSRF-guarded machinery as every other event, subscribable and OpenAPI-documented automatically via the events.AllEventTypes catalogue — no per-product code and no new migration.",
  "acceptance": [
    "events.go adds job.succeeded and job.failed constants, appends both to AllEventTypes (so TestAllEventTypesMatchesConstants stays green), and defines JobSucceededPayload / JobFailedPayload carrying job_id, operation, status, and (failed only) the already-sanitized error_code — never the raw worker result body.",
    "openapi.go adds exactly two webhookEventDescriptors so buildWebhooks does not panic and TestBuild_WebhookEventCatalogueLocked (len(webhooks)==len(AllEventTypes)) stays green.",
    "jobs.Executor gains a nil-safe *webhookout.Emitter (mirroring the existing nil-safe Emit pattern); Emit is invoked at the terminal success point after Complete, at the terminal Fail point, and at the DeadLetter branch of retryOrDeadLetter — and NOT on an intermediate requeue/retry.",
    "The emitter is constructed once in main.go and injected into BOTH the router Deps and jobs.NewExecutor, resolving the current seam where the emitter is created inside NewRouter but the executor is built in main.go (no second Emitter instance).",
    "executor_test.go asserts: a succeeded job -> exactly one job.succeeded Emit with the correct payload; each terminal-failure kind -> exactly one job.failed Emit; a retried (non-terminal) attempt -> zero Emit; a nil emitter -> no panic and no Emit.",
    "webhookout subscription registration accepts job.succeeded/job.failed with no webhookout change (endpoints.go already validates via events.IsValidEventType against AllEventTypes).",
    "go build/vet green and go test -race ./... green in gateway/ (DB-gated executor tests skip cleanly without POSTGRES_DSN); no new migration and no new delivery table."
  ],
  "forbidden": [
    "Do NOT change gateway/proto/tool.proto (invariant #1).",
    "Do NOT touch the billable_units>=1 trust-boundary check or the two-write Complete-then-Record ordering in executor.process() (invariants #2, #4).",
    "Do NOT put the raw job result payload in the webhook body (leak/size risk); job_id + a poll-back is the contract, and failed carries only the sanitized error_code.",
    "Do NOT add a migration or a new delivery table — reuse webhookout's existing webhook_subscriptions + webhook_deliveries schema and Emitter.",
    "Do NOT emit on retry/requeue — only on terminal succeeded/failed transitions.",
    "Do NOT fork webhookout or add per-product event fields; extend the shared events catalogue only.",
    "Do NOT touch ee/, gateway/internal/license/, dashboard/, or .github/ (disjoint from held drafts #167 open-core and #168 CI)."
  ]
}
```

## Subunits

1. `events/events.go` — add `job.succeeded` / `job.failed` constants, append to
   `AllEventTypes`, define small `JobSucceededPayload` / `JobFailedPayload`
   structs (job_id, operation, status, sanitized error_code on failed only).
2. `openapi/openapi.go` — two `webhookEventDescriptors` entries keeping the
   drift-panic catalogue in sync.
3. `jobs/notify.go` — thin, nil-safe emit helper marshalling the payload to JSON
   and calling `webhookout.Emitter.Emit` for a given job + terminal outcome.
4. `jobs/executor.go` — invoke the helper at the two terminal transitions
   (post-`Complete` success; `Fail`) and at the `retryOrDeadLetter` dead-letter
   branch; never on requeue.
5. `cmd/gateway/main.go` + `server/routes.go` — construct the `Emitter` once and
   inject into both the router `Deps` and `jobs.NewExecutor` (single instance).
6. Tests — `executor_test.go` emission assertions; `events_test.go` /
   `openapi_test.go` stay green via the catalogue locks.

## Reusability

This composes the two largest recent framework systems (async jobs + webhookout)
into shared infrastructure: **every future clone that populates `AsyncRoutes`
gets completion webhooks for free**, with zero per-product code. No import cycle
exists (`webhookout` does not import `jobs`); no proto, migration, or billing
invariant is touched.
