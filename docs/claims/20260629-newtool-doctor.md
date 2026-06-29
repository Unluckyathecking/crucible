# Claim — `new-tool.sh` preflight doctor

Directive for the small-claim worker. Grounded in `main` HEAD `7e8fc20`, direction PR #128
item 4, and the v0 P2.2 env-prefix drift bug.

## Target

`scripts/` in `unluckyathecking/crucible` (a doctor step invoked from / alongside
`new-tool.sh`), gated by `scripts/smoke-new-tool.sh`.

## Problem

Clone-and-adapt is the core promise, but silent adapt-misconfig slips through: the v0 P2.2
bug was a root-vs-`dashboard/` `.env.example` env-prefix mismatch. There is no preflight
check, so misconfig is only caught at first boot (or in production).

## Change

Add a preflight "doctor" step that asserts, before first boot:

1. **Env prefix / salt parity** — the env prefix and API-key salt match between the root and
   `dashboard/` `.env.example` files (the P2.2 class of drift).
2. **Symlink integrity** — `workers/active` resolves to a real worker directory.
3. **Route ↔ OpenAPI sync** — the routes registered in `gateway/internal/server/routes.go`
   match the OpenAPI/route registry (no endpoint registered without an OpenAPI entry or
   vice-versa).

The doctor must exit non-zero with a clear message on any mismatch.

## Expected outcome / acceptance

- A doctor script/step exists under `scripts/` and is reachable from the adapt flow.
- `bash scripts/smoke-new-tool.sh` stays green (doctor passes on a correctly-adapted tree).
- The doctor FAILS LOUDLY (non-zero + named cause) when drift is injected — demonstrate with
  at least the env-prefix mismatch case (a test/fixture or a smoke-test assertion that injects
  drift and asserts a non-zero exit).

## Constraints

- Stay within `scripts/**` (plus, if needed, a tiny fixture). Do not modify gateway runtime
  code, the frozen proto, or migrations.
- Keep it dependency-light (POSIX sh/bash + tools already used by the existing scripts); no new
  heavy tooling.
- This claim owns `scripts/**` for this cycle; it does not overlap the conformance harness
  (which is scoped to `workers/conformance/**`).
