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
// rolls back the increment if it would push the customer over their cap. Returns a
// 2-element array {admitted, current}: admitted=1 with the post-INCR counter value,
// or {0, cap} when denied (counter rolled back to cap via DECR).
//
// The pre-fix code used a non-atomic GET-then-check pattern in the middleware: under
// stampede, N goroutines could all read `current < cap` before any of them committed,
// allowing soft-overshoot of (N-1) requests per stampede event. Reserving via an
// atomic INCR-and-check closes that race.
//
// Returning the counter value is additive — the check-and-INCR semantics are unchanged.
// The caller computes remaining = cap − current without a second Redis round-trip.
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
  return {0, cap}
end
return {1, v}
`)

// Reserve atomically checks the in-month counter against `cap` and, if there's room,
// reserves a single unit. Returns `(admitted, reservedKey, current, err)`.
// `reservedKey` is the exact Redis key the reserve was applied to — callers MUST pass
// it back to RefundAt when releasing the reservation. This is what makes the refund
// path correct across midnight-UTC boundaries: a request that reserves at 23:59 and
// refunds at 00:01 must release the *previous* month's counter, not the new month's
// empty key.
//
// `current` is the counter value after this operation:
//   - admitted=true: the post-INCR value (remaining = cap − current).
//   - admitted=false: cap (counter rolled back; remaining = 0).
//
// `cap <= 0` means unlimited (always admit; empty reservedKey; current=0).
//
// Callers MUST pair an admitted Reserve with either:
//   - usage.Recorder.Record on successful worker invocation (adds the actual units), OR
//   - RefundAt if the request did not produce billable usage (worker failure, contract
//     rejection, bad request, HTTP 200 carrying an error envelope, etc.). The quota
//     middleware drives this via a context-based "was usage recorded" signal that the
//     recorder flips on a successful insert.
//
// Without the refund path, transient worker outages would burn a slot from the
// customer's monthly cap without any usage_events row to bill — see PR #5 P1.
func (t *Tracker) Reserve(ctx context.Context, customerID uuid.UUID, cap int64) (bool, string, int64, error) {
	if cap <= 0 {
		return true, "", 0, nil
	}
	// Use UTC throughout so the key (which monthKey() builds in UTC) and the expiry
	// always reference the same calendar month, even on hosts running in a non-UTC TZ.
	now := time.Now().UTC()
	key := monthKey(customerID, now)
	res, err := reserveScript.Run(ctx, t.cache,
		[]string{key},
		cap, expireAt(now).Unix(),
	).Slice()
	if err != nil {
		return false, "", 0, fmt.Errorf("reserve: %w", err)
	}
	admitted, _ := res[0].(int64)
	current, _ := res[1].(int64)
	return admitted == 1, key, current, nil
}

// RefundAt decrements the counter at the specified Redis key by 1. The key MUST come
// from a prior Reserve() call to handle the month-boundary case correctly:
// a request that starts at 23:59:59 UTC and refunds at 00:00:01 the next day refunds
// the PREVIOUS month's reservation rather than touching the (empty) new month key.
// Idempotent: a Lua-guarded floor at zero prevents counters going negative on
// clock-skew refunds after month rollover + key expiry.
func (t *Tracker) RefundAt(ctx context.Context, reservedKey string) error {
	if reservedKey == "" {
		// Unlimited-tier reserve (cap=0) returns empty key; nothing to refund.
		return nil
	}
	_, err := refundScript.Run(ctx, t.cache, []string{reservedKey}).Int()
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
