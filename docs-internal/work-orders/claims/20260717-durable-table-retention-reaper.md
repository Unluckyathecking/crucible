# worker:claim — durable-table retention reaper (idempotency_keys + webhook_deliveries)

**Base:** origin/main `642ed6a`
**Cycle:** 2026-07-17 sprint planner

## Directive

Add a bounded background retention reaper for the two durable queue-backed tables that
lack one, following the exact `jobs.Reaper` pattern already in the tree
(`gateway/cmd/gateway/main.go:181-188`):

1. **`idempotency_keys`** — delete rows older than a configurable retention window.
2. **`webhook_deliveries`** — delete terminal `status='delivered'` rows older than a
   configurable window. **MUST NOT** delete `dead_letter` rows — operators replay those
   via the #192 dead-letter replay console (`gateway/internal/webhookout/replay.go`).

Both default **opt-in / inert** (zero-value retention = no deletion), matching
`jobs.Reaper` which is itself a no-op until `JOB_RETENTION_DAYS` is set.

## Why it is real (verified against HEAD)

- `idempotency_keys` rows are deleted **only lazily**, when the *same*
  `(customer_id, idempotency_key)` is queried again and found expired
  (`gateway/internal/idempotency/store.go:105-112`). Idempotency keys are
  unique-per-request, so the vast majority are never re-queried and their rows are
  **never** deleted → unbounded growth at request volume, on a framework built for
  "high-volume." The standalone `idx_idempotency_keys_created_at`
  (`gateway/migrations/0007_idempotency_keys.sql:26`) is used by no query today — a
  strong signal a retention sweep was intended but never built.
- `webhook_deliveries` rows set to `status='delivered'`
  (`gateway/internal/webhookout/emitter.go:315-323`) are kept forever; the only
  `DELETE FROM webhook_deliveries` is `deleteUnsubscribedRow`
  (`emitter.go:388`), a narrow subscription-narrowing cleanup — no retention sweep. The
  delivery log is surfaced at `routes.go:1048-1060` and grows unbounded.
- `async_jobs` is the **only** queue-backed table that got a reaper (`jobs.Reaper`,
  `jobs/reaper.go`, wired at `main.go:181-188`). These two tables are the same class of
  gap.

## Target files

- `gateway/internal/idempotency/` — new reaper + test (mirror `jobs/reaper.go`).
- `gateway/internal/webhookout/` — new reaper + test (delivered-only; never dead_letter).
- `gateway/cmd/gateway/main.go` — wire both reapers beside `jobs.Reaper` (main.go:181-188).
- `gateway/internal/config/config.go` — two retention-window knobs (zero-value = off).
- `.env.example` — document the knobs.

## Acceptance (checkable from the diff)

1. An idempotency reaper deletes only rows with `created_at < NOW() - retention`; a
   real-Postgres test proves fresh rows survive and expired rows are deleted; with the
   knob at zero-value the reaper is a no-op.
2. A webhook_deliveries reaper deletes only `status='delivered'` rows past the window;
   a test proves `dead_letter` (and non-terminal) rows are **never** deleted regardless
   of age.
3. Both reapers are bounded (batched delete loop, same shape as `jobs.Reaper`) and are
   spawned in `main.go` next to the existing reaper; both are inert at zero-value.
4. New config knobs default to disabled; `.env.example` documents them.
5. `go test -race ./...` green in `gateway/`.

## Parallel-safety / no-overlap

- The reaper LOGIC is entirely in `idempotency/**` and `webhookout/**`, which the
  same-cycle primary `async-jobs-fair-scheduling` (#194) does **not** touch (that primary
  explicitly forbids `webhookout/**` and scopes to `jobs/**`).
- Shared files are only `config.go` + `.env.example`, **additive-only**: this claim
  appends retention knobs; #194 appends fairness knobs, in a separate block. Neither PR
  reorders or rewrites existing fields, so the two additive hunks do not collide.
- Not a duplicate of any open PR; not in the false-rationale ledger (this is a real
  unbounded-growth reliability gap, not a coverage/timeout/thin-wrapper claim).
