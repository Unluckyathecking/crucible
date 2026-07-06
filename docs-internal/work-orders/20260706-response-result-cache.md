# Work order — Framework opt-in content-addressed worker result cache (`response-result-cache`)

**Lane:** `10xworker:job` · **Module:** `response-result-cache` · **Seeded:** 2026-07-06

## Spec

```json
{
  "module": "response-result-cache",
  "scope": [
    "gateway/internal/respcache/cache.go",
    "gateway/internal/respcache/middleware.go",
    "gateway/internal/respcache/cache_test.go",
    "gateway/internal/respcache/middleware_test.go",
    "gateway/internal/server/routes.go",
    "gateway/internal/server/routes_table.go",
    "gateway/internal/config/config.go",
    "gateway/internal/config/config_test.go",
    "gateway/cmd/gateway/main.go"
  ],
  "input": "A per-product opt-in TTL declared on selected /v1 invoke routes, plus each authenticated, quota-reserved, non-replay request that has already passed auth + rate-limit + idempotency + quota in the existing /v1 chain.",
  "output": "A new gateway-only respcache package: a Redis-backed, content-addressed cache of successful worker responses keyed by sha256(operation || canonical-payload), plus a nil-safe middleware that, on a cache hit for a cache-opted route, serves the stored response WITHOUT re-invoking the worker while still recording usage + emitting the Stripe meter event with the cached BillableUnits. A miss calls the worker and populates the cache. Default-off; zero behavior change until a product sets CacheTTLSeconds on a route and the store is constructed.",
  "acceptance": [
    "New package gateway/internal/respcache with a Store (Get/Set over the existing redis client) and a Middleware; when the Store is nil the middleware is strict pass-through (mirror idempotency.Middleware nil-store handling and the d.DB nil-safe pattern in routes.go). main.go constructs the store from the already-built redis client and is byte-disjoint / default-off when caching is disabled.",
    "Cache key is sha256 over operation || canonical(payload) using a stable JSON canonicalization; the key MUST NOT include the API key, customer id, or any secret. A documented consequence: identical (operation,payload) from different customers can share a cached answer — this is intentional because quota, rate-limit, and billing all ran per-customer BEFORE the cache middleware.",
    "The middleware is registered in the /v1 chain AFTER quota.Middleware and immediately in front of the worker invoke handler, so a cache hit still reserves quota, still calls recorder.Record with the cached BillableUnits, and still emits the Stripe meter event; the hit only skips the worker HTTP round-trip. A test asserts a hit still increments usage_events.",
    "Only responses that pass the existing success + BillableUnits >= 1 trust-boundary check (routes.go invoke) are eligible to be stored. Worker errors, non-2xx, and BillableUnits < 1 (already 502 WORKER_BAD_RESPONSE) are NEVER cached.",
    "A route opts in via a new CacheTTLSeconds field on the RouteDescriptor (routes_table.go); 0 (the default) means the route is never cached, a positive value is the per-entry TTL clamped to a new config max (RespCacheMaxTTLSeconds). config_test.go covers a valid TTL, a negative value, and an over-max value for the new config field(s).",
    "go test -race ./... green in gateway/ against real Redis (no mocks); respcache tests cover hit, miss, TTL expiry, per-operation opt-in gating (TTL==0 route never caches), error-response non-caching, and hit-still-meters."
  ],
  "forbidden": [
    "No edit to gateway/proto/tool.proto (frozen, invariant #1) — caching lives entirely in the gateway.",
    "Do not weaken, relocate, or bypass the success + BillableUnits < 1 -> 502 WORKER_BAD_RESPONSE check (invariant #2); the cache reuses its verified output, it does not re-implement it.",
    "Do not reorder the cache relative to auth/rate-limit/idempotency/quota — a cache hit MUST remain a fully-metered, quota-counted, per-customer-billed call. It is never a free call.",
    "Do not fold this into internal/idempotency — that primitive is client-Idempotency-Key-scoped exactly-once semantics, a different contract from content-addressed cross-request dedup. Keep respcache a separate package.",
    "Do not add a schema/migration; the cache is Redis-only. Do not touch the per-product V1Routes edit point — register the middleware only in the framework /v1 block."
  ]
}
```

## Rationale

Crucible's thesis is "high-volume metered API products," and every leaf product to date (carrier-lookup;
the VIES VAT-validation candidate) is a **read-heavy lookup where identical inputs recur constantly and
every worker miss costs an upstream/HLR fee**. A framework-level, opt-in, content-addressed result cache is
the single highest-leverage reusable infrastructure remaining: it compounds across every lookup-type clone
with one `CacheTTLSeconds` line of ADAPT edit, reuses the already-constructed Redis client, and slots into
the existing nil-safe middleware pattern without touching any billing/auth/quota invariant. It is deliberately
distinct from `idempotency` (client-key exactly-once) — this is cross-request content dedup — and from a free
call: a cache hit still reserves quota, records usage, and bills, only skipping the worker round-trip. No such
module exists today (`internal/cache` is only the redis constructor). Parallel-safe with this cycle's auth
key-rotation claim (disjoint file set).
