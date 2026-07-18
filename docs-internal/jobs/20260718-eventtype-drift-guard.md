# worker:claim brief — Go↔TS webhook event-type parity drift guard

## Problem (verified against HEAD d4093f9)
`gateway/internal/events/events.go` `AllEventTypes` is the single source of truth for the
webhook event-type set (7 types). The dashboard keeps a **hand-copied** parallel list
`WEBHOOK_EVENT_TYPES` in `dashboard/lib/db.ts:530` (comment at :525 literally says it
"mirrors gateway/internal/events.AllEventTypes"), re-mirrored again in
`dashboard/app/api/webhooks/route.test.ts`. `webhookout/emitter.go:173-179` documents the
risk ("Non-Go registration paths must keep their own event-type list in sync").

The Go side is guarded (`openapi.go` panics at Build() if its descriptor list drifts from
`AllEventTypes`), but **nothing verifies the Go↔TS parity** — the TS constant is hardcoded
and never checked against the Go source. The repo already has analogous drift guards
(`asyncroutes_drift_test.go`, `TestV1RoutesDriftGuard`, CLAUDE.md invariant #5 for the
Go/TS key-hash mirror) — event types are the missing case.

## Directive
Add an automated parity guard so a change to `AllEventTypes` that isn't mirrored into
`dashboard/lib/db.ts` `WEBHOOK_EVENT_TYPES` fails a check. Preferred shape (pick the one
that fits the repo's existing drift-guard idiom):
- a Go test that emits the canonical `AllEventTypes` list and a small script/test that
  diffs it against the TS constant, OR
- a dashboard (vitest) test that reads the canonical list from a generated/exported
  fixture and asserts set-equality with `WEBHOOK_EVENT_TYPES`.

## Acceptance
- A test/check fails if `AllEventTypes` and `WEBHOOK_EVENT_TYPES` differ (add/remove/rename).
- Order-insensitive set equality (both lists compared as sets).
- Passes green on current HEAD (the two lists are in sync today).
- No change to `AllEventTypes` membership, `emitter.go`, or the openapi panic guard.

## Forbidden
- Do not change the event-type set or any runtime behaviour — this is a guard only.
- Do not touch `gateway/proto/**` (frozen), billing/auth/usage/quota, or `dashboard/app/page.tsx`.
- Do not introduce a build-time codegen step that rewrites `db.ts` (a *check*, not a generator).
