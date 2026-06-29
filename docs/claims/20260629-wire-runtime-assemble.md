# Claim — wire `runtime.Assemble` into `main.go`

Directive for the small-claim worker. Grounded in `main` HEAD `9e55830`.

## Target

`gateway/cmd/gateway/main.go` in `unluckyathecking/crucible`.

## Problem

`gateway/internal/runtime/assembly.go` exports `Assemble(ctx, cfg) -> Components{Policy,
TracerProvider, Shutdown}` (20 tests), built by PR #104 explicitly — per its commit message —
to convert "the dormant WorkerRetry/WorkerBreaker and OTel config knobs ... ready for injection
into proxy.New and server.Deps." The wiring step was never done:

- `grep -rn "internal/runtime" gateway/ --include=*.go | grep -v internal/runtime/` returns
  **zero matches** — `Assemble` is called nowhere.
- `main.go` builds `proxy.New(cfg.WorkerURL, ...timeout..., cfg.WorkerMaxConns).WithSecret(...)`
  with **no `ResiliencePolicy`** -> the worker breaker/retry are always disabled.
- `main.go` builds `server.Deps{...}` with **no `TracerProvider`** -> `tracing.Middleware`
  (routes.go) always receives nil -> tracing is a no-op even when `OTEL_TRACING_ENABLED=true`.

Consequence: `.env.example` documents `WORKER_RETRY_MAX`, `WORKER_RETRY_BACKOFF_MS`,
`WORKER_BREAKER_THRESHOLD`, `WORKER_BREAKER_COOLDOWN_MS` (and the OTel knobs), and `config.go`
validates them — but an operator who sets them gets silent no-ops.

## Change

In `gateway/cmd/gateway/main.go` only:
- Call `runtime.Assemble(ctx, cfg)` during startup wiring.
- Pass `Components.Policy` (the `ResiliencePolicy`) into `proxy.New(...)`, preserving the
  existing `.WithSecret(...)` HMAC chain.
- Set `Components.TracerProvider` on `server.Deps`.
- Register `Components.Shutdown` in the existing graceful-shutdown sequence (before/with
  `authStore.Close()`), respecting the current shutdown ordering.

## Expected outcome / acceptance

- `runtime.Assemble` is invoked exactly once in `main.go`; `Components.Policy` reaches
  `proxy.New` and `Components.TracerProvider` reaches `server.Deps`.
- With breaker/retry env knobs set, the resilience policy is active on the worker proxy path;
  with `OTEL_TRACING_ENABLED=true`, `tracing.Middleware` receives a non-nil provider.
- `Components.Shutdown` runs during graceful shutdown.
- `go build ./...` and `go test -race ./...` green in `gateway/`. Add a wiring assertion if
  feasible without a live OTel collector.

## Constraints

- Stay within `gateway/cmd/gateway/main.go`. Do not modify `internal/runtime/**` (already
  built + tested), `proxy/**` signatures, or the `/invoke` HMAC signing path (owned by the
  channel-auth work, already merged as #123 — preserve `.WithSecret`).
- Do not touch the frozen proto, auth, billing, ratelimit, or quota packages.
- Preserve the existing shutdown ordering and the 30s shutdown deadline.
