# worker:claim — Emit excludes inactive (soft-deleted) endpoints test

**Lane:** `worker:claim` · **Seeded:** 2026-07-05

**Target:** `gateway/internal/webhookout/emitter_test.go` (extend; ~40 LOC) covering the
`we.active = TRUE` filter in `Emit` at `emitter.go:120`.

**Directive:** Add a test proving `Emit` inserts ZERO `webhook_deliveries` rows for a soft-deleted
(`active = FALSE`) endpoint. This is the invariant that `DeleteEndpoint` (which sets `active = FALSE`,
`endpoints.go:192`) actually stops *new* events from being queued to that endpoint. `emitter_test.go`
tests the subscription filter (`TestEmit_SubscriptionFilter_UnsubscribedEndpointGetsZeroRows`, line
312) and nil-DB safety, but no test exercises the `active` clause — grep for `active`/`inactive`/
`deactiv` across `webhookout/*_test.go` finds only the seed helper's `active` INSERT column, no
assertion.

**Acceptance:** seed an endpoint with `active = FALSE` subscribed to the fired event type; assert
`Emit` produces zero `webhook_deliveries` rows for it (and, if an active sibling is seeded, exactly
one for the active one). `go test -race ./gateway/internal/webhookout/...` green against real
Postgres.

**Constraints:** Test-only; reuse the existing seed helper + real-Postgres harness in
`emitter_test.go`; no production edits. Disjoint from the concurrent
`webhook-endpoint-lifecycle-completion` primary (which edits `endpoints.go`/`endpoints_http.go`/
`endpoints_test.go`/`routes.go`/`openapi.go`, NOT `emitter*.go`) — same package, different file,
parallel-safe.
