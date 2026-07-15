# Work order — operator-jobs-console (`operator-jobs-console`)

**Lane:** `10xworker:job` · **Module:** `operator-jobs-console` · **Seeded:** 2026-07-15

## Context

The async-jobs subsystem (PRs #175–#186) is lifecycle-complete on the customer side: durable
execution, retry/backoff/dead-letter, completion webhooks, paginated customer-scoped `GET /v1/jobs`,
usage export, poll SDKs, and a retention reaper. But two operator-facing recovery levers that the
subsystem deliberately shipped are **built, tested, and completely unreachable** — no HTTP route or
UI exposes them:

- `jobs.Store.Requeue(ctx, id)` — `gateway/internal/jobs/store.go:384`. Doc comment
  (store.go:382-383) reserves it "as an operator-facing primitive … future operator tooling."
- `jobs.Store.ReleaseClaimed(ctx, instanceID)` — `gateway/internal/jobs/store.go:409`, scoped to a
  `claimed_by` instance UUID (store.go:416). Doc comment (store.go:399-408) documents it as the
  recovery action for jobs orphaned by a crashed executor instance. A planted seam test
  `TestStore_ReleaseClaimed_UsableAsOperatorPrimitive` (executor_test.go:389) marks it as a
  next-module hook. Grep confirms **zero non-test production callers** of either.

Meanwhile the operator console pattern already exists and is read-only: `/v1/admin/{customers,
customers/{id},customers/{id}/usage,audit,plans}` (routes.go:352-359), gated by the existing static
`OPERATOR_TOKEN` community path (`operator.Middleware`, config.go:58 + middleware.go:21-45), with a
matching dashboard at `dashboard/app/operator/{customers,audit,plans}/**`. There is **no
`/v1/admin/jobs` route and no `dashboard/app/operator/jobs` page** (confirmed absent). The operator
`Store` is SELECT-only by package invariant (operator/store.go:1-4); this module keeps that invariant
by putting reads/writes in the `jobs.Store`, not `operator.Store`.

This module completes the async subsystem's operability story — the one thing the "airtight" async
work left unreachable — by extending an established, reusable console pattern rather than a one-off.

## Spec

```json
{
  "module": "operator-jobs-console",
  "scope": [
    "gateway/internal/operator/jobs_handlers.go",
    "gateway/internal/operator/jobs_handlers_test.go",
    "gateway/internal/jobs/adminstore.go",
    "gateway/internal/jobs/adminstore_test.go",
    "gateway/internal/observability/metrics.go",
    "gateway/internal/server/routes.go",
    "dashboard/app/operator/jobs/**",
    "dashboard/app/operator/_components/operator-nav.tsx",
    "dashboard/lib/operator/client.ts"
  ],
  "input": "an operator holding the existing static OPERATOR_TOKEN needs cross-customer visibility into the async_jobs queue plus two safe recovery actions (requeue a job; release jobs claimed by a dead instance)",
  "output": "a /v1/admin/jobs read+control surface (cross-customer list/inspect, per-job requeue, per-instance force-release) wired into the existing OPERATOR_TOKEN-gated /v1/admin subrouter and exposing the already-built jobs.Store.Requeue / ReleaseClaimed primitives, plus a matching dashboard operator/jobs page",
  "acceptance": [
    "GET /v1/admin/jobs (with ?status= allow-listed to the existing job statuses) returns a cross-customer, paginated job view including claimed_by / claimed_at for running rows; is OPERATOR_TOKEN-gated (401 without a valid bearer); and sets Cache-Control: no-store, mirroring the customer job read endpoints.",
    "GET /v1/admin/jobs/{id} returns a single job across any customer (unscoped read), 404 on unknown id; reads go through a NEW jobs.Store.AdminGet/AdminList (the existing List/Get are customer_id-scoped at store.go:114/152 and cannot serve a cross-customer view).",
    "POST /v1/admin/jobs/{id}/requeue flips a claimed/failed/dead-lettered row to queued (claimed_at=NULL, claimed_by=NULL) via jobs.Store.Requeue; returns 404 for an unknown id.",
    "POST /v1/admin/jobs/release with an instance_id body returns {released: N} via jobs.Store.ReleaseClaimed, with a test proving it never touches another instance's rows (mirror the existing store_test.go ScopedToInstance coverage).",
    "Two additive Prometheus counters crucible_jobs_requeued_total and crucible_jobs_released_total increment on the respective actions (append-only to observability/metrics.go; existing series unchanged).",
    "Dashboard /operator/jobs renders the cross-customer list behind the existing operator session guard and adds one 'Jobs' link to operator-nav.tsx; the requeue/release actions state their in-flight-safety caveat in the UI.",
    "go build / go vet / go test -race ./... green across the gateway; pnpm build (type-check) green in dashboard/."
  ],
  "forbidden": [
    "Do NOT touch ee/**, gateway/internal/license/**, or gateway/cmd/licensegen/** (PR #167 open-core territory).",
    "Do NOT touch operator/middleware.go or operator/store.go, and do NOT add any operator-token CRUD / multi-token / license-gated auth code (operator/tokens_* is PR #167's EE 'operator multi-token' surface) — reuse the existing static operator.Middleware(d.OperatorToken) unchanged.",
    "Keep operator.Store SELECT-only (package invariant operator/store.go:1-4): all job reads/writes live in jobs.Store, not operator.Store.",
    "Do NOT touch the customer-scoped jobs.Store.List / Get, the frozen gateway/proto/tool.proto, the billable_units enforcement, or billing/** / auth/** / webhookout/**.",
    "No new migration: the async_jobs table and its claimed_by/claimed_at columns already exist on main."
  ]
}
```

## Rationale

Two operator recovery primitives (`Store.Requeue`, `Store.ReleaseClaimed`) were shipped, tested, and
explicitly reserved for "future operator tooling," yet remain reachable by no route or UI — the async
subsystem's only unfinished operability thread. Exposing them behind the existing `OPERATOR_TOKEN`
console pattern (not a new auth path) turns a dead primitive into an operational lever and extends a
reusable, already-blessed admin surface. Scoped to new files plus a ~5-line insertion into the shared
`/v1/admin` route block; ~1,500–2,300 LOC, well under the 10k cap.

## Parallel-safety

- **vs #168 (CI, `.github/**` + `.github/actions/setup-go`):** fully disjoint.
- **vs #167 (open-core, owner-held draft):** all new Go lands in NEW files
  (`operator/jobs_handlers.go`, `jobs/adminstore.go`) — file-disjoint from #167's
  `operator/middleware.go`/`store.go`/`tokens_*`. The only textual contact is the shared
  `r.Route("/v1/admin", …)` closure in `routes.go`: #167 inserts `/tokens` routes, this inserts
  `/jobs` routes — a small, mechanical, insertion-only merge, not an edit to the same lines. This
  module adds no token/license/auth code, so it does not collide with #167's auth logic. No shared
  migration. Whichever lands first, the other rebases its `routes.go` block insertion.
