# 10X Job Spec — `transactional-event-outbox`

Decomposition seed for closing the dual-write loss window on domain-event
emission. The downstream 10X worker implements against the JSON spec in the PR
body; this file is the in-repo record.

## Problem

The framework's entire outbound-reliability investment — delivery-fairness
(#196), dead-letter replay (#192), durable-table retention reapers (#195), and
W3C trace-context capture (#200) — all operates on rows that are only ever
written *after* the state change commits, on a separate connection, best-effort.
A crash in that window permanently loses the event before it ever reaches the
`webhook_deliveries` (or `audit`) table, so none of the downstream machinery can
save it. This is the textbook dual-write problem sitting directly under a metered
product's auth/billing/jobs state transitions.

Verified against current `main` (HEAD `dc67d7b`):

- `auth/store.go` — `Revoke` commits, then `audit.Emit` (`:161` in Revoke path)
  and `emitter.Emit` (`:177`) run best-effort; `Rotate` calls `tx.Commit(ctx)`
  at `:259`, *then* `audit.Emit` at `:270` and `s.emitter.Emit` at `:289`. A
  crash between commit and emit loses the `api_key.revoked` / `api_key.rotated`
  webhook **and** the audit record.
- `jobs/executor.go` — `process` calls `e.complete(...)` (status write, `:442`)
  and only afterwards `notifySucceeded(...)` (`:472`); `fail` writes status then
  `notifyFailed` (`:520`). The in-code comments at `:456–:468` already admit the
  webhook is "silently dropped" when the notify context is near-expired.
- `webhookout/emitter.go` — `Emit` (`:176`) inserts via `e.db.Exec` (`:192`) on
  the pool; there is **no** tx-aware variant. `audit/emitter.go` `Emit` (`:77`)
  likewise does its own `db.Exec` (`:123`).
- `grep -rn 'EmitTx' gateway/internal/` → **absent**: the primitive does not
  exist yet.

One reusable `EmitTx(ctx, tx, …)` primitive on both emitters closes the loss
window for 5 webhook events (`api_key.rotated/revoked`, `subscription.updated/
deleted`, `job.succeeded/failed`) plus 3 audit sites at once, and makes
crash-safe at-least-once emission automatic for every future event and every
per-product clone. The existing background poller still delivers, so the wire
format and delivery/fairness/dead-letter loop are untouched — only *where the
row is written* changes (inside the caller's transaction instead of after it).

## Spec

```json
{
  "module": "transactional-event-outbox",
  "scope": [
    "gateway/internal/webhookout/emitter.go",
    "gateway/internal/webhookout/emitter_test.go",
    "gateway/internal/audit/emitter.go",
    "gateway/internal/audit/emitter_test.go",
    "gateway/internal/auth/store.go",
    "gateway/internal/auth/store_test.go",
    "gateway/internal/billing/webhook.go",
    "gateway/internal/billing/webhook_test.go",
    "gateway/internal/jobs/executor.go",
    "gateway/internal/jobs/store.go",
    "gateway/internal/jobs/executor_test.go",
    "docs/sprint-plans/20260719-transactional-event-outbox.md"
  ],
  "input": "A state-changing operation (key rotate/revoke, subscription upsert/delete, job terminal transition) that today commits its DB change and then emits the corresponding webhook + audit event post-commit, best-effort, on a separate connection.",
  "output": "A tx-aware emit path — webhookout.Emitter.EmitTx(ctx, tx, ...) and audit.EmitTx(ctx, tx, ...) — that inserts the outbox/audit row on the caller's existing transaction, so the state change and the event enqueue commit atomically. The unchanged background poller then delivers, giving genuine at-least-once semantics for every durable event with the wire format and delivery loop untouched.",
  "acceptance": [
    "webhookout.Emitter exposes EmitTx(ctx, tx pgx.Tx, customerID, eventType, payload) that inserts the webhook_deliveries row on the passed tx; a -race test proves that rolling back the caller's tx leaves zero delivery rows and committing leaves exactly the expected row(s).",
    "audit.EmitTx(ctx, tx pgx.Tx, Event) inserts the audit row on the caller's tx; a test proves a rolled-back caller tx leaves no audit row.",
    "auth.Store.Rotate and Revoke write the key-state change and the api_key.* webhook + audit row inside one transaction (emit no longer runs after tx.Commit at store.go:259); a test asserts that an injected commit failure persists neither the key-state change nor the delivery/audit row.",
    "jobs terminal transitions enqueue job.succeeded / job.failed inside the same transaction that writes the terminal status (replacing the post-complete notifySucceeded at executor.go:472 and post-fail notifyFailed at :520); a test proves the delivery row is present iff the status row committed.",
    "billing subscription handlers enqueue subscription.updated / subscription.deleted in the same tx as the customers.plan_id upsert; existing webhook_test.go / webhook_handler_test.go stay green.",
    "go build, go vet, and go test -race ./... are green in gateway/ (DB-gated tests skip cleanly without POSTGRES_DSN); the emitter's poller, retry/dead-letter, delivery-fairness (#196), and traceparent-capture (#200) code paths are unchanged."
  ],
  "forbidden": [
    "No change to gateway/proto/tool.proto, the billable_units>=1 -> 502 WORKER_BAD_RESPONSE trust-boundary check, the usage flusher stable batch_id logic, or Store.Revoke's Redis cache-invalidation behaviour.",
    "No change to the inbound Stripe webhook dispatch-before-record ordering in billing/webhook.go (invariant #3) — this module only makes OUTBOUND emission transactional.",
    "No reordering of the jobs executor's two-phase Complete-then-Record terminal sequence (the double-bill guard); the job.succeeded/failed enqueue joins the status-write transaction only and must not move the usage-recording step.",
    "Do NOT touch the jobs<->webhookout FIFO claim/fairness scheduling SQL (FOR UPDATE SKIP LOCKED), the wire payload, event-type strings, or the poll-based delivery loop; and make quota.exceeded (advisory, no durable state) stay best-effort — do not make it transactional.",
    "No new config knobs and no per-product edits: EmitTx is a framework default every clone inherits."
  ]
}
```

## Rationale

The two most-invested subsystems (auth/billing state changes and the async
jobs + outbound-webhook outbox) all commit-then-emit best-effort, so every
reliability feature built on top of the outbox is undermined by the event being
lost before it lands. A single reusable `EmitTx` primitive — extending the
existing emitters rather than forking them — closes that window for every event
at once and reinforces ADAPT.md's shared-audit-trail guarantee. The change is
additive (new tx-aware method + moving existing emit calls inside existing
transactions) and is expected to rebase cleanly against the aging owner drafts
(#167 EE, #168 CI, #188 OSS-readiness), which are byte-disjoint from the outbox
insert path.
