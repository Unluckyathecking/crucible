# Work order — async_jobs retention reaper (`async-jobs-retention-reaper`)

**Lane:** `10xworker:job` · **Module:** `async-jobs-retention-reaper` · **Seeded:** 2026-07-14

## Spec

```json
{
  "module": "async-jobs-retention-reaper",
  "scope": [
    "gateway/internal/jobs/reaper.go",
    "gateway/internal/jobs/reaper_test.go",
    "gateway/migrations/0021_async_jobs_retention_index.sql",
    "gateway/internal/config/config.go",
    "gateway/cmd/gateway/main.go",
    "gateway/internal/observability/metrics.go"
  ],
  "input": "The existing *jobs.Store / *pgxpool.Pool plus a retention duration and reaper interval read from config; no new external dependency.",
  "output": "A durable background loop that periodically DELETEs async_jobs rows in terminal states (succeeded, failed) older than the configured retention, bounded per tick; a new idempotent partial index to keep that DELETE cheap; config knobs; and a reaped-rows counter metric.",
  "acceptance": [
    "New gateway/internal/jobs/reaper.go defines a Reaper type with a Run(ctx) loop on a time.Ticker, nil-safe receiver/store (mirroring usage.Flusher.Run and jobs.Executor.Run so a nil store makes Run a no-op); its DELETE filters status IN ('succeeded','failed') AND <stable timestamp column> < NOW() - retention.",
    "The per-tick DELETE is bounded (LIMIT / batched sweep, mirroring the usage flusher batch pattern) so a single sweep cannot lock async_jobs; the loop keeps sweeping within a tick only until fewer than the batch size are deleted.",
    "Wired into gateway/cmd/gateway/main.go as `go reaper.Run(rootCtx)` in the same background-loop region as flusher.Run and jobExecutor.Run (around main.go:183-193), constructed from the existing Store/pool and config.",
    "Two config fields added to gateway/internal/config/config.go with envconfig tags + defaults (JOB_RETENTION_DAYS and JOB_REAPER_INTERVAL_MS) plus negative-reject / zero-default validation matching the existing Job-knob block (config.go:205-227) and a time.Duration helper matching config.go:279-291. Behaviour is opt-in-safe: when retention <= 0 the reaper is inert (never deletes), matching the zero-config-safe stance the existing Job knobs already take.",
    "New migration gateway/migrations/0021_async_jobs_retention_index.sql adds a partial index ON async_jobs(created_at) WHERE status IN ('succeeded','failed'), using CREATE INDEX IF NOT EXISTS inside BEGIN/COMMIT so it is idempotent on every boot (invariant #8).",
    "Table-driven test gateway/internal/jobs/reaper_test.go runs against a real Postgres (no mocks, per CLAUDE.md) and proves: terminal-state rows older than retention ARE deleted; queued and running rows are NEVER deleted regardless of age; terminal rows within retention are NOT deleted; and a nil-store/nil-receiver Run is a safe no-op.",
    "go test -race ./... is green in gateway/."
  ],
  "forbidden": [
    "No change to Executor claim/retry/dead-letter logic (executor.go), the crash-recovery sweep inside Store.Claim, or any existing Store method signature.",
    "Never delete non-terminal (queued, running) rows under any condition — that would destroy in-flight/pending work and re-open the double-execution hazard the executor avoids.",
    "No edit to gateway/proto/tool.proto (frozen, invariant #1); no change to the billable_units contract (#2); no touch to any webhook/billing path.",
    "No new external dependency; stdlib + pgxpool only.",
    "Do not alter or repurpose the three existing async_jobs indexes (idx_async_jobs_queued/_customer/_stuck); add the one new partial index only.",
    "Retention must key off a stable existing column (created_at or updated_at); do NOT add a new terminal_at column or otherwise churn the async_jobs schema."
  ]
}
```

## Rationale

`async_jobs` is the one part of the recently-built async subsystem (#175–#183) with no lifecycle
termination: the only `DELETE FROM async_jobs` in the tree is test cleanup in
`gateway/internal/jobs/store_test.go`, every production mutation is INSERT/UPDATE, and terminal
`succeeded`/`failed` rows live forever — so the list (#180) and alerting (#178) surfaces degrade as
the table grows unbounded. A reaper is a self-contained, well-templated background loop (the usage
flusher and the async executor are exact precedents for the ticker/nil-safe/`go …Run(rootCtx)`
pattern) with a crisp DB-verifiable success bar and near-zero blast radius. Disjoint from the
owner-held drafts #168 (`.github/` only); it shares `main.go` and `config.go` with #167 (open-core)
but in non-overlapping regions (one background-goroutine line + two Job-family config fields),
the identical mergeable pattern every prior background loop followed — last-writer rebase on those
two files only.
