# Claim — sliding-window rate limiter

Directive for the small-claim worker. Grounded in `main` HEAD `7e8fc20`, direction PR #128
item 1, and the REVIEW "Open at v1" boundary-burst gap.

## Target

`gateway/internal/ratelimit/` (`bucket.go` + its middleware) in `unluckyathecking/crucible`.

## Problem

The limiter is a fixed-minute window. A burst straddling the window boundary can admit up to
~2x the configured per-minute limit — the one correctness gap REVIEW left open at v1. The
client-facing `RateLimit-*` headers (#109) already landed, so the response contract is stable.

## Change

Replace the fixed-minute window with an atomic sliding window (or token bucket) so a
boundary-straddling burst is capped at the configured limit, not ~2x. Keep the Redis-backed
design and the per-customer per-route keying; the multi-step Redis update must be atomic
(Lua `EVAL` or an equivalent atomic primitive) — no read-modify-write race.

## Expected outcome / acceptance

- `bucket.go` implements a sliding window/token bucket; the count is enforced atomically.
- A new case in `ratelimit_test.go` sends a burst across a window boundary and asserts it is
  capped at the configured limit (the test must FAIL against the current fixed-window code
  and PASS after the change).
- Client-facing `RateLimit-*` header values stay consistent with the new accounting.
- `go test -race ./...` green in `gateway/`.

## Constraints

- Stay within `gateway/internal/ratelimit/**` (+ its test). Do not touch auth, billing,
  proxy, quota, or the frozen proto.
- Preserve the existing middleware signature and the `RateLimit-*` header contract from #109.
- Use a real Redis in tests (no mocks), per repo testing policy.
