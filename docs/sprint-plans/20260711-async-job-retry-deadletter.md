# 10X Job Spec — `async-job-retry-deadletter`

Decomposition seed for the async-job retry/backoff/dead-letter module. The
downstream 10X worker implements against the JSON spec in the PR body; this file
is the in-repo record.

## Problem

PR #175 (`async-invoke`) shipped durable Postgres-backed async job execution:
an opt-in route returns `202 {job_id}` and a bounded `jobs.Executor` invokes the
worker in the background. But `jobs.Executor.fail()` marks a job **permanently
`failed` on the first worker error** — including a transient
`WORKER_UNREACHABLE` / proxy transport error caused by a worker restart or a
brief network blip. There is no retry.

This makes the *paid, long-running* async path (OCR, transcription, render,
LLM/inference — exactly the product classes #175 was built to unlock) **less
reliable than both** (a) the synchronous path, where a caller can retry a 502
themselves, and (b) the `webhookout` delivery path, which already ships
retry-with-backoff + an operator dead-letter replay surface
(`0016_deadletter_index`, PR #142). A single worker restart permanently loses an
already-accepted job.

## Spec

```json
{
  "module": "async-job-retry-deadletter",
  "scope": [
    "gateway/internal/jobs/**",
    "gateway/migrations/0020_async_jobs_retry.sql",
    "gateway/internal/observability/metrics.go",
    "gateway/internal/config/config.go"
  ],
  "input": "An async job whose worker call fails transiently (WORKER_UNREACHABLE / proxy transport error) is today marked terminal 'failed' on the first error by jobs.Executor.fail(), with no retry — unlike webhookout, which already backs off + dead-letters.",
  "output": "The executor retries retryable failures with bounded exponential backoff (requeue with a next_attempt_at) up to a configured max_attempts, then dead-letters to terminal 'failed'; deterministic failures (worker structured business error, billable_units<1 contract violation) still fail immediately without wasting retries.",
  "acceptance": [
    "Migration 0020 adds attempts/max_attempts/next_attempt_at via ALTER TABLE ... ADD COLUMN IF NOT EXISTS; idempotent and re-runnable on every boot (invariant #8), mirroring 0004_usage_batches.sql's batch_id add.",
    "Store.Claim's queued-scan skips rows whose next_attempt_at is in the future; oldest-eligible-first ordering preserved; FOR UPDATE SKIP LOCKED semantics unchanged.",
    "Only WORKER_UNREACHABLE / transport errors are retried; a worker structured business error and a billable_units<1 contract violation go straight to terminal 'failed' with attempts unchanged.",
    "A job that fails transiently twice then succeeds records usage exactly once (billing-safe: Record only runs after Complete) — asserted by a go test -race test against real Postgres.",
    "JOB_MAX_ATTEMPTS + JOB_RETRY_BACKOFF envconfig knobs with conservative defaults (e.g. 3 attempts); with AsyncRoutes empty the framework default is byte-unaffected.",
    "New crucible_jobs_retried_total{operation} counter; go test -race ./... green in gateway/."
  ],
  "forbidden": [
    "Do not add a customer-visible new status or touch routes.go / openapi.go / the GET /v1/jobs/{id} response shape — after exhaustion the terminal status stays 'failed' (that shape is owned by the disjoint spec-dump/gen-clients claim).",
    "Do not change the billable_units>=1 trust boundary or the two-write Complete-then-Record ordering (invariants #2, #4).",
    "Do not retry deterministic worker errors (wastes worker calls; never billed anyway).",
    "Do not touch gateway/cmd/spec-dump, scripts/gen-clients.sh, ee/, or .github/ (disjoint from the async-202 claim, PR #167, and PR #168)."
  ]
}
```

## Subunits

1. Migration `0020_async_jobs_retry.sql` — `ADD COLUMN IF NOT EXISTS attempts INT NOT NULL DEFAULT 0`, `max_attempts INT`, `next_attempt_at TIMESTAMPTZ`. Verified next sequence (existing: 0001–0019).
2. `jobs/store.go` — `Claim` filters `next_attempt_at <= now()`; a `Requeue(attempts++, next_attempt_at)` path distinct from terminal `Fail`.
3. `jobs/executor.go` — classify the worker error at the `process()` call site: retryable transport error → `Requeue` with exponential backoff until `attempts >= max_attempts`, then dead-letter to terminal `failed`; deterministic error → immediate terminal `failed`.
4. `config.go` — `JOB_MAX_ATTEMPTS`, `JOB_RETRY_BACKOFF` envconfig with conservative defaults.
5. `observability/metrics.go` — `crucible_jobs_retried_total{operation}` counter.

## Disjointness

Scope is `jobs/**` + a new `0020` migration + `metrics.go` + `config.go`. No
overlap with open drafts #167 (`ee/`) or #168 (`.github/`, `setup-go`,
`kimi-review.yml`), and file-disjoint from the concurrent spec-dump/gen-clients
async-202 `worker:claim` (which lives in `cmd/spec-dump` + `scripts/` + a new
`server` helper file). Estimated ~500–900 LOC incl. tests.
