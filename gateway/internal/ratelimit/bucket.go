// Package ratelimit enforces per-customer per-minute request caps using a fixed-window
// counter in Redis. Fail-open: if Redis is unreachable the call is allowed (logged).
package ratelimit

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// ErrLimited is returned when the customer has exceeded their per-minute quota.
var ErrLimited = errors.New("rate limited")

type Bucket struct {
	cache *redis.Client
}

func New(cache *redis.Client) *Bucket { return &Bucket{cache: cache} }

// Allow returns nil if the customer is under their per-minute limit; ErrLimited otherwise.
// Fixed-window counter: simplest correct primitive for "N requests per minute".
// perMinute <= 0 means "no limit".
func (b *Bucket) Allow(ctx context.Context, customerID string, perMinute int) error {
	if perMinute <= 0 {
		return nil
	}
	minute := time.Now().Unix() / 60
	key := fmt.Sprintf("rl:%s:%d", customerID, minute)

	pipe := b.cache.Pipeline()
	cnt := pipe.Incr(ctx, key)
	// Expire a bit past the window so the key survives clock skew.
	pipe.Expire(ctx, key, 70*time.Second)
	if _, err := pipe.Exec(ctx); err != nil {
		// Fail-open. Middleware logs. Better to bill an extra request than refuse legit traffic on Redis blips.
		return nil
	}
	if cnt.Val() > int64(perMinute) {
		return ErrLimited
	}
	return nil
}
