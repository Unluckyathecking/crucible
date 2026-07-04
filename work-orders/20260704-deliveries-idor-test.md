# worker:claim — IDOR/customer-scoping coverage for `GET /v1/webhooks/deliveries`

**Target:** `gateway/test/scenarios/scenarios_test.go` (uses the existing DB-backed
harness at `gateway/test/harness/harness.go`, which already builds a router and can seed
`webhook_endpoints`). Source unchanged — test-only.

## The gap

`webhookDeliveriesHandler`'s **sole** tenant-isolation guard is
`WHERE we.customer_id = $1` (in `gateway/internal/server/routes.go`). A grep of the whole
tree finds **zero** test references to `webhookDeliveriesHandler` or
`/v1/webhooks/deliveries` — the handler is entirely untested. An IDOR regression here would
silently leak one customer's webhook delivery metadata (endpoint URLs, event ids, response
codes) to another customer.

## Change

Add a scenario that:
1. Seeds two customers, each with a webhook endpoint + a `webhook_deliveries` row.
2. Asserts customer A's API key on `GET /v1/webhooks/deliveries` returns only A's rows and
   **never** B's `event_id` / URL / response-code.
3. Asserts a request with no/invalid key → `401`.

## Acceptance

- New scenario present in `scenarios_test.go` exercising `GET /v1/webhooks/deliveries`
  across two seeded customers; A never sees B's rows; unauthenticated → 401.
- Runs green under the DB-backed harness (`go test -race ./test/...` with real PG+Redis).

## Constraints

Test-only; no production source change. Disjoint from open #143 (main.go DB wiring) and
from the customer-webhook-endpoint-management primary (which adds *endpoint* CRUD and
explicitly excludes this pre-existing *deliveries* read handler). Do not modify the handler
or its query.
