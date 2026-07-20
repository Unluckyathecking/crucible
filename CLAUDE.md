# Operating standard

Five pillars, in priority order: **simplicity, modularity, maintainability, cost-effectiveness, competency.** When they conflict, the earlier one wins. When in doubt, do less.

## Think before coding
State your assumptions; if uncertain, ask. If a request has several readings, surface them rather than silently pick one. If a simpler approach exists, say so. When something is unclear, stop and name it before writing code.

## Simplicity first
Write the minimum that solves the problem, nothing speculative. No features beyond the ask, no abstraction for single-use code, no configurability nobody requested, no error handling for cases that can't happen. If 200 lines could be 50, rewrite it. The test: would a senior engineer call this overcomplicated?

## Surgical changes
Touch only what the task needs. Don't "improve" adjacent code, don't refactor what isn't broken, match the existing style even where you'd do it differently. Remove only the orphans your own change created; leave pre-existing dead code alone and mention it instead. Every changed line should trace to the request.

## Modularity and maintainability
Many small files over few large ones — aim for 200-400 lines, functions under 50. One responsibility per module, low coupling, respect declared module boundaries. Don't repeat logic. Each module earns its own tests, and any new dependency earns a one-line reason.

## Goal-driven execution
Turn a task into a verifiable goal before starting: "fix the bug" becomes "write a failing test, then make it pass." For multi-step work, write a short plan with a check per step and loop until each one verifies.

## Cost
Tokens are a budget. Keep context minimal — a diff and its test output beat a whole-repo read. Prefer flat structures: a manager-of-workers only earns its cost when it does something the caller genuinely can't see. Reach for a fresh sub-agent only when a bounded, single job needs its own context.

## Writing that others read
Commit messages, PR descriptions, and review comments are a public record. Write them plain, tight, and competent — like a senior engineer, not a generated bullet list. No filler, no manufactured structure, straight quotes, punctuation used sparingly. Say what changed and why, then stop.

## Running a multi-agent loop
Learned in operation, kept because it earned its place:
- Stay flat. Add a manager between you and workers only if that manager does work you structurally can't see. It is never worth adding as a review hop when you already gate every merge.
- Trust state, not self-report. A delegate that looks idle may be done but unpushed. Check the branch or worktree directly on a fixed cadence; don't wait for an end-of-run message.
- Partition before you parallelize. Split ownership by path up front so two agents can't build the same thing. Discovering the overlap after both shipped is wasted spend.
- Fan out only read-only, bounded work (parallel review, parallel gating) — it's cheap and collision-free. Give write access only to a narrowly scoped, non-colliding fix.
- Grade on merit. When two implementations race, merge the one that clears a bar you stated in advance, even when it isn't yours.
- Confirm the ship. After a commit meant to land, check you're not on a detached HEAD and that nothing sits unpushed before calling the cycle done.
- Let the standard sharpen itself. Every so often, read the operating record for what worked and what didn't, and update this file from the evidence.

---
*Coding principles above draw on the widely-shared community distillation of Andrej Karpathy's coding-agent guidance.*

# Crucible — project brief

This file is the project-specific brief for Claude sessions and any other AI agents working in this repo. The operating standard above governs how to work; the Go language rules in `~/.claude/rules/golang/` also apply. What follows adds only the Crucible-specific, load-bearing constraints.

## What this repo is

A clone-and-adapt template for high-volume metered API products. One Go gateway handles all cross-cutting concerns (auth, rate-limit, Stripe metered billing, quota, observability). Per-product logic lives in a single worker process speaking a frozen HTTP/JSON contract. New products are `scripts/new-tool.sh <name>` away from a buildable tree.

Authoritative docs:
- **`README.md`** — quickstart, layout, current status.
- **`ADAPT.md`** — what to change per product, what to leave alone.
- **`docs-internal/REVIEW.md`** — pre-release review record + the three rounds of findings/fixes. Useful for understanding why some things are the way they are.

## Load-bearing invariants — do NOT change these casually

These exist for non-obvious reasons. If you think you need to change one of them, read `docs-internal/REVIEW.md` first; it almost certainly explains why.

1. **`gateway/proto/tool.proto` is frozen across all clones.** Never add per-product fields. `operation` is a free-form string the gateway forwards opaquely; product semantics live in the worker. Per-product proto extensions break the entire clone-and-adapt promise.

