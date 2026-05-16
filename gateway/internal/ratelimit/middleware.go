package ratelimit

import (
	"errors"
	"net/http"

	"github.com/Unluckyathecking/crucible/gateway/internal/auth"
	"github.com/Unluckyathecking/crucible/gateway/internal/billing"
	"github.com/Unluckyathecking/crucible/gateway/internal/observability"
)

// Middleware enforces per-customer rate limits based on their plan.
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
			if err := bucket.Allow(r.Context(), key.Customer.ID.String(), limit); err != nil {
				if errors.Is(err, ErrLimited) {
					observability.RateLimitedTotal.Inc()
					w.Header().Set("Content-Type", "application/json")
					w.Header().Set("Retry-After", "60")
					w.WriteHeader(http.StatusTooManyRequests)
					_, _ = w.Write([]byte(`{"error":{"code":"RATE_LIMITED","message":"rate limit exceeded","retryable":true}}`))
					return
				}
			}
			next.ServeHTTP(w, r)
		})
	}
}
