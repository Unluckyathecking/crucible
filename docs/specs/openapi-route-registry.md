# Spec — `openapi-route-registry`

Primary `10xworker:job` decomposition. Collapse the gateway's two independently
hand-maintained `/v1` route surfaces into one declarative descriptor table, and
add a drift guard so the router and the published OpenAPI document (and therefore
the generated client SDKs) can never silently disagree.

## Problem (verified against `main`, HEAD f1e6331)

The live route surface and the published API surface are two separate, hand-edited
sources of truth for the same thing:

- `internal/server/routes.go` (`NewRouter`, the `/v1` block) mounts the product
  routes — today a single `r.Post("/echo", ...)`.
- `internal/openapi/openapi.go::Build()` **statically hard-codes**
  `Paths["/v1/echo"]`. That document feeds `clients/openapi.json` →
  `scripts/gen-clients.sh` → the generated Go/TS consumer SDKs.

There is **zero test asserting the two agree**: `openapi_test.go::TestBuild_RequiredPaths`
checks the doc in isolation, and there is no `chi.Walk` anywhere in the tree. A
clone that adds `/v1/validate-vat` per `ADAPT.md` gets a correct router but a
silently stale `/openapi.json` and stale generated clients — exactly the
per-product trap the framework is meant to eliminate. ADAPT already asks each
clone to edit `routes.go`; this turns that into a **one-place** declaration both
the router and the OpenAPI builder consume, and adds a guardrail.

## Module

- **Input:** one declarative entry per product endpoint (path, opaque operation
  string, summary), declared once.
- **Output:** the router mounts those routes AND `Build()` derives their OpenAPI
  `/v1` paths from the same source; a test fails if the two ever diverge.

## Decomposition

1. **Route-descriptor table** — a small slice/map of `{path, operation, summary}`
   (in `routes.go` or a new sibling `internal/server/routes_table.go` if cleaner).
   This becomes the only place `/v1` endpoint paths appear.
2. **`routes.go`** — the `/v1` block iterates the table to mount each invoke route;
   the `invoke` handler body (and the `billable_units < 1` → 502 trust-boundary
   check) is untouched — only *how* routes are mounted changes.
3. **`openapi/openapi.go`** — `Build()` derives the `/v1/*` `Paths` entries by
   reading the same table; the hard-coded `Paths["/v1/echo"]` literal is removed.
4. **Drift-guard test** (`routes_test.go` + `openapi_test.go`) — builds the real
   `NewRouter`, walks its mounted `/v1` POST patterns, and asserts that set equals
   the `/v1` paths in `openapi.Build()`; fails if either side adds/removes a route
   without the other.

## Acceptance

- A single in-repo declaration (slice/map of route descriptors) is the only place
  `/v1` endpoint paths appear; `routes.go` mounts by iterating it.
- `openapi.Build()` produces the `/v1/*` `Paths` from that same declaration — no
  hard-coded `Paths["/v1/echo"]` literal remains (grep returns zero hits).
- A new test builds the real router and asserts the set of mounted `/v1` POST
  patterns equals the set of `/v1` paths in `openapi.Build()` (drift guard).
- Existing `/v1/echo` behaviour, response envelope, headers, and all current
  `openapi_test.go` assertions still pass unchanged.
- System/public routes (`/healthz`, `/readyz`, `/metrics`, `/webhooks/stripe`,
  `/v1/billing/*`) remain exactly as today; the registry covers only the
  per-product `/v1` invoke routes (the ADAPT edit point).
- Each per-product invoke route still maps to exactly one opaque operation string
  forwarded to the worker (no proto change).
- `go test -race ./...` green in `gateway/`.

## Forbidden

- Editing `cmd/gateway/main.go` or `internal/auth/store.go` (PR #48 parallel-safety).
- Touching `gateway/proto/tool.proto`, the `billable_units < 1` → 502 check in
  `routes.go` (invariant #2 — leave the `invoke` handler body untouched),
  `billing/webhook.go` ordering, `usage/flusher.go`, `auth/keys.go` hash parity,
  `PrefixLen`, or `Store.Revoke`.
- Changing the worker SDK or the wire contract; adding any new config key.
- Widening into a code-generation or routing-framework swap — this is a
  single-source-of-truth + drift-guard change only, `chi` stays.