2. **`billable_units` is a contract, not a convention.** Workers MUST return >= 1 on every successful response. The gateway enforces this at the trust boundary (`server/routes.go`): success + `BillableUnits < 1` returns `502 WORKER_BAD_RESPONSE`. Removing that check re-opens free-usage escape.

3. **Stripe webhook ordering: dispatch first, record after success.** `billing/webhook.go` deliberately runs the handler before inserting the dedup row. Reversing this order permanently loses events when handlers transiently fail. The handlers (`handleSubscriptionUpsert`, `handleSubscriptionDeleted`) are idempotent on subscription state, so double-dispatch on race is safe; permanent loss is not.

4. **Stripe meter_event idempotency key uses a stable `batch_id`, not row-id ranges.** `usage/flusher.go` uses two phases: retry pending batches first (same UUID, Stripe dedupes), then claim newly-arrived rows into a fresh UUID. Don't "simplify" this back to MIN/MAX(id) — that caused double-billing in v0.

5. **API key hash semantics are mirrored across Go and TypeScript.** `gateway/internal/auth/keys.go::Hash` (SHA-256 of `salt || key`) and `dashboard/lib/keys.ts::hashKey` MUST stay byte-identical. The dashboard issues keys; the gateway verifies them. The base32 alphabet is RFC 4648 standard, padding-free. Don't switch to base64 in one without the other.

6. **`PrefixLen = 24`** in both Go and TS. The UNIQUE partial index `idx_api_keys_active_prefix_unique` depends on this giving enough entropy (15 random base32 chars). Lowering it re-opens prefix-collision auth failures.

7. **API key revocation goes through `Store.Revoke`.** Don't write a bare `UPDATE api_keys SET revoked_at = NOW()` somewhere else — the Redis cache won't get invalidated and the key stays valid for up to 60 s. If you need to revoke, call `Store.Revoke(ctx, keyID)`.

   **Plan changes also hit stale cache.** When `customers.plan_id` changes (e.g. upgrade, downgrade, plan tier edit), the Redis hot cache still holds the old plan for up to 60 s. If the new plan has different rate-limit or quota tiers, the old cached plan is applied to incoming requests during that window. To flush immediately:

   ```
   redis-cli DEL auth:<prefix>
   ```

   (The prefix is the first 24 characters of the API key.) This is safe to run even if the key isn't currently cached — DEL is idempotent.

   `Store.Revoke()` handles this automatically for key revocation, but plan edits bypass the revocation path, so manual invalidation is needed when plan tiers change.

8. **Migrations are idempotent and run on every gateway boot.** Every `gateway/migrations/*.sql` file uses `CREATE TABLE IF NOT EXISTS`, `INSERT ... ON CONFLICT DO NOTHING`, etc. No version-tracking table — files run in lexical order every time. Per-product clones add their seed migration at the next free index (`0026_*.sql` at the time of writing).

9. **`workers/sdk-go/` is the shared SDK. `workers/active` is a symlink.** Don't fork the SDK per product; extend it. Don't move the active worker out of `workers/`; the Dockerfile build context expects it there.

## What changes per product

See `ADAPT.md`. The summary: `workers/active`, one entry per endpoint in `gateway/internal/server/routes_table.go` (`V1Routes`), plan tier seeds in a new migration at the next free index, dashboard marketing copy, docs, Stripe product/prices. **Nothing else** should be touched per product. If you find yourself editing the gateway's `internal/auth`, `internal/billing`, `internal/ratelimit`, `internal/quota`, `internal/proxy`, or `internal/observability` per product — stop and explain why; you're probably solving the wrong problem.

## Coding style

