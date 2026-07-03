# worker:claim — Prometheus alert rules for Redis fail-open and stuck circuit breaker

**Target file:** `ops/prometheus/rules/resilience.yml` (new). `ops/prometheus/prometheus.yml` already globs `rules/*.yml`, so no scrape-config change is needed.

**Concrete change:** Add a Prometheus rule group with alerts on the resilience signals that are emitted, wired, and unit-tested today but have no alerting consumer:
- `crucible_ratelimit_failopen_total` and `crucible_quota_failopen_total` — `rate(...[5m]) > 0` sustained for a few minutes → Redis degraded, customers silently exceeding rate-limit / monthly-quota caps. Warning severity.
- `crucible_worker_breaker_state == 1` sustained → worker down / circuit breaker stuck open. Warning/critical severity.

Match the structure and label conventions of the existing `ops/prometheus/rules/billing.yml` (group name, `for:`, `severity` labels, `summary`/`description` annotations). Use the exact metric names as registered in `gateway/internal/observability/metrics.go` — verify each name against the source before writing the rule.

**Expected outcome:** The otherwise-silent fail-open counters and the breaker-open state get the alerting consumer they were built for; operators are paged when Redis degradation lets customers bypass caps, or when a worker breaker is stuck open.

**Constraints:** Pure-additive ops config; zero Go changes. Do not touch `billing.yml` or `prometheus.yml`. Confirm each referenced metric name exists in `metrics.go` (do not invent metric names). Disjoint from #143, #146, #147.

**Verified gap:** `ls ops/prometheus/rules/` → only `billing.yml`; `grep -rn 'failopen\|breaker\|fail_open' ops/` → no matches. The metrics exist and are tested (`ratelimit_test.go`, `quota_test.go`) but nothing alerts on them.
