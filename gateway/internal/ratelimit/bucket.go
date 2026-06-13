// Package ratelimit enforces per-customer per-minute request caps using a SLIDING
// window in Redis (a sorted set keyed on timestamps). Fail-open: if Redis is
// unreachable the call is allowed.
//
// Why sliding window not fixed: the original fixed-minute bucket let customers send
// `perMinute` calls at second 59 and another `perMinute` at second 61 — 2× the
// advertised rate across the boundary. Sliding window counts the last 60 s exactly,
// so the limit is honoured no matter where in the minute the requests land.
package ratelimit

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"

	"github.com/Unluckyathecking/crucible/gateway/internal/observability"
)

// ErrLimited is returned when the customer has exceeded their per-minute quota.
var ErrLimited = errors.New("rate limited")

type Bucket struct {
	cache *redis.Client
}

func New(cache *redis.Client) *Bucket { return &Bucket{cache: cache} }

const windowSeconds = 60

// allowScript runs as an atomic Lua script (EVAL — not a MULTI/EXEC transaction). Logic:
//  1. ZREMRANGEBYSCORE removes timestamps older than now-window.
//  2. ZCARD returns the count of remaining (in-window) timestamps.
//  3. If count < limit, ZADD (score=now, member=unique-id) and return {1, remaining}.
//  4. Else return {0, 0} (denied) — do NOT add, so a flooded customer doesn't keep
//     pushing their own window forward.
//
// Member uniqueness matters: ZADD upserts on duplicate members. If two calls
// arrived in the same millisecond and used "now" as both score AND member, only
// one ZSET entry would exist after both — silently undercounting under burst.
// Caller passes a UUID per call to guarantee distinct members.
//
// Using a Lua script (vs pipelined commands) keeps the check-and-add atomic;
// otherwise two concurrent goroutines could both pass the count check before
// either ZADDs.
//
// The script returns a 2-element array {allowed, remaining}: allowed=1 with the
// post-ZADD remaining count, or {0, 0} when denied. This is additive — the existing
// semantics (check-and-add) are unchanged; we only add the remaining count to the
// return value so the caller can emit rate-limit headers without a second round-trip.
var allowScript = redis.NewScript(`
local key    = KEYS[1]
local now    = tonumber(ARGV[1])
local win    = tonumber(ARGV[2])
local limit  = tonumber(ARGV[3])
local member = ARGV[4]

redis.call('ZREMRANGEBYSCORE', key, 0, now - win * 1000)
local count = redis.call('ZCARD', key)
if count >= limit then
  return {0, 0}
end
redis.call('ZADD', key, now, member)
redis.call('PEXPIRE', key, (win + 10) * 1000)
return {1, limit - count - 1}
`)

// noRemaining is returned by Allow when the remaining count is not reliably known —
// either the plan is unlimited (perMinute <= 0) or Redis was unreachable (fail-open).
// Callers MUST omit rate-limit headers when they receive this sentinel.
const noRemaining = -1

// Allow reports whether the customer is within their per-minute limit and returns
// the number of requests remaining in the current window. Returns (noRemaining, nil)
// when the count is unreliable (unlimited plan or Redis error — fail-open).
// Returns (0, ErrLimited) when the customer has exceeded their limit.
// perMinute <= 0 means "no limit".
// RateLimitKey returns the Redis sorted-set key for a customer's sliding-window rate limit.
// Exported so test-harness cleanup can delete the key using the same format as production.
func RateLimitKey(customerID string) string { return "rl:" + customerID }

func (b *Bucket) Allow(ctx context.Context, customerID string, perMinute int) (int, error) {
	if perMinute <= 0 {
		return noRemaining, nil
	}
	key := RateLimitKey(customerID)
	now := time.Now().UnixMilli()
	member := uuid.NewString()

	res, err := allowScript.Run(ctx, b.cache, []string{key}, now, windowSeconds, perMinute, member).Slice()
	if err != nil {
		// Fail-open. Better to bill an extra request than refuse legit traffic on a Redis blip.
		// Count it so operators can alert on a degraded limiter instead of it being silent.
		// Return noRemaining so the caller does not emit fabricated header values.
		observability.RateLimitFailOpenTotal.Inc()
		return noRemaining, nil
	}
	if len(res) != 2 {
		observability.RateLimitFailOpenTotal.Inc()
		return noRemaining, nil
	}
	allowed, ok1 := res[0].(int64)
	remaining, ok2 := res[1].(int64)
	if !ok1 || !ok2 {
		// Unexpected Lua return type (Redis version mismatch, protocol error). Fail-open
		// and omit headers rather than emitting fabricated counts.
		observability.RateLimitFailOpenTotal.Inc()
		return noRemaining, nil
	}
	if allowed == 0 {
		return 0, ErrLimited
	}
	return int(remaining), nil
}
