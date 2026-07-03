# Work order — customer usage & quota self-service API (`GET /v1/usage`)

Seed commit for the downstream 10X worker. Full spec + acceptance are in the PR body.

## Module: `customer-usage-quota-self-api`

The framework is a programmatic metered API, yet the API consumer (holding only an API
key) has no way to read its own consumption or how close it is to the quota cap.
Operators can (`GET /v1/admin/customers/{id}/usage`, merged) and dashboard humans can
(NextAuth session), but a programmatic client cannot. This adds the missing
customer-scoped, framework-level read surface every clone inherits.

- New package `gateway/internal/selfusage/` — customer-scoped read handler + a SELECT
  over `usage_events` for the current UTC billing period, plus a per-operation breakdown.
- Register `GET /v1/usage` in the **framework** block of `gateway/internal/server/routes.go`
  (after `auth.Middleware(d.Auth)`, NOT the per-product `V1Routes` edit point).
- Document it in `gateway/internal/openapi/openapi.go`.

Derives `used`/`remaining` from `quota.Tracker.Current`, cap from
`billing.PlanCache.MonthlyCap` (0 = unlimited) — the exact signals quota enforcement
uses, so what the customer sees matches what throttles them.

Read-only. No proto change, no billing/quota mutation, no operator-token path, no
`customer_id` parameter (always the authenticated caller — no IDOR). Not metered.

Gate on `go test -race ./...` in `gateway/`.
