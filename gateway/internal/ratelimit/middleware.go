package ratelimit

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/Unluckyathecking/crucible/gateway/internal/apierror"
	"github.com/Unluckyathecking/crucible/gateway/internal/auth"
	"github.com/Unluckyathecking/crucible/gateway/internal/billing"
	"github.com/Unluckyathecking/crucible/gateway/internal/httputil"
	mwpkg "github.com/Unluckyathecking/crucible/gateway/internal/middleware"
	"github.com/Unluckyathecking/crucible/gateway/internal/observability"
)

// Middleware enforces per-customer rate limits based on their plan and sets
// RateLimit-* / X-RateLimit-* headers on every response where the count is known.
// MUST be mounted after auth.Middleware — depends on auth context.
func Middleware(bucket *Bucket, plans *billing.PlanCache) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			key := auth.FromContext(r.Context())
			if key == nil {
				// Should never happen post-auth; fail-safe by skipping the limiter.
				next.ServeHTTP(w, r)
				return
			}
			limit := plans.RatePerMinute(r.Context(), key.Customer.Plan)
			remaining, err := bucket.Allow(r.Context(), key.Customer.ID.String(), limit)
			// Capture once after Allow returns so both 429 and success paths use the
			// same instant for RateLimit-Reset — eliminates any split-millisecond drift
			// between the two branches.
			resetAt := time.Now().Add(time.Minute)
			// Allow returns only nil or ErrLimited; errors.Is(nil, ErrLimited) is false.
			if errors.Is(err, ErrLimited) {
				observability.RateLimitedTotal.Inc()
				// limit > 0 is guaranteed here (unlimited skips Allow), but guard anyway.
				if limit > 0 {
					httputil.SetRateLimitHeaders(w, limit, 0, resetAt)
				}
				w.Header().Set("Retry-After", strconv.Itoa(windowSeconds))
				rid, _ := r.Context().Value(mwpkg.RequestIDKey).(string)
				apierror.Write(w, rid, http.StatusTooManyRequests, apierror.RATE_LIMITED, "rate limit exceeded", true)
				return
			}
			// Emit rate-limit headers only when the count is reliable (not an unlimited
			// plan and not a Redis-error fail-open path — both return noRemaining).
			if remaining != noRemaining {
				httputil.SetRateLimitHeaders(w, limit, remaining, resetAt)
			}
			next.ServeHTTP(w, r)
		})
	}
}
