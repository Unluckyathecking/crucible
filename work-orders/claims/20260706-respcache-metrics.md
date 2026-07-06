# Claim — respcache hit/miss/fail-open observability

**Lane:** `worker:claim` · **Seeded:** 2026-07-06

**Target:** `gateway/internal/observability/metrics.go`, `gateway/internal/respcache/middleware.go` (+ their tests). Small, bounded observability fix on the freshly-landed #156 module.

**Problem:** The respcache middleware (#156) has **zero** Prometheus instrumentation — `grep respcache gateway/internal/observability/metrics.go` returns nothing. Operators can't tell whether an opted-in route's cache is actually helping (hit rate) or whether the Redis store is silently degrading: on a `store.Get` error the middleware just `log.Warn(...)` and fails open to the worker (`middleware.go:106-108`), with no counter — unlike the established fail-open-gets-a-counter pattern used by ratelimit and quota (`crucible_ratelimit_failopen_total` / `crucible_quota_failopen_total`), which the #148 alert rules already depend on.

**Directive:** Add three counters mirroring the existing `promauto` pattern in `metrics.go`, registered in the `Metrics` struct and in `NewMetricsForTest`:
- `crucible_respcache_hits_total{operation}`
- `crucible_respcache_misses_total{operation}`
- `crucible_respcache_failopen_total{operation}`

The `operation` label is bounded (it comes from the fixed `V1Routes` operation set, same bounded-cardinality guarantee the metric `path` label relies on). Increment them at the three existing branch points in `middleware.go`: hit (the cache-hit serve path), miss (the store-miss → worker path), and fail-open (the `store.Get` error path at `:106-108`). Thread the `*observability.Metrics` (or a minimal counter interface) into the middleware constructor; keep the middleware nil-safe (nil metrics → no-op increment) so the default-off / test paths are unaffected.

**Acceptance:**
- A middleware test asserts the hits counter increments on a seeded-Redis cache hit and the misses counter increments on a miss.
- The three counters are registered by `NewMetricsForTest` and appear on the `/metrics` handler output.
- `operation` is the only label; no unbounded label is introduced.
- `go test -race ./gateway/...` green against real Redis (no mocks).

**Constraints:** Touch only `observability/metrics.go` + `respcache/middleware.go` (+ their tests). Do NOT alter cache semantics, key derivation, TTL handling, or the billing/quota/usage path — a hit must still reserve quota, record usage, and emit the Stripe meter event (invariant unchanged); this claim only observes. Do not touch `respcache/cache.go` (owned by the `respcache-usenumber` claim this cycle). Parallel-safe / disjoint from the `selferrors-read-api` primary.

---
_Seeded by the cross-repo sprint planner._
