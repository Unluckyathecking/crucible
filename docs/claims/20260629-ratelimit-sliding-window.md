# Claim — sliding-window rate limiter

Directive for the small-claim worker. Grounded in `main` HEAD `7e8fc20`, direction PR #128
item 1, and the REVIEW "Open at v1" boundary-burst gap.

## Target

`gateway/internal/ratelimit/` (`bucket.go` + its middleware) in `unluckyathecking/crucible`.

## Problem

The limiter had a fixed-minute window. A burst straddling the window boundary could admit up to
~2x the configured per-minute limit — the one correctness gap REVIEW left open at v1. The
client-facing `RateLimit-*` headers (#109) already landed, so the response contract is stable.

The sliding-window implementation (`allowScript` Lua EVAL: ZREMRANGEBYSCORE → ZCARD → ZADD →
PEXPIRE) landed in `main` before this PR was filed. This PR's contribution is the
boundary-burst proof test that the original implementation lacked.

## Change

Add `TestAllow_BoundaryBurstCapped` in `ratelimit_test.go`: seed the Redis sorted set with
`limit` entries timestamped 30 s in the past (within the 60 s sliding window but in what a
fixed-minute bucket considers the "previous" minute), then assert all subsequent `Allow()` calls
return `ErrLimited`. Keep the Redis-backed, per-customer design; the sorted-set Lua script is
already atomic — no read-modify-write race.

## Expected outcome / acceptance

- `bucket.go` enforces the count atomically via the existing sliding-window Lua script.
- `TestAllow_BoundaryBurstCapped` in `ratelimit_test.go` seeds Redis with entries from 30 s ago
  and asserts all subsequent `Allow()` calls are rejected — proving the sliding window counts
  prior-window entries that are still within 60 s (a fixed-window bucket would miss them and
  allow 2× the limit).
- Client-facing `RateLimit-*` header values remain consistent with the sliding-window accounting.
- `go test -race ./...` green in `gateway/`.

## Constraints

- Stay within `gateway/internal/ratelimit/**` (+ its test). Do not touch auth, billing,
  proxy, quota, or the frozen proto.
- Preserve the existing middleware signature and the `RateLimit-*` header contract from #109.
  The limiter is keyed per-customer only (`rl:<customerID>`); there is no per-route dimension.
- Use a real Redis in tests (no mocks), per repo testing policy.
