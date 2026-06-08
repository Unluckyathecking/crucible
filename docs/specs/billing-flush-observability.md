# 10xworker:job — `billing-flush-observability`

> Primary decomposition spec. The canonical machine-readable contract is the JSON block in the PR body; this document is the human-facing decomposition for downstream 10X workers.

## Why this phase

PR #107 closed the monetization loop (checkout → customer linkage → subscription → meter flush → Stripe). The loop now runs end-to-end, but it is **flying blind on its most revenue-critical leg**. Today the only billing signal is the binary counter `crucible_billing_flush_total{outcome}` (`gateway/internal/observability/metrics.go:78-80`). There is:

- **no backlog visibility** — how many unflushed rows/units are piling up, and how old the oldest unflushed event is — so a Stripe outage or a stuck flusher is invisible until revenue is already lost; and
- **no detection of permanently un-billable usage** — the flusher's `AND c.stripe_customer_id IS NOT NULL` filter (`gateway/internal/usage/flusher.go:74` and `:115`) silently and permanently excludes usage for any customer who consumed the API but was never linked to Stripe. That usage is never billed, never logged, never alerted — a silent revenue leak on the loop #107 just closed.

This phase makes the flush pipeline's health **observable** (Prometheus gauges + alert rules) so operators can detect Stripe-outage backlog growth, stuck batches, and unbilled-customer leakage *before* they become lost revenue. The work only **observes**; it does not change flush semantics.

## Module boundary

The usage flusher already runs on a ticker, already holds the `*pgxpool.Pool`, and is already wired in `main.go` (`go flusher.Run(rootCtx)`). The new reconciliation scan is hosted on the existing flusher tick — **no `main.go` wiring is needed and none is permitted** (parallel-safe vs open PR #48, which owns `gateway/cmd/gateway/main.go` and `gateway/internal/auth/store.go`).

- New `gateway/internal/usage/reconcile.go` — bounded, aggregate-only backlog/leak SQL queries (kept out of `flusher.go` for testability).
- Extend `gateway/internal/usage/flusher.go` — after the two existing flush phases on each tick, run the reconcile scan and set gauges. The two-phase claim/emit idempotency guarantee and the `IS NOT NULL` filter are **preserved**; `emitAndMark` was hardened (returns `error`, accepts `customerID` for defense-in-depth, checks `RowsAffected`) but the flush semantics are unchanged.
- Extend `gateway/internal/observability/metrics.go` — add label-free aggregate gauges + mirror them in the test `Metrics` struct.
- New `gateway/migrations/0010_*.sql` — optional idempotent partial index supporting the unbilled-customer query if `EXPLAIN` shows a seq scan (invariant #8: idempotent, lexical-order).
- `ops/` — Prometheus alert rules + a Grafana panel for backlog age / leak gauge.

## Why this is reusable framework infra

Every Crucible clone inherits the same flusher and the same Stripe meter pipeline unchanged (per-product logic is worker-only). Backlog/leak observability is a cross-cutting billing concern that every leaf product (carrier-lookup, vat-worker, …) gets for free on the next rebase — exactly the "one gateway handles all cross-cutting concerns" promise. It touches none of the frozen contract surface (proto, `billable_units`, webhook ordering, flusher two-phase idempotency).

## Suggested decomposition for downstream workers

1. **Reconcile queries** — `usage/reconcile.go`: `BacklogStats(ctx)` → (unflushed_units, unflushed_rows, oldest_unflushed_age_seconds) using the existing `idx_usage_pending_flush` partial index (`migrations/0001_init.sql`); `UnbillableUsage(ctx)` → units/rows for customers with `stripe_customer_id IS NULL`. Aggregate/LIMIT-only; no per-row scan.
2. **Gauges** — add `BillingBacklogUnits`, `BillingBacklogRows`, `BillingBacklogOldestAgeSeconds`, `BillingUnbillableUnits`, `BillingUnbillableRows` to `metrics.go` (label-free) and mirror in the test `Metrics` struct / `NewMetricsForTest`.
3. **Flusher integration** — call reconcile + set gauges at the end of each flusher tick, after both existing phases.
4. **Bounded-cost / fail-soft guard** — reconcile query errors degrade to a logged warning and must never abort or block the flush phases (mirror existing phase error handling).
5. **Migration** — `0010_*.sql` idempotent partial index for the unbillable query if `EXPLAIN` shows a seq scan.
6. **Alert rules + dashboard** — `ops/` Prometheus rules (backlog age > threshold, unbillable units > 0, flush error-rate) + Grafana panel.
7. **Docs** — short ops note (in existing README/ops docs) on interpreting the new gauges. No new top-level doc file.

## Acceptance (verifiable from the diff)

See the PR-body JSON `acceptance` array. In summary: new gauges exist in `metrics.go` and are set from the flusher tick; `reconcile.go` exposes `BacklogStats` + `UnbillableUsage` with tests against real Postgres (per CLAUDE.md — no PG/Redis mocks); a failing reconcile query does not abort the flush phases (error-injection test); `go test -race ./...` green in `gateway/`; migration applies idempotently twice; `promtool check rules` passes on the new `ops/` rules; `gateway/cmd/gateway/main.go` and `gateway/internal/auth/store.go` are NOT modified.

## Forbidden

- No edits to `gateway/cmd/gateway/main.go` or `gateway/internal/auth/store.go` (collides with open PR #48).
- No change to the flusher's two-phase claim/emit logic, idempotency-key derivation, or the `stripe_customer_id IS NOT NULL` flush filter (invariant #4). Reconciliation only **observes** the leak — it must not auto-flush unlinked customers.
- No unbounded-cardinality labels on the new gauges (no per-customer_id / per-batch labels) — keep them label-free aggregates, per the cardinality discipline already enforced in `metrics.go` / `routes.go`.
- No new runtime dependency; Prometheus client is already in the module.

## Scope LOC

~600–900 LOC including tests, alert rules, and migration — well under the 10k cap.
