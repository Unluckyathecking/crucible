// Package quota tracks per-customer monthly usage in Redis and exposes a middleware
// that rejects requests when a customer is over their plan's monthly_unit_cap.
//
// Redis is the right home for this counter (vs Postgres): incremented on every successful
// call, read on every gated call, and naturally per-month-scoped via key expiry.
package quota

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

type Tracker struct {
	cache *redis.Client
}

func New(cache *redis.Client) *Tracker { return &Tracker{cache: cache} }

// monthKey returns "quota:<customer>:<YYYY-MM>" in UTC.
func monthKey(customerID uuid.UUID, now time.Time) string {
	return fmt.Sprintf("quota:%s:%s", customerID, now.UTC().Format("2006-01"))
}

// Current returns the customer's billable-unit count for the current calendar month.
// Missing key → 0. Errors are surfaced; callers may choose to fail-open.
func (t *Tracker) Current(ctx context.Context, customerID uuid.UUID) (uint64, error) {
	v, err := t.cache.Get(ctx, monthKey(customerID, time.Now())).Uint64()
	if errors.Is(err, redis.Nil) {
		return 0, nil
	}
	return v, err
}

// Add increments the customer's monthly counter by `units`. Sets an expiry that survives
// past month-end (a grace window) so late flushes don't lose the value.
func (t *Tracker) Add(ctx context.Context, customerID uuid.UUID, units uint64) error {
	now := time.Now().UTC()
	key := monthKey(customerID, now)
	expireAt := time.Date(now.Year(), now.Month()+1, 2, 0, 0, 0, 0, time.UTC)

	pipe := t.cache.Pipeline()
	pipe.IncrBy(ctx, key, int64(units))
	pipe.ExpireAt(ctx, key, expireAt)
	_, err := pipe.Exec(ctx)
	return err
}