The operating standard at the top of this file sets the general coding principles. The Crucible-specific stance (from `~/.claude/projects/-Users-mohammedalibhai/memory/feedback_scratch_build_lean.md`):

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
├── cmd/gateway/main.go       wiring: config → db → redis → runtime → middleware → server
├── internal/apierror/        canonical JSON error-response envelope for handlers
├── internal/audit/           append-only writes to the audit_log table
├── internal/auth/            API key issue, hash, lookup, Bearer gating; Store.Revoke
├── internal/billing/         plan cache, Stripe meter_event POST, checkout, webhook receiver
├── internal/cache/           redis client constructor (parse + ping)
├── internal/channelsig/      shared HMAC-SHA256 signed-channel primitive (webhooks + worker)
├── internal/config/          envconfig — the operational contract
├── internal/db/              pgx pool + embedded migration runner
├── internal/egress/          SSRF-hardened outbound HTTP transport (blocks private/loopback IPs)
├── internal/errorlog/        records non-2xx /v1 responses into error_events
├── internal/events/          outbound-webhook event catalogue + payload shapes
├── internal/httputil/        small shared HTTP helpers (status recorder, headers)
├── internal/idempotency/     dedups POST /v1 retries within a TTL window
├── internal/jobs/            durable Postgres-backed async job queue + worker-pool executor
├── internal/middleware/      request-id, recovery, access log, security headers, body limit
├── internal/observability/   Prometheus metrics + the middleware that increments them
├── internal/openapi/         builds and serves the OpenAPI 3.1 document
├── internal/operator/        read-only (SELECT-only) operator/admin query layer
├── internal/paging/          shared list pagination (parse, clamp, offset, envelope)
├── internal/proxy/           HTTP/JSON client to the worker
├── internal/quota/           per-customer monthly unit cap via atomic Redis reserve
├── internal/ratelimit/       per-customer per-minute sliding window (Redis sorted set + Lua)
├── internal/resilience/      default-off retry + circuit-breaker policies for worker calls
├── internal/respcache/       opt-in content-addressed cache of successful worker responses
├── internal/runtime/         assembles config-driven, default-off resilience + tracing providers
├── internal/selferrors/      serves GET /v1/errors (customer's own error history)
├── internal/selfusage/       serves GET /v1/usage (aggregate usage vs quota)
├── internal/selfusagedetail/ serves GET /v1/usage/events (per-event export, JSON/CSV)
├── internal/server/          chi router + handlers; per-product routes live in routes_table.go
├── internal/tracing/         optional, default-off OpenTelemetry (OTLP) tracing
├── internal/usage/           records usage_events sync; async two-phase Stripe flush
├── internal/validate/        stdlib-only JSON Schema subset validator for request bodies
├── internal/webhookout/      delivers signed outbound webhook POSTs to customer endpoints
└── migrations/               migrations.go (embedded fs.FS) + 0001–0025 framework schema; clones seed plans at the next free index (0026 at time of writing)

workers/
├── sdk-go/, sdk-rust/, sdk-ts/, sdk-python/   worker SDKs, one per host language, same /invoke contract
├── conformance/fixture.json                   shared contract fixture every SDK is tested against
├── stubs/                                     hello-world reference workers
└── active                                     symlink to the worker this clone ships

clients/                                       generated consumer SDKs (go, typescript, python) + openapi.json snapshot
test/conformance/                              cross-cutting contract conformance suite

dashboard/
├── app/{api, dashboard, login, operator, page.tsx}  Next.js 15 App Router (customer + operator console)
├── auth.config.ts + auth.ts                   edge-safe + full NextAuth config (split required for middleware)
├── lib/{db,keys}.ts                           postgres pool, API key generation/hash (MIRRORS Go)
└── middleware.ts                              auth-gates /dashboard/*, /operator/*, and customer API routes

ops/                                           prometheus + grafana provisioning
deploy/                                        deploy, backup, and host bootstrap scripts
scripts/
├── new-tool.sh                                clone-and-rename ergonomic
├── smoke-new-tool.sh                          THE CI smoke test — non-negotiable
├── conformance-run.sh                         builds a worker stub, runs the contract conformance suite, cleans up
├── acceptance-run.sh                          runs workers/active through the real gateway (real Postgres/Redis)
├── gen-clients.sh                             regenerates the clients/ SDKs from the OpenAPI snapshot
├── doctor.sh                                  preflight adapt-drift guard (env parity, active symlink, route table)
└── seed-dev.sh                                dev customer + API key for local testing
```

## When in doubt

Read `docs-internal/REVIEW.md`. Three rounds of reviews are recorded with each finding's verdict (real vs false-positive) and the reasoning. If you're about to touch anything in billing, auth, or webhook code paths, the answer to "should I?" is almost certainly already there.
