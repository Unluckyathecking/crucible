# Crucible — agent rules

This file is the project-specific brief for Claude sessions and any other AI agents working in this repo. Global rules in `~/.claude/rules/common/` and `~/.claude/rules/golang/` apply by default; what follows only adds the Crucible-specific load-bearing constraints.

## What this repo is

A clone-and-adapt template for high-volume metered API products. One Go gateway handles all cross-cutting concerns (auth, rate-limit, Stripe metered billing, quota, observability). Per-product logic lives in a single worker process speaking a frozen HTTP/JSON contract. New products are `scripts/new-tool.sh <name>` away from a buildable tree.

Authoritative docs:
- **`README.md`** — quickstart, layout, current status.
- **`ADAPT.md`** — what to change per product, what to leave alone.
- **`docs-internal/REVIEW.md`** — pre-release review record + the three rounds of findings/fixes. Useful for understanding why some things are the way they are.
- **`docs-internal/handoff-micro-saas-api-ideation-2026-05-16.md`** — the product catalogue that motivated the framework.
- **`~/.claude/plans/ultrathink-create-a-plan-proud-penguin.md`** — the original approved plan. Defers to ADAPT for per-product workflow.

## Load-bearing invariants — do NOT change these casually

These exist for non-obvious reasons. If you think you need to change one of them, read `docs-internal/REVIEW.md` first; it almost certainly explains why.

1. **`gateway/proto/tool.proto` is frozen across all clones.** Never add per-product fields. `operation` is a free-form string the gateway forwards opaquely; product semantics live in the worker. Per-product proto extensions break the entire clone-and-adapt promise.

2. **`billable_units` is a contract, not a convention.** Workers MUST return >= 1 on every successful response. The gateway enforces this at the trust boundary (`server/routes.go`): success + `BillableUnits < 1` returns `502 WORKER_BAD_RESPONSE`. Removing that check re-opens free-usage escape.

3. **Stripe webhook ordering: dispatch first, record after success.** `billing/webhook.go` deliberately runs the handler before inserting the dedup row. Reversing this order permanently loses events when handlers transiently fail. The handlers (`handleSubscriptionUpsert`, `handleSubscriptionDeleted`) are idempotent on subscription state, so double-dispatch on race is safe; permanent loss is not.

4. **Stripe meter_event idempotency key uses a stable `batch_id`, not row-id ranges.** `usage/flusher.go` uses two phases: retry pending batches first (same UUID, Stripe dedupes), then claim newly-arrived rows into a fresh UUID. Don't "simplify" this back to MIN/MAX(id) — that caused double-billing in v0.

5. **API key hash semantics are mirrored across Go and TypeScript.** `gateway/internal/auth/keys.go::Hash` (SHA-256 of `salt || key`) and `dashboard/lib/keys.ts::hashKey` MUST stay byte-identical. The dashboard issues keys; the gateway verifies them. The base32 alphabet is RFC 4648 standard, padding-free. Don't switch to base64 in one without the other.

6. **`PrefixLen = 24`** in both Go and TS. The UNIQUE partial index `idx_api_keys_active_prefix_unique` depends on this giving enough entropy (15 random base32 chars). Lowering it re-opens prefix-collision auth failures.

7. **API key revocation goes through `Store.Revoke`.** Don't write a bare `UPDATE api_keys SET revoked_at = NOW()` somewhere else — the Redis cache won't get invalidated and the key stays valid for up to 60 s. If you need to revoke, call `Store.Revoke(ctx, keyID)`.

8. **Migrations are idempotent and run on every gateway boot.** Every `gateway/migrations/*.sql` file uses `CREATE TABLE IF NOT EXISTS`, `INSERT ... ON CONFLICT DO NOTHING`, etc. No version-tracking table — files run in lexical order every time. Per-product clones add `0005_*.sql` etc.

9. **`workers/sdk-go/` is the shared SDK. `workers/active` is a symlink.** Don't fork the SDK per product; extend it. Don't move the active worker out of `workers/`; the Dockerfile build context expects it there.

## What changes per product

See `ADAPT.md`. The summary: `workers/active`, one line per endpoint in `gateway/internal/server/routes.go`, plan tier seeds in a new `gateway/migrations/0005_seed_plans.sql`, dashboard marketing copy, docs, Stripe product/prices. **Nothing else** should be touched per product. If you find yourself editing the gateway's `internal/auth`, `internal/billing`, `internal/ratelimit`, `internal/quota`, `internal/proxy`, or `internal/observability` per product — stop and explain why; you're probably solving the wrong problem.

## Coding style

Defers to `~/.claude/rules/common/coding-style.md` and `~/.claude/rules/golang/coding-style.md`. The project-specific stance is in `~/.claude/projects/-Users-mohammedalibhai/memory/feedback_scratch_build_lean.md`:

- **Scratch-build, don't port.** Even when prior code in `equipulse-trading-bot` or elsewhere solves a similar problem, write fresh in Crucible's idioms.
- **Every file earns its inclusion.** No speculative abstractions. No "in case we want to swap X later" wrappers. If three call sites would clearly benefit from a 30-line helper, ship it; otherwise don't.
- **Boring stdlib over novelty.** `net/http`, `pgx`, `chi`, `redis/v9`, `zerolog`, `envconfig`, `prometheus/client_golang`, `stripe-go` (or in our case, direct HTTP because the surface is small) — these are the locked picks. Novelty has to justify itself; obvious doesn't.
- **No comments explaining WHAT the code does.** Identifiers do that. Comments explain WHY: a non-obvious invariant, a workaround for a specific bug, behaviour that would surprise a reader. The flusher's two-phase explanation in `usage/flusher.go` is a good template — it explains the bug the design prevents.

