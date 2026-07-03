# worker:claim — count worker-unreachable and contract-violation 502s

**Target files:** `gateway/internal/server/routes.go` (the `invoke` handler), `gateway/internal/server/routes_test.go` (assertion), `gateway/internal/observability/metrics.go` (doc-comment note only if needed).

**Concrete change:** Increment the existing bounded `observability.WorkerErrorsTotal{code}` counter on the two 502 failure paths in `invoke()` that currently increment nothing:
- the `err != nil` → `WORKER_UNREACHABLE` path (~routes.go:361), and
- the `billable_units < 1` → `WORKER_BAD_RESPONSE` contract-violation path (~routes.go:400).

Today `WorkerErrorsTotal.Inc` fires only at routes.go:373 (the `resp.Error` structured-envelope path). Add a `routes_test.go` assertion covering both new increments.

**Expected outcome:** Operators can alert on worker-unreachable rate and, critically, on the free-usage-escape trust-boundary rejection (`billable_units >= 1`, invariant #2) as a distinct metric series — instead of both being invisible inside an undifferentiated `crucible_requests_total{status="502"}`.

**Constraints:** Reuse the existing `WorkerErrorsTotal` CounterVec — do NOT add a new metric or a new label. The `code` label stays bounded (fixed set of error codes, never request-derived). Do not change status codes, error envelopes, or the `billable_units < 1` → 502 behaviour itself; observability-only. Disjoint from #143 (main.go) and PR #146 (webhookout/events/dashboard).

**Verified gap:** `grep -rn WorkerErrorsTotal.Inc gateway/internal/` → the only call site is routes.go:373; the other two 502 returns in `invoke()` emit no metric.
