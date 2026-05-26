# Plan: auth-cache-negative-prefix

**Sprint:** 2026-05-26

## Context

The current `Store.Lookup` (gateway/internal/auth/store.go) has two DoS-relevant cold paths:

**Case A** (covered by open PRs #34/#43/#44): Redis hot-path hit, prefix found, but hash doesn't match → code falls through to Postgres.

**Case B** (NOT covered by any open PR — this spec): Redis miss + Postgres ErrNoRows → function returns `ErrKeyNotFound` with **no caching**. An attacker sending requests with random but plausible prefixes (not in DB) hammers Postgres with one O(log n) index scan per request, indefinitely.

This spec adds a 30-second negative sentinel cache for Case B only. "auth:miss:<prefix>" → "1"; subsequent lookups skip Postgres for 30s. The normal "auth:<prefix>" cache key is untouched.

## Scope

- `gateway/internal/auth/store.go`
- `gateway/internal/auth/store_test.go`

## What to implement

In `Lookup`, after the Postgres query returns `pgx.ErrNoRows`:

```go
if errors.Is(err, pgx.ErrNoRows) {
    // Cache a short-lived sentinel to absorb repeated misses.
    _ = s.cache.Set(ctx, "auth:miss:"+prefix, "1", 30*time.Second).Err()
    return nil, ErrKeyNotFound
}
```

At the top of the Redis hot path, before attempting the normal cache lookup, check for the miss sentinel:

```go
if v, err := s.cache.Get(ctx, "auth:miss:"+prefix).Result(); err == nil && v == "1" {
    return nil, ErrKeyNotFound
}
```

This sentinel check comes BEFORE the normal `"auth:"+prefix` lookup so cached-miss short-circuits in one Redis round-trip.

## Acceptance criteria

1. `go build ./gateway/...` exits 0
2. `go test -race ./gateway/internal/auth/...` exits 0; all existing tests pass
3. New test `TestLookup_NegativePrefixCache`: first call with an unknown prefix → one Postgres query, returns ErrKeyNotFound, sentinel key `"auth:miss:<prefix>"` exists with TTL ≤ 30s; second call with same prefix → Redis returns sentinel, zero Postgres queries, returns ErrKeyNotFound
4. Sentinel TTL verifiable: `s.cache.TTL(ctx, "auth:miss:"+prefix)` returns a positive duration ≤ 30s immediately after the first miss
5. Normal auth flow (valid key, matching hash) is unaffected — all existing passing tests pass
6. diff in `store.go` is ≤ 40 lines

## Forbidden

- No changes to Redis timeout values (DialTimeout/ReadTimeout/WriteTimeout are set; do not touch)
- No changes to the `Lookup` signature, `NewStore` signature, `Close`, or `Revoke`
- No changes to `redis.go` or any other file
- Do not generalise: scope is prefix-miss (ErrNoRows) only — do not add sentinel for hash-mismatch case (separate concern, covered by open PRs)
- Do not add a `MissTTL` config field — hard-code 30s as an unexported constant
