# Further steps — 2026-06-28

Engineering-direction re-seed for the resumed fleet. Operator has un-suspended the
frozen agents; this doc is the ready-to-claim backlog. Grounded in repo state at
`main` HEAD `7e8fc20` (06-15). Nothing below touches a load-bearing invariant
(proto frozen, `billable_units>=1` trust boundary, webhook dispatch-first ordering,
flusher `batch_id`, Go/TS hash parity, `PrefixLen=24`, `Store.Revoke`, idempotent
migrations). When in doubt, read `docs-internal/REVIEW.md`.

## Project goal (restated)

Crucible is a clone-and-adapt template for high-volume metered API products. One Go
gateway owns every cross-cutting concern (auth, rate-limit, Stripe metered billing,
quota, observability, OpenAPI/SDK generation, dashboard); per-product logic lives in
a single worker speaking the frozen HTTP/JSON `/invoke` contract. A new product is
worker + one route + plan tiers away from a buildable tree.

## Current state

v1 shipped and has hardened well past it. Since the v1 review, merged work includes:
OpenAPI 3.1 + route-registry drift guard (#58/#114), unified `apierror` envelope
(#111), request idempotency-key middleware (#100), OTel tracing default-off (#103),
self-serve Stripe lifecycle (#107), client-facing RateLimit/quota headers (#109),
signed/retried/dead-lettered outbound webhooks (#116), customer error-history +
request inspection (#115/#125), OpenAPI-driven request-body validation (#118), and
the `/invoke` non-POST→405 SDK conformance test (#124). Migrations run to 0013.
Three worker SDKs exist (Go, Rust, TS). The gateway↔worker channel-auth HMAC work
order (`docs-internal/work-orders/20260614-gateway-worker-auth.md`) is specced and
decomposed but **not yet merged** — it is the highest-value in-flight item.

## Ranked next work items

### 0. (In-flight) Land gateway↔worker channel auth — HMAC `/invoke` signing
- **Rationale:** Today the worker `/invoke` trusts any network caller; a worker
  reachable outside the gateway boundary bypasses auth/rate-limit/quota/billing —
  the same free-usage escape class on the ingress side. Already specced + decomposed.
- **Acceptance:** ship per `work-orders/20260614-gateway-worker-auth.md` acceptance
  list; `go test -race ./...` green in `gateway/` + `workers/sdk-go/`, Rust + TS SDK
  tests green; opt-in (empty secret == today's behaviour) keeps smoke test green.

### 1. Sliding-window rate limiter (close the ~2x edge burst)
- **Rationale:** `ratelimit/bucket.go` is still a fixed-minute window; REVIEW flags
  ~2x burst across the boundary as the one known correctness gap left open at v1.
  Client-facing headers (#109) already landed, so the contract surface is stable.
- **Acceptance:** Lua-atomic sliding window (or token bucket) in `bucket.go`; new
  `ratelimit_test.go` case proving a boundary-straddling burst is capped at the
  configured limit (not ~2x); `go test -race ./...` green.

### 2. Cross-SDK conformance parity (Go/Rust/TS run the same suite)
- **Rationale:** #124 added the non-POST→405 conformance assertion, but the suite
  must run identically across all three SDKs or the frozen contract drifts per
  language. TS SDK is the newest (v1.5) and the likeliest to diverge.
- **Acceptance:** a shared conformance matrix (method-guard, `billable_units>=1`,
  error envelope shape) executed against Go, Rust, and TS in CI; one new failing-
  then-passing case added to each; `worker-conformance.yml` green for all three.

### 3. Surface worker non-2xx response body to operators (diagnostic, not customer)
- **Rationale:** REVIEW "Open at v1" — gateway logs the worker status code but not
  the body, so a misbehaving worker is hard to debug. Distinct from the customer-
  facing error capture (#115/#125): this is operator-side, log/metric-bounded.
- **Acceptance:** `proxy/client.go` captures a size-bounded worker error body into
  the structured log (never the customer response); `client_test.go` asserts a
  non-2xx body is logged and is absent from the caller-facing envelope.

### 4. Adapt-ergonomics: `new-tool.sh` preflight doctor
- **Rationale:** Clone-and-adapt is the core promise; the v0 P2.2 env-drift bug
  (root vs dashboard `.env.example` prefix mismatch) shows how silent adapt
  misconfig slips through. A preflight check catches it before first boot.
- **Acceptance:** `scripts/` adds a doctor step asserting env prefix/salt parity,
  `workers/active` symlink resolves, and route↔OpenAPI registry are in sync;
  `bash scripts/smoke-new-tool.sh` green and the doctor fails loudly on injected drift.

## Fleet dispatch

These items are independent and ready for `worker:claim` pickup. Item 0 is already
decomposed in its work order; items 1–4 are sized for a single PR each. Take one,
branch off `main`, keep changes additive (no invariant files), and gate on the
acceptance criterion above plus `go test -race ./...` / `pnpm build` / smoke as
applicable. Update `docs-internal/REVIEW.md` "Open at v1" when an item lands.
