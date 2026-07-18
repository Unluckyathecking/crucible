# 10X Job Spec — `async-trace-propagation`

Decomposition seed for propagating W3C trace context across the framework's
async outbox boundaries. The downstream 10X worker implements against the JSON
spec in the PR body; this file is the in-repo record.

## Problem

The framework shipped an OTEL tracing subsystem (`gateway/internal/tracing/`,
work-order `docs-internal/work-orders/20260605-gateway-otel-tracing.md`) that
spans only the **synchronous** HTTP → proxy path: inbound extract + proxy
inject. Every trace dies at the async boundary.

Yet the two largest recent investment areas are entirely async:

- **Async jobs** (#194 fair scheduling, #195 retention reapers, plus the
  durable executor): `jobs.Store.Enqueue` (`store.go:71`) writes a job row; a
  detached background `Executor.Run` later processes it under its own `runCtx`.
- **Outbound webhooks** (#196 delivery-fairness, #192 dead-letter replay):
  `webhookout.Emitter.Emit` (`emitter.go:155`) enqueues a delivery row; a
  background loop later POSTs it to the customer.

A `grep` for `traceparent`/`otel`/`tracer` across `gateway/internal/jobs/**` and
`gateway/internal/webhookout/**` (non-test) returns **nothing** — zero span
sites, no trace column in any migration (highest is `0024_*`), and the outbound
webhook request (`emitter.go`) sets `Content-Type`/`X-Crucible-*` headers but no
`traceparent`. So a submitted async job or webhook delivery cannot be correlated
back to the request that created it. This is the "wired-but-underused"
observability gap: a one-time **capture-at-enqueue / restore-at-execute**
primitive that every present and future async subsystem inherits for free.

## Spec

```json
{
  "module": "async-trace-propagation",
  "scope": [
    "gateway/internal/tracing/*.go",
    "gateway/internal/jobs/store.go",
    "gateway/internal/jobs/executor.go",
    "gateway/internal/jobs/executor_test.go",
    "gateway/internal/webhookout/emitter.go",
    "gateway/internal/webhookout/emitter_test.go",
    "gateway/migrations/0025_async_trace_context.sql",
    "docs/sprint-plans/20260718-async-trace-propagation.md"
  ],
  "input": "An enqueue call (jobs.Store.Enqueue, webhookout.Emitter.Emit) executing under a request that may carry an active OTEL span.",
  "output": "The outbox row persists the W3C traceparent captured at enqueue; at execution time the background engine restores it as the remote parent, wraps the work in a span (jobs.execute / webhook.deliver), and injects a well-formed traceparent header on the outbound HTTP request — producing one continuous trace from submit -> async execute -> worker/customer.",
  "acceptance": [
    "A new helper in gateway/internal/tracing serializes an active span's context to a W3C traceparent string and restores a remote-parent context from one; unit tests cover round-trip, absent input (no-op empty), and malformed input (no-op, never panics).",
    "Migration 0025_async_trace_context.sql adds a nullable traceparent TEXT column to the async_jobs and webhook_deliveries tables using ADD COLUMN IF NOT EXISTS (idempotent per invariant #8); re-running the full migration set is a no-op.",
    "jobs.Store.Enqueue persists the captured traceparent on the job row; the executor starts a span linked to that stored parent so the existing proxy.invoke span nests under it instead of orphaning.",
    "webhookout.Emitter.Emit persists the captured traceparent on the delivery row; the delivery loop starts a webhook.deliver span and sets a well-formed traceparent header on the outbound request alongside the existing X-Crucible-* headers.",
    "Disabled path: with OTEL tracing disabled the new columns stay NULL and no spans are produced (zero overhead), mirroring the existing default-off precedent.",
    "go build/vet green and go test -race ./... green in gateway/ (DB-gated executor/emitter tests skip cleanly without POSTGRES_DSN); the new migration is idempotent on re-run."
  ],
  "forbidden": [
    "No change to gateway/proto/tool.proto, the billable_units>=1 502 WORKER_BAD_RESPONSE check, Store.Revoke, the usage flusher batch_id logic, or Stripe webhook dispatch-then-record ordering.",
    "No change to the two-phase Complete-then-Record terminal ordering in the jobs executor (the double-bill guard) — only wrap it in a span, never reorder it.",
    "Do NOT touch the jobs<->webhookout FIFO claim/fairness scheduling code; this module only adds a trace-context column plus span emission, it never refactors claim or admission logic.",
    "No per-product edits and no new config knobs — reuse the existing OTEL_* configuration; this must be a framework default every clone inherits."
  ]
}
```

## Rationale

The two biggest recent subsystems (async jobs, outbound webhooks) are dark to
tracing — verified zero span sites and no trace column on disk. This adds a
single reusable framework primitive that extends the existing `tracing` package
rather than forking it, and it is byte-disjoint from the open drafts (#167 EE,
#168 CI, #188 OSS-readiness), so it lands independently.
