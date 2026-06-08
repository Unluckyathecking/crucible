package ratelimit

import (
	"errors"
	"net/http"
	"time"

	"github.com/Unluckyathecking/crucible/gateway/internal/auth"
	"github.com/Unluckyathecking/crucible/gateway/internal/billing"
	"github.com/Unluckyathecking/crucible/gateway/internal/httputil"
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
			// Capture once so both the 429 path and the success path emit the same
			// RateLimit-Reset timestamp regardless of which branch executes.
			resetAt := time.Now().Add(windowSeconds * time.Second)
			if err != nil {
				if errors.Is(err, ErrLimited) {
					observability.RateLimitedTotal.Inc()
					// limit > 0 is guaranteed here (unlimited skips Allow), but guard anyway.
					if limit > 0 {
						httputil.SetRateLimitHeaders(w, limit, 0, resetAt)
					}
					w.Header().Set("Content-Type", "application/json")
					w.Header().Set("Retry-After", "60")
					w.WriteHeader(http.StatusTooManyRequests)
					_, _ = w.Write([]byte(`{"error":{"code":"RATE_LIMITED","message":"rate limit exceeded","retryable":true}}`))
					return
				}
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
