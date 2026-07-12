# 10X Job Spec — `async-job-list-api`

Decomposition seed for the read-surface cap on the async-job phase
(#175–#179). The downstream 10X worker implements against the JSON spec in the
PR body; this file is the in-repo record.

## Problem

The `#175–#179` async phase built durable execution, retry/backoff/dead-letter,
the 202 `{job_id}` client shape, ops alerts, and completion webhooks — but left
customers with only **single-row polling**:

- `gateway/internal/jobs/store.go` has enqueue/claim/complete/fail/retry/
  dead-letter/idempotency, but **no `List` method**.
- `gateway/internal/server/routes.go` exposes only `GET /v1/jobs/{id}`
  (`jobsGetHandler`).
- `gateway/internal/openapi/openapi.go::jobsPathItems()` documents only
  `/v1/jobs/{id}`.
- `clients/go/client.go` and `clients/typescript/src/client.ts` have
  `GetJob`/`getJob` but **no `ListJobs`/`listJobs`**.

There is no way for a customer to enumerate job history, reconcile
queued/running/failed state, or build a dashboard job list. The enabling infra
already exists and is unused: migration `0019_async_jobs.sql:51-53` provisions
`idx_async_jobs_customer ON async_jobs(customer_id, created_at DESC)` with the
comment *"this index also serves a future list-by-customer endpoint"*, the
shared `gateway/internal/paging` package, and the exact `#164`
`GET /v1/webhooks/deliveries` list + `scripts/gen-clients.sh` precedent.

## Spec

```json
{
  "module": "async-job-list-api",
  "scope": [
    "gateway/internal/jobs/store.go",
    "gateway/internal/jobs/store_test.go",
    "gateway/internal/server/routes.go",
    "gateway/internal/server/routes_test.go",
    "gateway/internal/openapi/openapi.go",
    "clients/go/client.go",
    "clients/go/client_test.go",
    "clients/typescript/src/client.ts",
    "clients/typescript/test/**",
    "clients/openapi.json",
    "docs/sprint-plans/20260712-async-job-list-api.md"
  ],
  "input": "A durable async_jobs table with per-customer rows exposed only through single-row GET /v1/jobs/{id}, plus an already-provisioned (customer_id, created_at DESC) index and shared paging package that no jobs path uses.",
  "output": "A paginated, customer-scoped GET /v1/jobs list endpoint (status/operation filters) backed by a new jobs.Store.List, documented in OpenAPI and surfaced as ListJobs/listJobs in the generated Go + TS consumer SDKs.",
  "acceptance": [
    "jobs.Store.List exists; its SQL contains WHERE customer_id = $1 (SQL-level IDOR scope, matching Store.Get) and ORDER BY created_at DESC; a store_test asserts another customer's rows are excluded.",
    "GET /v1/jobs is registered under auth.Middleware(d.Auth) in routes.go; a routes_test asserts a 200 body shaped {\"items\":[...],\"total\":N} (the paging.Page[T] envelope) containing only the caller's jobs.",
    "Endpoint honors ?status= validated against the jobs.Status* constants; a test asserts ?status=failed returns only failed rows and an unknown status returns 400 BAD_REQUEST.",
    "Response sets Cache-Control: no-store (mirroring jobsGetHandler and the deliveries list); a test asserts the header.",
    "openapi.go::jobsPathItems() gains a \"/v1/jobs\" GET PathItem, clients/openapi.json is regenerated, and a generated ListJobs/listJobs method appears with a client test exercising it.",
    "git diff adds no gateway/migrations/00XX_*.sql file; List uses the existing idx_async_jobs_customer."
  ],
  "forbidden": [
    "No List/enumerate path that can return another customer's jobs — every query scoped by customer_id in SQL, never a post-fetch filter (upholds Store.Get's IDOR/404-not-403 contract).",
    "No change to gateway/proto/tool.proto and no per-product proto/operation fields (invariant #1); operation stays opaque free-form, exposed only as an optional read filter.",
    "No change to the billable_units >= 1 trust-boundary check in server/routes.go or to jobs.ValidBillableUnits/SanitizeWorkerError (invariant #2) — this is a read-only surface.",
    "No new migration and no drop/rename of idx_async_jobs_customer; and does not touch the #167 EE/licensing surface or #168 CI-hygiene draft."
  ]
}
```

## Subunits

1. `jobs.Store.List(ctx, customerID, filter, page)` — customer-scoped SQL over
   the existing `idx_async_jobs_customer`, status/operation filters, `paging`
   envelope. Store-level test asserts cross-customer exclusion.
2. `jobsListHandler` + `GET /v1/jobs` wiring under `auth.Middleware`, mirroring
   `jobsGetHandler` (no-store, stable error codes). Route test for the
   `{items,total}` shape, status filter, and 400 on unknown status.
3. `openapi.go::jobsPathItems()` extended with `/v1/jobs`; regenerate
   `clients/openapi.json` via `scripts/gen-clients.sh`.
4. Generated `ListJobs`/`listJobs` in the Go + TS SDKs with a client test each.

## Reusability

The list surface is the natural cap that compounds on the async phase, and every
future clone that opts a route into `AsyncRoutes` inherits it for free. Highest
leverage precisely because the enabling infra already exists — a pre-provisioned
index explicitly reserved for this, the shared `paging` package, and an exact
`#164` deliveries-list + `gen-clients.sh` precedent — making it small, low-risk,
reusable framework plumbing rather than product-specific code.
