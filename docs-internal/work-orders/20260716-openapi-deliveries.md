# Worker claim — document `GET /v1/webhooks/deliveries` in the served OpenAPI

**Date:** 2026-07-16
**Lane:** `worker:claim` (compact)
**Base:** origin/main @ `7efcdb4`

## The gap (proven end-to-end)

`GET /v1/webhooks/deliveries` is a shipped, paginated, customer-facing route but is **absent
from the served OpenAPI document**, so it is silently stripped from all three generated
consumer SDKs.

- Mounted: `gateway/internal/server/routes.go:287` — `r.Get("/deliveries", webhookDeliveriesHandler(d.DB))`
  under `r.Route("/v1/webhooks", ...)`. Handler is real + paginated (routes.go:1011-1084), added in #164.
- Absent from spec: `grep -c deliveries gateway/internal/openapi/openapi.go` → **0**. openapi.go
  layers exactly five self-service path-item helpers (`webhookEndpointsPathItems` :895,
  `keysPathItems` :1051, `errorsPathItems` :1161, `usageEventsPathItems` :1218, `jobsPathItems`
  :1288) — **none covers deliveries**.
- Propagates: `grep -c webhooks/deliveries clients/openapi.json` → **0**; there is no generated
  `ListWebhookDeliveries` method in `clients/{go,python,typescript}` (only the hand-written
  `VerifyWebhook` HMAC helper). The endpoint is invisible to SDK consumers.

Root cause it slipped through: the drift guard `TestV1RoutesDriftGuard` (routes_test.go:1013)
walks **POST invoke routes only** (routes_test.go:1060 `if item.Post == nil { continue }`) and
never checks layered self-service GET routes against the served doc.

## Deliverable

Add a `webhookDeliveriesPathItems()` helper to `gateway/internal/openapi/openapi.go` documenting
`GET /v1/webhooks/deliveries` (query params `page`, `per_page`; paginated response envelope with
the delivery row shape mirroring `webhookDeliveriesHandler`'s SELECT and the `Page[T]` envelope),
layer it into the served doc alongside the other five self-service helpers, then regenerate
`clients/openapi.json` + the three SDKs with `bash scripts/gen-clients.sh` (idempotent — a second
run must produce zero diff).

## Acceptance

- `gateway/internal/openapi/openapi.go` describes `GET /v1/webhooks/deliveries` with `page`/`per_page`
  query params and a paginated response schema matching the handler's returned fields.
- `clients/openapi.json` regenerated from the served doc (`go run ./gateway/cmd/spec-dump`) now
  contains the `/v1/webhooks/deliveries` path; `bash scripts/gen-clients.sh` is idempotent (re-run
  → no git diff).
- The three consumer SDKs gain a generated list-deliveries method (per the generator's normal
  GET+query handling); their existing test suites stay green.
- `go test ./gateway/internal/openapi/...` and the client drift guard pass; `go build ./...` green.

## Forbidden

- Do NOT change `webhookDeliveriesHandler` or any route wiring — the route is correct; document it.
- Do NOT touch the dashboard (`dashboard/**`) — that is the disjoint
  `10xworker:job operator-webhook-deadletter-console` PR (#192). Zero shared file.
- Do NOT add the `/v1/billing/*` or `/v1/admin/*` routes to the spec this cycle — billing is a
  deliberately drift-guard-excluded Stripe redirect surface (routes_test.go:1015,1049) and admin
  is operator-token-gated internal surface. Scope is the deliveries omission only.
- Do NOT alter `tool.proto`, billing, auth, or the existing drift-guard test's POST-route logic.
