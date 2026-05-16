# Crucible Full Code Review

Date: 2026-05-16 (v2 — updated after re-review of current tree)

Scope: full review of the repository at `/Users/mohammedalibhai/Documents/crucible`. The original review was run against the initial import; this update reflects fixes applied since then and adds new findings.

## Executive summary

The project has a solid shape for a clone-and-adapt metered API framework. Since the original review, four P1 issues have been fixed:

1. **Flusher double-billing** — resolved. The flusher now uses two-phase batch claims (`batch_id`) with stable Stripe idempotency keys.
2. **Webhook dedupe ordering** — resolved. Dispatch now runs before `recordEvent`; failed dispatches are not recorded, so Stripe retries re-process correctly.
3. **API key prefix entropy** — resolved. `PrefixLen` increased from 12 to 24 (15 random base32 chars ≈ 3.5×10²² combinations).
4. **`billable_units >= 1` enforcement** — resolved. The gateway rejects worker responses with `billable_units < 1` as `502 WORKER_BAD_RESPONSE`.
5. **Monthly quota enforcement** — resolved. A `quota` package with Redis-backed tracking and middleware now blocks over-cap requests with `429 QUOTA_EXCEEDED`.
6. **Plan cache now loads `monthly_unit_cap`** — resolved. `PlanEntry` includes `MonthlyCap` and the query fetches it.

Remaining open issues are at P2 and below. The codebase is approaching production-readiness for billing paths; outstanding items are correctness hardening, operational hygiene, and the clone env mismatch.

## Checks run

```bash
make test
```

Result: passed.

```bash
pnpm build
```

Run from `dashboard/`. Result: passed.

```bash
make smoke-test-new-tool
```

Result: passed.

Docker/Compose was not run because `docker` is not installed in this environment.

## Original P1 findings — status

| # | Finding | Status |
|---|---------|--------|
| 1 | Stripe webhooks permanently dropped after transient handler failure | **FIXED** — dispatch-before-record ordering; handlers are idempotent on subscription state |
| 2 | Usage flush double-billing via changing idempotency key | **FIXED** — `batch_id` two-phase claim; stable `crucible-batch-<uuid>` idempotency key |
| 3 | API key prefix collisions at real customer counts | **FIXED** — `PrefixLen=24` (15 random base32 chars); unique partial index on active prefixes |
| 4 | `billable_units: 0` escapes billing | **FIXED** — gateway rejects with `502 WORKER_BAD_RESPONSE` |
| 5 | Monthly caps not enforced | **FIXED** — `quota.Tracker` + `quota.Middleware` with Redis counters and 429 response |

## Open findings

### P2: `new-tool.sh` leaves dashboard key settings mismatched

Evidence:

- `scripts/new-tool.sh:70` rewrites only root `.env.example`.
- `dashboard/.env.example:10` keeps `API_KEY_PREFIX=cru_`.
- `dashboard/.env.example:11` keeps the placeholder `API_KEY_HASH_SALT`.

Impact:

After cloning a product, root `.env.example` and dashboard `.env.example` disagree. The dashboard can issue keys with a different prefix or salt than the gateway verifies, causing dashboard-created keys to fail authentication.

Recommended fix:

Have `new-tool.sh` update both env examples, or remove duplicated dashboard key settings and document that dashboard should share the same env source as the gateway.

Test to add:

Extend `scripts/smoke-new-tool.sh` to grep both root and dashboard env examples and assert that the API key prefix and salt placeholder/fresh value match.

### P2: Docker Go images are older than the modules require

Evidence:

- `gateway/go.mod:3` requires Go 1.25.0.
- `workers/sdk-go/go.mod:3` requires Go 1.25.
- `gateway/Dockerfile:2` uses `golang:1.23-alpine`.
- `workers/stubs/go/Dockerfile:4` uses `golang:1.23-alpine`.

Impact:

Docker builds may fail or implicitly download a newer toolchain depending on Go's toolchain behavior and network availability. The Dockerfiles should be deterministic and match the module requirement.

Recommended fix:

Use `golang:1.25-alpine` or a pinned patch version available from the official image registry. Keep CI, Dockerfiles, and `go.mod` aligned.

Test to add:

Run `docker compose build gateway worker` in CI or add a lightweight Docker build job if Compose is too heavy.

### P2: Revoked API keys remain valid for up to 60 seconds

Evidence:

- `gateway/internal/auth/store.go:53-61` caches auth lookups in Redis with a 60-second TTL.
- When a key is revoked (setting `revoked_at`), the cache entry is not invalidated.
- The SQL query at `store.go:64-70` filters `WHERE revoked_at IS NULL`, so the DB path correctly rejects revoked keys — but any request hitting the Redis cache within the 60s TTL will succeed.

Impact:

