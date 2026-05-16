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

func expireAt(now time.Time) time.Time {
	return time.Date(now.Year(), now.Month()+1, 2, 0, 0, 0, 0, time.UTC)
}

// reserveScript atomically: INCRs the counter by 1, sets the month-end expiry, and
// rolls back the increment if it would push the customer over their cap. Returns 1
// if the slot is reserved, 0 if the request would exceed the cap.
//
// The pre-fix code used a non-atomic GET-then-check pattern in the middleware: under
// stampede, N goroutines could all read `current < cap` before any of them committed,
// allowing soft-overshoot of (N-1) requests per stampede event. Reserving via an
// atomic INCR-and-check closes that race.
//
// Net behaviour: the recorder's later Add(units) still runs after worker success, so
// total counter movement per successful call = 1 (reserve) + units (recorder). The
// +1 of overhead is the trade-off for closing the race; for high-units-per-call
// products it's negligible.
var reserveScript = redis.NewScript(`
local key = KEYS[1]
local cap = tonumber(ARGV[1])
local exp = tonumber(ARGV[2])
local v = redis.call('INCR', key)
redis.call('EXPIREAT', key, exp)
if v > cap then
  redis.call('DECR', key)
  return 0
end
return 1
`)

// Reserve atomically checks the in-month counter against `cap` and, if there's room,
// reserves a single unit. Returns true if the request is admitted.
// `cap <= 0` means unlimited (always admit).
//
// Callers MUST pair an admitted Reserve with either:
//   - usage.Recorder.Record on successful worker invocation (adds the actual units), OR
//   - Refund if the request did not produce billable usage (worker failure, contract
//     rejection, bad request, etc.). The quota middleware does this via a deferred call
//     keyed on response status.
//
// Without the Refund path, transient worker outages would burn a slot from the
// customer's monthly cap without any usage_events row to bill — see PR #5 P1.
func (t *Tracker) Reserve(ctx context.Context, customerID uuid.UUID, cap int64) (bool, error) {
	if cap <= 0 {
		return true, nil
	}
	// Use UTC throughout so the key (which monthKey() builds in UTC) and the expiry
	// always reference the same calendar month, even on hosts running in a non-UTC TZ.
	now := time.Now().UTC()
	res, err := reserveScript.Run(ctx, t.cache,
		[]string{monthKey(customerID, now)},
		cap, expireAt(now).Unix(),
	).Int()
	if err != nil {
		return false, fmt.Errorf("reserve: %w", err)
	}
	return res == 1, nil
}

// Refund decrements the monthly counter by 1 to release a previously-Reserve'd slot
// when the request did not result in billable usage (failed worker, contract reject,
// invalid request). Idempotent at the Redis level — DECR on a missing key returns -1
// but we floor at 0 via the Lua so counters never go negative on a clock-skew refund
// after the month rolled over.
func (t *Tracker) Refund(ctx context.Context, customerID uuid.UUID) error {
	now := time.Now().UTC()
	_, err := refundScript.Run(ctx, t.cache, []string{monthKey(customerID, now)}).Int()
	if err != nil {
		return fmt.Errorf("refund: %w", err)
	}
	return nil
}

// refundScript decrements only if the key exists and the value is > 0. Prevents
// negative counters when a refund races with month-boundary expiry.
var refundScript = redis.NewScript(`
local key = KEYS[1]
local v = tonumber(redis.call('GET', key) or 0)
if v <= 0 then return 0 end
redis.call('DECR', key)
return 1
`)

// Current returns the customer's billable-unit count for the current calendar month.
// Missing key → 0. Errors are surfaced; callers may choose to fail-open.
// Kept for observability / dashboard use; the request-gating path uses Reserve.
func (t *Tracker) Current(ctx context.Context, customerID uuid.UUID) (uint64, error) {
	v, err := t.cache.Get(ctx, monthKey(customerID, time.Now())).Uint64()
	if errors.Is(err, redis.Nil) {
		return 0, nil
	}
	return v, err
}

// Add increments the customer's monthly counter by `units` after a successful worker call.
// The middleware already reserved +1 via Reserve; this adds the actual units consumed.
func (t *Tracker) Add(ctx context.Context, customerID uuid.UUID, units uint64) error {
	now := time.Now().UTC()
	key := monthKey(customerID, now)

	pipe := t.cache.Pipeline()
	pipe.IncrBy(ctx, key, int64(units))
	pipe.ExpireAt(ctx, key, expireAt(now))
	_, err := pipe.Exec(ctx)
	return err
}
