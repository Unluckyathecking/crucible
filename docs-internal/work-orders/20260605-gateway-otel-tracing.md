# Work order — gateway→worker distributed tracing (W3C / OTel)

Primary `10xworker:job` decomposition. Downstream 10X workers implement against this spec.

## Spec

```json
{
  "module": "gateway-otel-tracing",
  "scope": [
    "gateway/internal/tracing/*.go",
    "gateway/internal/server/routes.go",
    "gateway/internal/proxy/client.go",
    "gateway/internal/config/config.go",
    "gateway/go.mod",
    "gateway/go.sum"
  ],
  "input": "an inbound HTTP request (optionally carrying a W3C `traceparent`) plus gateway config",
  "output": "a span tree (gateway-request -> worker-call) exported via OTLP, `traceparent` propagated to the worker, and `trace_id`/`span_id` attached to every structured log line for the request",
  "acceptance": [
    "NEW package gateway/internal/tracing with a tracer-provider constructor that is DEFAULT-DISABLED (zero-config = no-op tracer, dials no exporter), mirroring the resilience default-off precedent in config.go",
    "routes.go::NewRouter mounts the tracing middleware in the framework middleware block (after RequestID, before AccessLog/observability.Middleware) — NOT the per-route section",
    "proxy/client.go::doOnce sets a well-formed W3C `traceparent` header on the outbound worker request whenever a span is active, WITHOUT removing the existing X-Request-ID set",
    "config.go adds OTEL/TRACING envconfig fields (enabled flag default-off, exporter endpoint, sample ratio) with range validation, plus a config_test.go case (valid + invalid) for each new field",
    "new package tests assert: (a) inbound traceparent is parsed and continues the trace, (b) absent header starts a fresh root span, (c) propagator writes a well-formed outbound traceparent, (d) no-op path when disabled",
    "log lines for a traced request carry trace_id (assert via captured zerolog output, as middleware_test.go already does for request_id)",
    "go test -race ./... green in gateway/"
  ],
  "forbidden": [
    "no change to gateway/proto/tool.proto (frozen) — propagation rides HTTP headers, never proto fields",
    "no edits to cmd/gateway/main.go or internal/auth/store.go (owned by open PR #48)",
    "no edits to .github/workflows/ci.yml (owned by open PR #102)",
    "no change to the billable_units>=1 -> 502 trust-boundary check, Store.Revoke, flusher batch_id, webhook ordering, API-key hash parity, PrefixLen=24",
    "no per-product field or per-product config — must be a framework default every clone inherits",
    "do NOT change X-Request-ID semantics — tracing ADDS correlation, it does not replace the flat id",
    "do NOT wire the dormant resilience package (#101) into main.go here — out of scope and conflicts with #48"
  ]
}
```

## Decomposition (6 subunits)

1. **Dependency + propagator decision** — add `go.opentelemetry.io/otel` + W3C propagator (or a ~60-line stdlib `traceparent` parse/format if the lean-stdlib stance wins). One doc-comment line explaining the WHY of the choice. Verify: `go build` green.
2. **`tracing` package core** — tracer-provider constructor (enabled/disabled), exporter selection (OTLP endpoint vs no-op), sampler from config. Verify: disabled config returns a no-op tracer that dials nothing.
3. **Inbound HTTP middleware** — extract/continue `traceparent`, start a server span named by chi RoutePattern (bounded, like the metrics label in metrics.go), put span in context. Verify: continue-vs-root tests.
4. **Log enrichment** — zerolog hook/helper stamping `trace_id`/`span_id` from context so the existing `Str("request_id", rid)` sites gain trace ids without rewriting each call. Verify: captured-output test.
5. **Outbound propagation in proxy** — inject `traceparent` in `doOnce` and wrap each attempt (including retries) in a client span so retry causality is visible. Verify: header present + well-formed; existing proxy tests stay green.
6. **Config + wiring** — add/validate OTEL/TRACING env fields; mount middleware in NewRouter; thread tracer through Deps. Verify: config_test.go range cases + `go test -race ./...` green.

## Rationale

Crucible observability is metrics-deep and log-present but **causality-blind**: correlation is one flat `X-Request-ID`, re-stamped manually at 8+ log sites (`middleware.go:41,62`; `routes.go:162,181,211,221`) and forwarded to the worker as a bare header (`proxy/client.go:334`) with no span hierarchy and no W3C `traceparent` (zero otel refs in tree or `go.mod`). Because the framework middleware chain (`routes.go::NewRouter`) and the proxy seam are inherited unchanged by every clone, adding OTel spans + W3C propagation there is a one-time framework investment giving every present and future product end-to-end gateway→worker tracing — and it is byte-disjoint from #48 (`main.go`) and #102 (`ci.yml`).
