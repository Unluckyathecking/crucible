# Work order — async-job-cancel (`async-job-cancel`)

**Lane:** `10xworker:job` · **Module:** `async-job-cancel` · **Seeded:** 2026-07-16 (sprint planner)

## Context

The async-job subsystem is otherwise lifecycle- and operability-complete: durable execution
(#175), retry/backoff/dead-letter (#176), completion webhooks (#179), customer list/get
(#180), retention reaper (#184), operator requeue/release + 409 status-guard (#187/#189),
deadlettered metric (#190), and the poll SDK trio (#185). The single standard lifecycle verb
still missing is **cancellation**.

Verified against `origin/main` HEAD `c3639d6`:

- The status enum is frozen at four values:
  `status TEXT NOT NULL DEFAULT 'queued' CHECK (status IN ('queued','running','succeeded','failed'))`
  (`gateway/migrations/0019_async_jobs.sql:20`). There is no `cancelled` state and dead-letter
  reuses terminal `failed`.
- A `cancel`-scoped grep across `gateway/internal/jobs`, `gateway/internal/server/routes.go`,
  `gateway/migrations`, and `clients/` returns **zero** job-cancel path — every hit is Go
  `context.WithCancel` in tests, not a job verb. Customer routes are `GET /v1/jobs` and
  `GET /v1/jobs/{id}` only; admin has requeue/release only (`routes.go:337,346,431`).
- The client SDKs hardcode the terminal set as only `succeeded`/`failed`
  (`clients/go/jobs.go:17-20` and the TS/Python equivalents), so a cancel state would need to
  land there too.

The async lane exists precisely for long/expensive worker calls (OCR, transcription,
render/scrape/inference pipelines — see the `AsyncRoutes` comment in `routes_table.go`), where
cancelling a wrong or costly **queued** submission is a real operational need, not gold-plating.

## Spec

```json
{
  "module": "async-job-cancel",
  "scope": [
    "gateway/migrations/0022_async_jobs_cancel.sql",
    "gateway/internal/jobs/jobs.go",
    "gateway/internal/jobs/store.go",
    "gateway/internal/jobs/store_test.go",
    "gateway/internal/jobs/reaper.go",
    "gateway/internal/jobs/reaper_test.go",
    "gateway/internal/server/routes.go",
    "gateway/internal/server/routes_test.go",
    "gateway/internal/openapi/openapi.go",
    "gateway/internal/openapi/openapi_test.go",
    "clients/openapi.json",
    "clients/go/jobs.go", "clients/go/jobs_test.go",
    "clients/python/crucible_client/jobs.py", "clients/python/tests/test_jobs.py",
    "clients/typescript/src/jobs.ts", "clients/typescript/test/jobs.test.ts"
  ],
  "input": "HEAD c3639d6. async_jobs.status CHECK is ('queued','running','succeeded','failed') (migrations/0019:20); customer routes are GET /v1/jobs and GET /v1/jobs/{id} only (routes.go:337,346); admin has requeue/release only (routes.go:431); no cancel path exists anywhere.",
  "output": "A queued async job can be cancelled by its owner via POST /v1/jobs/{id}/cancel: a terminal 'cancelled' state, IDOR-safe customer scoping, and a 409 status-guard for non-queued jobs mirroring the requeue guard (#189). Documented in the served OpenAPI and mirrored across all three client SDKs.",
  "acceptance": [
    "Migration 0022 extends async_jobs.status to include 'cancelled' idempotently (DROP CONSTRAINT IF EXISTS then ADD, re-runnable every boot per invariant #8 — no version table).",
    "jobs.StatusCancelled const added; Store.CancelQueued(ctx,id,customerID) issues UPDATE ... WHERE id=$1 AND customer_id=$2 AND status='queued' — the grep shows BOTH the customer_id scope and the status='queued' guard in the SQL.",
    "POST /v1/jobs/{id}/cancel registered under auth.Middleware (customer path, NOT /v1/admin); returns 200/204 on queued->cancelled, 409 with a stable code when status != queued, 404 when absent/not-owned. A table test covers all three outcomes.",
    "The claim scan's queued-only WHERE already excludes cancelled rows; a test proves a cancelled job is never claimed/executed and bills 0 units.",
    "reaper terminal-status set includes 'cancelled' (grep reaper.go WHERE clause) with a test.",
    "openapi.Handler documents POST /v1/jobs/{id}/cancel and adds 'cancelled' to the jobs status enum; spec-dump output == clients/openapi.json (drift guard green).",
    "Each client SDK gains a CancelJob method + a JobStatusCancelled terminal const; WaitForJob/poll companions return on 'cancelled'; all three SDK test suites green.",
    "go test -race ./... green in gateway/."
  ],
  "forbidden": [
    "No cooperative cancel of RUNNING jobs this cycle — return 409; do NOT add a cancel-request flag polled by the executor.",
    "No changes to tool.proto, billable_units enforcement, webhook dispatch ordering, flusher batch_id, auth hash, or PrefixLen.",
    "Do not touch ee/, .github/, or OSS-policy docs (owner drafts #167/#168/#188).",
    "Do not add a job.cancelled outbound webhook event this cycle (leave events.AllEventTypes untouched) — that is a follow-up."
  ]
}
```

## Rationale

Cancellation is the one missing standard lifecycle verb in an otherwise complete async
subsystem — status is frozen at four values (`migrations/0019:20`) with no cancel path anywhere,
yet the lane targets long/expensive worker calls where cancelling a queued submission is a real
need. Scoping cancellation to **queued-only with a 409 guard** reuses the exact
requeue-status-guard pattern shipped in #189 (`routes.go:433`), sidesteps the hard
cooperative-cancel-of-running problem, and compounds across every async-opted clone. It is
cross-cutting infra (migration + store + route + OpenAPI + three SDKs) but bounded well under
10k LOC.

**Parallel-safety vs open drafts:** Disjoint. #167 lives in `ee/`, `gateway/internal/license/`,
operator tokens, SSO, `dashboard/app/page.tsx`; #168 is `.github/` CI only; #188 touches
`clients/python/crucible_client/errors.py` (a different file than `jobs.py`) + OSS-policy docs.
The only shared file is the generated `clients/openapi.json`; since #188 adds no routes its
regeneration is a no-op, so whichever merges second re-runs `spec-dump` cleanly — a mechanical,
not semantic, reconcile.
