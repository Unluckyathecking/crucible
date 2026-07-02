# Work order — wire `Deps.DB` in the gateway entrypoint

**Type:** `worker:claim` (bug: features dead-in-binary)
**Scope glob:** `gateway/cmd/gateway/main.go` (+ any test needed to prove activation)

## Problem

`gateway/cmd/gateway/main.go` constructs `server.Deps{...}` with `PG: &pgPinger{pool}`
(a readiness pinger) but **never sets the `DB *pgxpool.Pool` field**. Because three
route/middleware blocks in `gateway/internal/server/routes.go` are gated on
`if d.DB != nil`, they are silently **dead-in-binary** in the shipped gateway today:

1. `idempotency.Middleware` on `/v1` — request idempotency is a documented design
   invariant (registered outer to quota so replays exit before quota reserve/refund).
   It is currently a no-op pass-through.
2. `GET /v1/webhooks/deliveries` — the customer-facing delivery log never registers.
3. `GET/POST /v1/admin/webhooks/deadletters*` — the dead-letter replay endpoints
   merged in #142 never register.

The `pool` is already fully constructed and migrated a few lines above; only the field
assignment is missing. This is a latent gap (predates #142), surfaced during the #142
merge review.

## Change

Set `DB: pool,` in the `server.Deps{...}` literal in `main.go`. That single line
activates all three blocks.

## Acceptance

- `DB: pool` is present in the `server.Deps` literal; `PG` pinger stays as-is.
- With `POSTGRES_DSN` + `REDIS_URL` set, boot the gateway and confirm the three blocks
  now register/behave: idempotency replay of a POST returns the cached response without
  double-billing; `GET /v1/webhooks/deliveries` returns 200 (not 404); operator-token
  `GET /v1/admin/webhooks/deadletters` returns 200.
- `go test -race ./...` in `gateway/` green (add a wiring/smoke assertion if one doesn't
  already exercise the `d.DB != nil` path end-to-end).
- No change to any frozen invariant (proto, `billable_units>=1`, flusher `batch_id`,
  webhook dispatch-first, `PrefixLen=24`, Go/TS hash parity, `Store.Revoke`, idempotent
  migrations). Idempotency migration 0007 already runs on boot.

## Forbidden

- No change outside `main.go` except a test proving activation.
- Do not alter the idempotency middleware ordering (it must stay outer to quota).
- Do not remove or rename the `PG` readiness pinger.

CI in this org is synthetic-fail (jobs abort ~2s, logs 404); gate on local
`go test -race ./...` + a manual boot check, not the red checks.
