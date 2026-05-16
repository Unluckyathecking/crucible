# Pre-release review — 2026-05-16

## Process

Two passes, both single-shot (no agent loops):

1. **Internal: OpenRouter 1-shot, DeepSeek V3.1** (`docs-internal/REVIEW.md` initial pass). Bundled all 14 gateway files into one HTTP POST. Most of its CRITICAL findings were stdlib misreads (`hmac.Equal` flagged as non-constant-time, missed `io.Copy(io.Discard, ...)`, etc.). The one genuine HIGH was my self-review finding: webhook dedup happening before dispatch could permanently drop events on transient handler failure.

2. **External: Codex full-tree review** (`docs-internal/full-code-review-2026-05-16.md`). Higher-signal pass. Caught 6 real issues missed by both DeepSeek and my self-review.

3. **GLM-5.1 third pass** running in parallel as of 2026-05-16T17:50; results will be triaged when complete.

The Codex CLI binary on the local install is broken (missing native binary, `ENOENT`); the actual Codex review was provided externally and reviewed against the post-Sprint-8 codebase. The OpenCode CLI's agent loop interprets review prompts as work-to-execute and produces no output for read-only review — switching to direct HTTP POST is the right pattern.

## Fixes applied — round 1 (Sprint 8 close)

| Severity | File | Issue | Status |
|---|---|---|---|
| HIGH (self-review) | billing/webhook.go | Idempotency row inserted BEFORE dispatch → lost events on transient handler failure. | Fixed: dispatch first, record on success. Verified end-to-end. |
| MEDIUM (self-review) | observability/metrics.go | Metric `path` label was unbounded `r.URL.Path` — random-path attacks blow up Prometheus series. | Fixed: use `chi.RouteContext(r).RoutePattern()`, fallback `"unmatched"`. |
| LOW (DeepSeek) | auth/middleware.go | `Bearer` prefix case-sensitive (RFC 7235 says it shouldn't be). | Fixed: `strings.EqualFold`. |
| LOW (DeepSeek + self) | billing/plans.go | Stale-cache TTL stampede on reload. | Fixed: single-flight `loading` flag. |

## Fixes applied — round 2 (Codex pass)

| Severity | File | Issue | Status |
|---|---|---|---|
| **P1.2** | usage/flusher.go + migration 0004 | Stripe idempotency-key was derived from `(min_id, max_id)` of the unflushed-rows window. If Stripe accepted the event but the mark-flushed UPDATE failed, the next tick saw the OLD rows + new arrivals → different `(min, max)` → different idem-key → Stripe billed the old rows again. | **Fixed: persistent `batch_id` column.** Two-phase flush — retry pending batches first (same idem-key, Stripe dedupes), then claim newly-arrived rows into a fresh UUID. Stable across crashes. |
| **P1.3** | auth/keys.go, dashboard/lib/keys.ts, migration 0003 | `PrefixLen = 12` left only 3 random base32 chars after the literal `cru_live_` — collision likely at hundreds of keys. With `LIMIT 1` on lookup, a colliding key fails to authenticate. | **Fixed: PrefixLen → 24** (15 chars of entropy = 32^15 ≈ 3.5e22 combinations). Added UNIQUE partial index `idx_api_keys_active_prefix_unique`. Dashboard retries on `23505` (Postgres unique-violation). |
| **P1.4** | server/routes.go | A non-SDK worker returning success with `billable_units=0` would have its usage insert fail the DB check constraint, but the gateway STILL returned the successful payload to the customer → unbilled usage. | **Fixed: gateway rejects** with `502 WORKER_BAD_RESPONSE` when `resp.Error == nil && resp.BillableUnits < 1`. The trust boundary now enforces the contract regardless of which language the worker is written in. |
| **P2.1** | quota/ + billing/plans.go + usage/recorder.go | `monthly_unit_cap` was defined in schema but never enforced — customers could exceed their plan cap indefinitely. | **Fixed: new `quota` package**. Redis counter keyed `quota:<customer>:<YYYY-MM>`, incremented on every successful usage record, checked by the quota middleware before forwarding. Plans cache now exposes `MonthlyCap`. Verified end-to-end: 1500-unit counter on a 1000-cap free plan returns `429 QUOTA_EXCEEDED`. |
| **P2.2** | scripts/new-tool.sh + scripts/smoke-new-tool.sh | `new-tool.sh` rewrote only `.env.example`, leaving `dashboard/.env.example` with the old `cru_` prefix → dashboard-issued keys wouldn't verify on the cloned gateway. | **Fixed: script rewrites both env examples**, applies the same fresh salt to both. Smoke-test grep'd to assert they stay in sync. |
| **P2.3** | gateway/Dockerfile, workers/stubs/go/Dockerfile | `FROM golang:1.23-alpine` but `go.mod` requires `1.25`. Non-deterministic toolchain download or build failure. | **Fixed: bumped to `golang:1.25-alpine`** in both. |
| **P3.1** | workers/stubs/go/go + .gitignore | A stray 8 MB Mach-O binary from a bare `go build` was sitting in the repo tree. | **Removed**. Added `workers/stubs/go/go` + `workers/stubs/*/main` patterns to `.gitignore`. |
| **S2** | billing/plans.go | Cache reload loop didn't check `rows.Err()` → partially loaded plan map silently possible. | **Fixed: `rows.Err()` checked before swapping in the new map**; on error, keep last-known values. |

## Reviewer claims rejected on triage

DeepSeek's CRITICAL/HIGH findings that did NOT survive triage:

- `hmac.Equal` flagged as non-constant-time → it IS constant-time per stdlib docs.
- `string(body)` in HMAC payload "corrupts binary" → Go strings hold arbitrary bytes; the round-trip is byte-identical.
- Worker non-200 body "not drained" → code already calls `io.Copy(io.Discard, resp.Body)`.
- Cache JSON unmarshal "could bypass auth" → failure path goes to DB lookup, which still hash-gates.
- `last_used_at` goroutine "may leak" → uses its own `context.WithTimeout(context.Background(), 2s)`, fully bounded.

## What's verified

- `go test -race ./...` green across all internal packages.
- `pnpm build` green.
- `bash scripts/smoke-new-tool.sh` green — clone-and-rename produces a buildable tree AND root/dashboard env stay in sync.
- End-to-end on local Postgres 17 + Redis 8:
  - All four migrations apply (0001 init, 0002 nextauth, 0003 unique prefix, 0004 usage batches).
  - 24-char prefixes issued via `seed-dev.sh`.
  - `/v1/echo` returns 200 with `X-Billable-Units` header.
  - Webhook HMAC verifies; idempotency holds across replay; failed dispatch does NOT poison the dedupe table.
  - Quota gate: 1500-unit counter on free plan (cap=1000) → `429 QUOTA_EXCEEDED`; under-cap call returns 200 and counter increments correctly.
  - Auth cache invalidation pattern documented: changing `customers.plan_id` requires `redis-cli DEL auth:<prefix>` (or a 60s wait for TTL).

## Fixes applied — round 3 (GLM-5.1 pass)

GLM-5.1's v2 review confirmed all 5 round-2 P1 fixes. Its "still open" P2.2/P2.3/P3.1 items were stale-snapshot artefacts (those were already fixed before the review ran). Three genuinely new findings:

| Severity | File | Issue | Status |
|---|---|---|---|
| P2 | auth/store.go | Revoked keys remained valid for up to 60 s — the Redis lookup cache wasn't invalidated, only the DB row's `revoked_at` was set. | **Fixed: `Store.Revoke(ctx, keyID)` method** — single API that UPDATEs `revoked_at` and DELs the `auth:<prefix>` Redis entry in one call. Verified end-to-end: cache-warmed key 401s instantly after revoke. |
| P3 | auth/middleware.go | `writeUnauthorized` used string concatenation to build the JSON envelope. Callers only pass literals today, but the pattern would corrupt the response if a future caller forwarded user-derived data. | **Fixed: switched to `json.NewEncoder(w).Encode(...)`** in line with the rest of the gateway's error helpers. |
| P3 | middleware/ + observability/ | Two identical `statusRecorder` types in two packages — guaranteed to diverge eventually. | **Fixed: extracted to `internal/httputil.StatusRecorder`** (single source of truth, both packages import it). |

## Open at v1

- **Sliding-window rate limit**: fixed-minute boundaries still allow ~2x burst across the window edge. Acceptable for current scale; revisit when a customer complains.
- **Stripe signature `v2` support**: only `v1=` is honoured today. Stripe hasn't shipped v2, but if they do this is a one-line fix.
- **Quota soft-overshoot**: under heavy stampede on the boundary, multiple goroutines can read `current < cap` simultaneously and all admit. The Postgres row is the durable truth; the Redis counter is a fast-read mirror. Soft overshoot is acceptable for billing-per-unit semantics.
- **Worker non-200 body not surfaced**: gateway logs the status code but not the body. Add when worker failures actually need diagnostic visibility.

## Release readiness

The framework now clears the bar Codex flagged as blocking. Billing safety (P1.2), authentication correctness (P1.3), contract enforcement (P1.4), and product-contract enforcement (P2.1 quotas) are all in place and tested. Pending the GLM-5.1 pass landing, this is v1-shippable pending a `gh repo create` + push.