A revoked key continues to authenticate until the TTL expires. For most API products 60 seconds is acceptable, but it should be documented. If instant revocation is needed:

- Add a Redis `DEL auth:<prefix>` on revoke, or
- Use a cache-busting approach (e.g., revocation list in Redis).

Recommended fix:

Add a `Revoke(ctx, prefix)` method that issues `DEL auth:<prefix>` to Redis alongside the DB `UPDATE ... SET revoked_at`. This makes revocation near-instant at the cost of one extra Redis call per revoke.

Test to add:

Create a key, cache-bust it with a request (populates Redis), revoke it, and verify the next request returns 401 without waiting for TTL expiry.

### P3: `writeUnauthorized` uses string concatenation to build JSON

Evidence:

- `gateway/internal/auth/middleware.go:55`:

  ```go
  _, _ = w.Write([]byte(`{"error":{"code":"UNAUTHORIZED","message":"` + msg + `"}}`))
  ```

- All other error helpers in `routes.go` use `json.NewEncoder(w).Encode(...)`.

Impact:

Currently `msg` is always a hardcoded literal, so this is not exploitable. But the pattern is fragile — any future caller passing user-derived data will break the JSON envelope and potentially enable XSS in API responses.

Recommended fix:

Replace with `json.NewEncoder(w).Encode(...)` like the other error helpers, or at minimum switch to `fmt.Fprintf` with a JSON-safe template.

### P3: Duplicate `statusRecorder` type

Evidence:

Both `gateway/internal/middleware/middleware.go:92-99` and `gateway/internal/observability/metrics.go:85-92` define identical `statusRecorder` structs with the same `WriteHeader` method. If either copy diverges (e.g., adding `Write` byte-count tracking), the other won't get the update.

Recommended fix:

Extract to a shared `internal/httputil` package or have `observability` depend on `middleware`'s type. This is low urgency but prevents future divergence bugs.

### P3: A local Mach-O binary is present in the worker stub

Evidence:

- `workers/stubs/go/go` is an 8.1 MB Mach-O arm64 executable.
- `.gitignore` ignores `*.test`, `*.out`, coverage, and `gateway/bin/`, but not this output path.

Impact:

If committed, the repository includes a platform-specific local build artifact. It also gets copied into clone output and Docker build context, which is noisy and confusing.

Recommended fix:

Remove `workers/stubs/go/go` and add ignore patterns for common local binaries, or direct local builds into ignored `bin/` paths.

## Secondary observations

### README status is stale

`README.md` says the gateway is currently a `/healthz` stub and that auth, billing, rate limiting, dashboard, and CI are not implemented. The codebase now includes those components. Updating the status section would prevent confusion for anyone evaluating the template.

### Plan cache now checks `rows.Err()`

The previous review noted that `plans.go` did not check `rows.Err()` after iteration. The current code at `plans.go:101-104` now checks and logs it. This observation is resolved.

### Metrics path labels now use chi route patterns

The previous review noted that `metrics.go` used `r.URL.Path` (unbounded cardinality). The current code at `metrics.go:69` uses `chi.RouteContext(r.Context()).RoutePattern()` with a fallback to `"unmatched"` for 404s. This observation is resolved.

## Suggested fix order

1. Fix clone env synchronization (`new-tool.sh` + dashboard `.env.example`).
2. Align Docker Go versions with `go.mod`.
3. Add Redis cache invalidation on key revocation.
4. Replace `writeUnauthorized` string concat with `json.NewEncoder`.
5. Extract shared `statusRecorder`.
6. Remove local worker binary and tighten ignore rules.

## Release readiness

The five original P1 issues (billing double-count, webhook event loss, prefix collisions, zero-unit billing escape, and monthly cap enforcement) are all fixed. The billing and auth paths are now robust enough for production use.

The remaining P2 issues (clone env mismatch, Docker version skew, revoked-key TTL) should be addressed before general availability. The P3 items are code hygiene that can be batched.

The next review should focus on end-to-end lifecycle flows:

- Customer signup to API key creation.
- API key use through gateway and worker.
- Usage recording to Stripe meter event.
- Subscription upgrade/downgrade through webhook.
- Quota exhaustion behavior.
- Clone-and-adapt workflow from `scripts/new-tool.sh` to a running product.

## Reviewer methodology

- **Round 1**: OpenCode CLI agent with `deepseek-v4-pro` and `kimi-k2.6` (agent loop produced no usable output; switched to 1-shot HTTP POST via OpenRouter). OpenRouter `deepseek/deepseek-chat-v3.1` bundled all 14 gateway files into one request. Self-review in parallel as ground truth.
- **Round 2**: `glm-5.1` full-codebase review via opencode-go task tool, cross-referenced against current tree and original findings. Confirmed five P1 fixes, identified new findings (revoked-key TTL, `writeUnauthorized` JSON concat, duplicate `statusRecorder`).