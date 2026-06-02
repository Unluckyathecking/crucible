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
	"fmt"
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

// allowScript runs as a single atomic Redis transaction. Logic:
//  1. ZREMRANGEBYSCORE removes timestamps older than now-window.
//  2. ZCARD returns the count of remaining (in-window) timestamps.
//  3. If count < limit, ZADD (score=now, member=unique-id) and return 1 (allowed).
//  4. Else return 0 (denied) — do NOT add, so a flooded customer doesn't keep
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
var allowScript = redis.NewScript(`
local key    = KEYS[1]
local now    = tonumber(ARGV[1])
local win    = tonumber(ARGV[2])
local limit  = tonumber(ARGV[3])
local member = ARGV[4]

redis.call('ZREMRANGEBYSCORE', key, 0, now - win * 1000)
local count = redis.call('ZCARD', key)
if count >= limit then
  return 0
end
redis.call('ZADD', key, now, member)
redis.call('PEXPIRE', key, (win + 10) * 1000)
return 1
`)

// Allow returns nil if the customer is under their per-minute limit; ErrLimited otherwise.
// perMinute <= 0 means "no limit".
func (b *Bucket) Allow(ctx context.Context, customerID string, perMinute int) error {
	if perMinute <= 0 {
		return nil
	}
	key := fmt.Sprintf("rl:%s", customerID)
	now := time.Now().UnixMilli()
	member := uuid.NewString()

	res, err := allowScript.Run(ctx, b.cache, []string{key}, now, windowSeconds, perMinute, member).Int()
	if err != nil {
		// Fail-open. Better to bill an extra request than refuse legit traffic on a Redis blip.
		// Count it so operators can alert on a degraded limiter instead of it being silent.
		observability.RateLimitFailOpenTotal.Inc()
		return nil
	}
	if res == 0 {
		return ErrLimited
	}
	return nil
}
