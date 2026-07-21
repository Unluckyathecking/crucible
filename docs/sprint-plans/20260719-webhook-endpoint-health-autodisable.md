# Plan: webhook-endpoint-health-autodisable

**Sprint:** 2026-07-19
**Lane:** `10xworker:job` — reliability (outbound-webhook delivery)

## Context

The outbound-webhook delivery arc merged over #192–#201 is now mature: at-least-once
delivery with `FOR UPDATE SKIP LOCKED` + crash recovery, per-customer fair-claim
(#194/#196), retention reapers (#195), a dead-letter replay console (#192), W3C
trace propagation (#200), transactional `EmitTx` enqueue (#201), a Go↔TS event-type
parity guard (#197), and a customer delivery-log at `GET /v1/webhooks/deliveries`
(`routes.go:287`).

One reliability primitive is conspicuously missing: **a permanently-dead customer
endpoint keeps generating doomed deliveries forever.** Every event produces one
`webhook_deliveries` row that burns all retry attempts (~4.5h) before dead-lettering,
with no back-pressure and no customer signal. `webhook_endpoints` (migration
`0012_outbound_webhooks.sql`) has `active BOOLEAN` and `secret BYTEA` but **no health
/ failure-accounting columns**; `config.go:134-149` has retention and
`WebhookMaxInflightPerCustomer` knobs but **no failure/disable threshold**; the
`resilience.Breaker` is wired **only** to `proxy/client.go` (gateway→worker), never to
the outbound delivery path; and `routes.go:287-292` registers
create/list/delete/patch/rotate-secret but **no enable route** and **no health
surface**.

This module adds the standard provider primitive (Stripe/Svix auto-disable + notify):
after a configurable number of consecutive dead-letters an endpoint auto-disables,
surfaces `disabled_at`/`disabled_reason` to the customer, fans an `endpoint.disabled`
event to the customer's *other* active endpoints, and can be customer-re-enabled. It
is pure composable framework infra every clone inherits, and it caps unbounded
`webhook_deliveries` growth + wasted worker ticks fleet-wide.

## Scope (all globs, ~2,000 LOC incl. tests — confirmed < 10k)

- `gateway/migrations/0026_webhook_endpoint_health.sql` (new, ~25)
- `gateway/internal/webhookout/health.go` + `health_test.go` (new, ~350)
- `gateway/internal/webhookout/emitter.go` (surgical: reset counter in `markDelivered`;
  bump + maybe-disable in the terminal dead-letter path — ~40 changed)
- `gateway/internal/webhookout/endpoints.go` + `endpoints_http.go` (expose
  `disabled_at`/`disabled_reason` on the read model; add re-enable handler — ~150)
- `gateway/internal/events/events.go` (add `endpoint.disabled` type + payload — ~15)
- `dashboard/lib/db.ts` (mirror `endpoint.disabled` into `WEBHOOK_EVENT_TYPES` — ~1)
- `gateway/internal/openapi/openapi.go` (event descriptor + re-enable route descriptor — ~30)
- `gateway/internal/server/routes.go` (register `POST /v1/webhooks/endpoints/{id}/enable` — ~3)
- `gateway/internal/config/config.go` (one threshold knob + validation — ~15)
- `gateway/internal/observability/metrics.go` (one counter — ~10)

## Machine-actionable spec

```json
{
  "module": "webhook-endpoint-health-autodisable",
  "scope": [
    "gateway/migrations/0026_webhook_endpoint_health.sql",
    "gateway/internal/webhookout/health.go",
    "gateway/internal/webhookout/health_test.go",
    "gateway/internal/webhookout/emitter.go",
    "gateway/internal/webhookout/endpoints.go",
    "gateway/internal/webhookout/endpoints_http.go",
    "gateway/internal/events/events.go",
    "gateway/internal/openapi/openapi.go",
    "gateway/internal/server/routes.go",
    "gateway/internal/config/config.go",
    "gateway/internal/observability/metrics.go",
    "dashboard/lib/db.ts"
  ],
  "input": "Per-endpoint delivery-health counter persisted on webhook_endpoints, updated by the existing delivery worker's terminal-outcome calls.",
  "output": "Chronically-failing endpoints auto-disable after a configurable consecutive-dead-letter threshold, surface disabled_at/disabled_reason to the customer, fan an endpoint.disabled event to the customer's other active endpoints, and can be customer-re-enabled.",
  "acceptance": [
    "New migration 0026 adds consecutive_failures, disabled_at, disabled_reason via ADD COLUMN IF NOT EXISTS; no version-tracking table (invariant #8); runs in lexical order.",
    "markDelivered resets consecutive_failures=0; the terminal dead-letter path increments it and, on crossing the threshold, sets active=FALSE + disabled_reason='delivery_failures' in the SAME UPDATE — a test drives N dead-letters and asserts the row flips.",
    "New config knob WEBHOOK_ENDPOINT_FAILURE_THRESHOLD defaults to 0 (=disabled); a test proves default-0 leaves delivery behaviour byte-identical (no auto-disable), matching the WebhookMaxInflightPerCustomer opt-in precedent.",
    "events.AllEventTypes AND dashboard/lib/db.ts WEBHOOK_EVENT_TYPES both gain endpoint.disabled so events parity_test.go stays green and openapi.Build() constructs without panic.",
    "POST /v1/webhooks/endpoints/{id}/enable re-enables an auto-disabled endpoint, rejects a customer-deleted one (auto-disable vs soft-delete distinguished though both use active=FALSE), and returns IDOR-safe 404 for another customer's id.",
    "Prometheus crucible_webhook_endpoints_disabled_total increments on auto-disable; go test -race ./... green in gateway/."
  ],
  "forbidden": [
    "Do NOT reintroduce a second delivery worker or fork claimDue's fair-claim / FOR UPDATE SKIP LOCKED path (#194/#196) — auto-disable rides the existing active=TRUE gating in Emit's INSERT…SELECT and claimDue; setting active=FALSE already stops delivery, no query rewrite.",
    "Do NOT conflate auto-disable with customer DeleteEndpoint (both set active=FALSE); re-enable must not revive a deleted endpoint.",
    "Do NOT break event-type parity across events.AllEventTypes / dashboard/lib/db.ts / openapi descriptors (#197).",
    "Migration must be idempotent, lexical-order, no version table (invariant #8).",
    "Do NOT touch billable_units trust boundary (#2), proto/tool.proto (#1), usage/flusher.go batch_id (#4), the api-key hash mirror (#5), or PrefixLen=24 (#6). Store.Revoke/Redis 60s cache semantics (#7) do NOT apply here — webhook_endpoints has no hot cache; the worker reads active fresh each tick.",
    "The endpoint.disabled fan-out must self-exclude the just-disabled endpoint (no delivery to a disabled URL; no re-entrancy)."
  ]
}
```

## Gap evidence (verified on `origin/main`, HEAD `5f17abc`)

- `gateway/migrations/0012_outbound_webhooks.sql`: `webhook_endpoints` has `active` and
  `secret` but no health/failure columns.
- `config.go:134-149`: retention, reaper interval, `WebhookMaxInflightPerCustomer` — no
  failure/disable threshold.
- `grep -rin "disabled_at|consecutive_fail|EnableEndpoint|auto.*disable"
  gateway/internal --include=*.go | grep -v _test` → zero hits in webhookout.
- `grep -rln "Breaker" … | grep -v _test` → `assembly.go, config.go, metrics.go,
  proxy/client.go` only — the breaker never guards the outbound delivery path.
- `emitter.go` `markDeadLetter` / `markDelivered` update only
  `status/attempts/last_response_code` — no per-endpoint health accounting.
- `routes.go:287-292`: create/list/delete/patch/rotate-secret — no enable route, no
  health surface.

## Not this module (deferred)

- **webhook-secret-rotation-grace-window** (dual-secret rolling rotation): real gap —
  `RotateEndpointSecret` (`endpoints.go:266`) hard-`UPDATE`s the single
  `secret BYTEA NOT NULL`, instantly invalidating the old secret during the customer's
  redeploy window. Cleaner (~1,200 LOC) but a UX/correctness feature, lower operational
  leverage than capping doomed-delivery generation. Sequence after this.