## Testing expectations

- **`go test -race ./...`** must be green in `gateway/` and `workers/sdk-go/` before claiming done. Always include `-race` — the gateway has real concurrency in PlanCache, usage flushing, and rate-limit.
- **`bash scripts/smoke-new-tool.sh`** must be green after any change to `scripts/`, env templates, Dockerfiles, or `go.mod`. The smoke test runs in CI on every PR; locally is the same.
- **`pnpm build`** in `dashboard/` must be green after any TypeScript / Next.js change. The build also lint-checks types.
- **Webhook signature verification** has unit-test coverage in `gateway/internal/billing/webhook_test.go` (HMAC valid, wrong secret, expired, missing, malformed, tampered-body). If you touch `verifySignature`, add cases.
- **Don't mock Postgres or Redis in unit tests** — use real ones via `brew services` locally or `docker compose` services in CI. Mocks let migrations diverge from queries silently.

## Security non-negotiables

These were the reviewer findings that mattered. Keep them enforced.

- API key prefix lookup uses `strings.EqualFold` for the `Bearer` scheme (RFC 7235 case-insensitive).
- API key hash compare uses `subtle.ConstantTimeCompare` (`auth.VerifyHash`).
- Stripe webhook signature uses `hmac.Equal` (`crypto/hmac`'s constant-time compare) inside `Webhook.verifySignature`.
- Stripe webhook has a 5-minute replay window. Don't widen it.
- `webhook_events.event_id` is the dedup key (PK with ON CONFLICT DO NOTHING).
- `api_keys.hash` is `BYTEA` storing `SHA-256(salt || key)`. The full key is shown to the customer exactly once on issuance.
- Metric `path` label uses `chi.RouteContext(r).RoutePattern()` (bounded), never `r.URL.Path` (unbounded).
- Errors to customers expose a stable code + safe message; stack traces and internal IDs never escape.

## Workflow rules

- **Local-first.** No `gh repo create` until the framework hits its acceptance bar (already done). The user pushes when ready; no agent runs `git push` to a remote that doesn't exist.
- **Plan mode for non-trivial changes.** Use `~/.claude/plans/*.md` for anything spanning more than 3-4 files. Update the existing plan rather than creating new ones; one plan per project.
- **Single-shot reviews, not agent loops.** The OpenCode CLI agent interprets review prompts as work-to-execute and produces no output. Use a direct HTTP POST to OpenRouter (or similar) with the bundled source code in one request. Pattern documented in `docs-internal/REVIEW.md` under Process.
- **Don't dismiss reviewer findings without re-verification.** My initial dismissal of "flusher double-bill" in round-1 was wrong; Codex caught the real bug in round-2. Triage carefully, especially for billing / auth paths.

## File map (where things live)

```
gateway/
├── cmd/gateway/main.go                  wiring: config → db → redis → middleware → server
├── internal/auth/{keys,store,middleware} API key generation, lookup, Bearer enforcement, Store.Revoke
├── internal/billing/{plans,stripe,webhook} plan cache, Stripe meter_event POST, webhook receiver
├── internal/cache/redis.go              redis client constructor
├── internal/config/config.go            envconfig — the operational contract
├── internal/db/{pool,migrate}.go        pgx pool, embedded migration runner
├── internal/httputil/recorder.go        shared StatusRecorder used by middleware + observability
├── internal/middleware/middleware.go    request-id, recovery, access log, security headers, body limit
├── internal/observability/metrics.go    Prometheus counters/histograms + /metrics handler
├── internal/proxy/client.go             HTTP/JSON client to worker (gRPC opt-in seam)
├── internal/quota/{tracker,middleware}  monthly cap enforcement via Redis counter
├── internal/ratelimit/{bucket,middleware} per-customer per-minute fixed-window
├── internal/server/routes.go            chi router; PER-PRODUCT EDIT POINT for new endpoints
├── internal/usage/{recorder,flusher}    write usage_events sync; async two-phase Stripe flush
└── migrations/                          0001 init, 0002 nextauth, 0003 unique prefix, 0004 usage batches

workers/
├── sdk-go/crucible.go                   Serve() helper for Go workers
├── stubs/go/main.go                     hello-world Go worker (~30 LOC)
└── active                               symlink to the worker this clone ships

dashboard/
├── app/(api/auth, api/keys, dashboard, login, page.tsx)  Next.js 15 App Router
├── auth.config.ts + auth.ts             edge-safe + full NextAuth config (split required for middleware)
├── lib/{db,keys}.ts                     postgres pool, API key generation/hash (MIRRORS Go)
└── middleware.ts                        auth-gates /dashboard/* and /api/keys/*

ops/                                     prometheus + grafana provisioning
scripts/
├── new-tool.sh                          clone-and-rename ergonomic
├── smoke-new-tool.sh                    THE CI smoke test — non-negotiable
└── seed-dev.sh                          dev customer + API key for local testing
```

## When in doubt

Read `docs-internal/REVIEW.md`. Three rounds of reviews are recorded with each finding's verdict (real vs false-positive) and the reasoning. If you're about to touch anything in billing, auth, or webhook code paths, the answer to "should I?" is almost certainly already there.
