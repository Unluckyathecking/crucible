# `operator-console` — Operator/admin dashboard surface over the read API (10x decomposition)

Seed work order for the primary 10x worker. The authoritative spec is the JSON block in
the PR body; this doc carries the rationale and invariant context.

## Why now

The operator read API (`gateway/internal/operator/` + `/v1/admin/*`, merged #137) shipped
fully wired — SELECT-only `Store`, operator-token middleware, paginated envelopes — but has
**zero consumers**. Its entire value is locked until a UI reads it. The blackboard flagged
the operator **console** as the deliberate next phase over the merged read API. This is the
highest-leverage next step and, because the admin API is framework-level and identical per
clone, the console compounds across **every** Crucible clone.

Confirmed unbuilt: the `dashboard/` Next.js app is customer-only — it reads Postgres
directly via `lib/db.ts`, has no operator role, no gateway-HTTP client, and no `/operator`
or `/admin` pages.

## Module boundary

A new App Router segment `dashboard/app/operator/**`, a typed HTTP client
`dashboard/lib/operator/**` for the five `/v1/admin/*` endpoints, and a server-only proxy
`dashboard/app/api/operator/**` that injects `OPERATOR_TOKEN` from env so the token never
reaches the browser. Two small additive edits to shared files (`middleware.ts` matcher,
`lib/env.ts`). No gateway/Go changes — UI-only over the frozen read API.

## Why read-only / why go through the gateway

- The operator surface mirrors the SELECT-only guarantee of `gateway/internal/operator/`.
  No POST/PUT/PATCH/DELETE against customers, keys, plans, or billing. The write/action
  tier is deliberately out of scope (it collides with `Store.Revoke` cache-coherence and
  documented plan-edit stale-cache hazards).
- The console reaches data **only** through `/v1/admin/*`, never Postgres directly. That
  preserves the read-only + separate-operator-auth boundary the gateway enforces; the
  customer dashboard's direct-`lib/db.ts` pattern must not leak into the operator pages.
- `OPERATOR_TOKEN` is read only in server modules (route handlers / server components) and
  is never exposed to the client (no `NEXT_PUBLIC_`, never serialized into a payload). The
  constant-time compare stays in the gateway.

## Surface (read-only)

Customer list (plan filter + pagination), customer drill-down with monthly
usage-by-operation breakdown, filterable audit-log viewer, and a plans table — each backed
by the existing `/v1/admin/customers`, `/v1/admin/customers/{id}`,
`/v1/admin/customers/{id}/usage`, `/v1/admin/audit`, `/v1/admin/plans` endpoints. The typed
client + response validators are the reusable seam clones extend with product-specific
operator views.

## Acceptance & forbidden

See the JSON spec in the PR body. Gate on `pnpm build` (type-checks the whole tree) and
`pnpm test` (new vitest suites) green. All Crucible invariants (frozen proto,
`billable_units` floor, webhook ordering, flusher `batch_id`, Go/TS hash parity,
`PrefixLen=24`, `Store.Revoke`, idempotent migrations) are untouched by a read-only UI phase.
