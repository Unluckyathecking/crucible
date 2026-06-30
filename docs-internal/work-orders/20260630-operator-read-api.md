# `operator-read-api` — Read-only Operator/Admin API tier (10x decomposition)

Seed work order for the primary 10x worker. The authoritative spec is the JSON block in
the PR body; this doc carries the rationale and invariant context.

## Why now

The framework gives **customers** self-serve visibility (dashboard: keys, usage, billing,
audit) but gives **operators** nothing — system-wide health, cross-customer usage trends,
and the full audit trail are only reachable via `psql`. Every metered-API clone eventually
needs an operator surface. This is the next deliberate phase the blackboard flagged
(operator/admin tier), and it is a reusable module that compounds across **every** clone
since crucible is the template.

Confirmed unbuilt: no `/v1/admin/*` routes exist; there is no operator-token path distinct
from customer API keys; there is no non-customer-scoped query package.

## Module boundary

A new `gateway/internal/operator/` package (a SELECT-only `Store` + handlers), an
operator-token middleware in the auth seam, and an `/v1/admin/*` subrouter registered in
`routes.go` — in the **framework** block, never the per-product edit block. Read-only by
construction.

## Why read-only / why a separate auth path

- **Read-only** keeps the module entirely clear of the billing/auth trust boundaries: no
  mutation of customer, plan, key, or billing state; no access to `api_keys.hash`.
- **Separate operator token** (`OPERATOR_TOKEN`, compared with `subtle.ConstantTimeCompare`)
  keeps the customer API-key path (`auth.Middleware`, hash parity, `PrefixLen`) completely
  untouched — operators and customers authenticate on different paths.

## Surface (GET only)

`/v1/admin/customers`, `/v1/admin/customers/{id}`, `/v1/admin/customers/{id}/usage`,
`/v1/admin/audit`, `/v1/admin/plans`. Paginated envelopes; usage broken down by operation.
The `Store` interface is the reusable seam clones extend with product-specific operator
views.

## Acceptance & forbidden

See the JSON spec in the PR body. Invariants respected: frozen proto (#1),
`billable_units>=1` trust boundary (#2), hash parity + `PrefixLen` (#5/#6), idempotent
additive migrations (#8 — any operator-token table is `0015+`). `go test -race ./...` green
against real Postgres (no mocks). Estimated ~2,000 LOC, under the 10k ceiling.
