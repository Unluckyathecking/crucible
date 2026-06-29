# Claim — `crucible_quota_exceeded_total` counter on the quota 429 path

Directive for the small-claim worker. Grounded in `main` HEAD `9e55830`.

## Target

`gateway/internal/observability/metrics.go` + `gateway/internal/quota/middleware.go` in
`unluckyathecking/crucible`.

## Problem

The rate-limit and quota rejection paths are observably asymmetric. `ratelimit/middleware.go`
increments `observability.RateLimitedTotal` on its 429. The symmetric quota path
(`quota/middleware.go`, the `!admitted` branch) writes
`apierror.Write(..., StatusTooManyRequests, QUOTA_EXCEEDED, ...)` but increments **no metric** —
`grep "Quota" observability/metrics.go` shows only `QuotaFailOpenTotal`, no rejection counter.
Operators can alert on rising rate-limit rejections but not on rising quota rejections.

## Change

Following the existing `RateLimitedTotal` pattern:
- Add a `crucible_quota_exceeded_total` counter in `observability/metrics.go` (register it,
  add it to the `Metrics` struct and to `NewMetricsForTest`, mirroring `RateLimitedTotal`).
- Increment it once in the `quota/middleware.go` `!admitted` (429) branch, alongside the
  existing `apierror.Write`.
- Add one unit test asserting the counter increments on a quota-exceeded request and does not
  increment on an admitted request.

## Expected outcome / acceptance

- `crucible_quota_exceeded_total` is registered and appears on `/metrics`.
- The counter increments exactly once per quota-rejected (429 `QUOTA_EXCEEDED`) request and not
  on admitted requests (covered by a new test).
- The metric naming/label style matches `RateLimitedTotal` (no unbounded labels).
- `go test -race ./...` green in `gateway/`.

## Constraints

- Stay within `gateway/internal/observability/**` and `gateway/internal/quota/**` (+ tests).
- Do not change the quota admission logic, the fail-open behavior, the `X-Quota-*` headers, or
  the `apierror` envelope. This is additive instrumentation only.
- Do not add per-customer or per-route labels (cardinality bound, per repo security rules).
- Use a real Redis in any test that exercises the quota path (no mocks).
