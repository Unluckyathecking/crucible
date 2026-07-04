# Work order — bound the MaxInt64 seed leak breaking `UnbillableUsage` reconcile tests

**Lane:** `worker:claim` (small, direct implementation)
**Area:** `gateway/internal/usage` (test-only fix)
**Date:** 2026-07-04

## Problem (real, reproduces on clean `main`)

Three tests fail on clean `main` with `bigint out of range` (SQLSTATE 22003):
`TestUnbillableUsage_noStripeCustomer`, `TestUnbillableUsage_stripeCustomerExcluded`,
and `TestSetBacklogGauges_setsGauges` (its `baseUbUnits` baseline call) — all in
`gateway/internal/usage/reconcile_test.go`.

Root cause is a leaked seed, not the reconcile tests themselves:

- `usage_events.billable_units` is `BIGINT` (`gateway/migrations/0001_init.sql:58`).
- `UnbillableUsage` does `COALESCE(SUM(u.billable_units), 0)::bigint`
  (`gateway/internal/usage/reconcile.go:57`). Postgres `SUM()` over `BIGINT` returns
  `NUMERIC`; the `::bigint` cast raises 22003 once the total exceeds `MaxInt64`.
- `TestRecord_tableDriven` (`gateway/internal/usage/recorder_test.go:112`, case
  "max int64 units" at ~line 125) seeds a row with
  `billable_units = 9223372036854775807` for a customer whose `stripe_customer_id` is
  NULL (an *unbillable* row). That test registers **no** `t.Cleanup(deleteUsageRows...)`,
  so it leaks `MaxInt64 (+ the other rows)` into the shared test DB. Any later
  `UnbillableUsage` call sums past `MaxInt64` → 22003.

`BacklogStats` is unaffected because it filters `stripe_customer_id IS NOT NULL` and the
leaked rows are NULL — consistent with only `UnbillableUsage` overflowing.

## Directive

Add the same cleanup the other `reconcile_test.go` tests already use, so the MaxInt64
seed does not leak into the shared table:

- In `gateway/internal/usage/recorder_test.go`, in `TestRecord_tableDriven`, right after
  the customer/api-key are set up (`custID, apiKeyID := setupTestCustomer(t, pool)`), add
  `t.Cleanup(func() { deleteUsageRows(t, pool, custID) })`.

## Acceptance

- `go test -race ./internal/usage/...` (with real Postgres+Redis) is green, including the
  three previously-failing reconcile tests, run in any order.
- No production source change — only `recorder_test.go` gains the `t.Cleanup` line.
- `TestRecord_tableDriven` still asserts everything it did before (the MaxInt64 case still
  runs; only its leaked rows are now cleaned up).

## Constraints

- Test-only. Do **not** change `reconcile.go` (the `::bigint` return signature can't
  represent a real >MaxInt64 backlog anyway; the overflow here is purely a test-DB leak).
- Disjoint from the `channelsig` primary (that touches `channelsig/`, `webhookout/`,
  `proxy/`) — this touches only `internal/usage/recorder_test.go`.
