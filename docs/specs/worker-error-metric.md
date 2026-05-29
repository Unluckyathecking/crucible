# Directive: worker-errors-by-code Prometheus metric

**Date:** 2026-05-29
**Type:** worker:claim (small, observability)

See PR body for the full directive. Targets:
`gateway/internal/observability/metrics.go` (new CounterVec) and the worker-error
branch in `gateway/internal/server/routes.go` (single additive increment).
