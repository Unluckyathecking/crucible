package quota

import (
	"net/http"

	"github.com/Unluckyathecking/crucible/gateway/internal/auth"
	"github.com/Unluckyathecking/crucible/gateway/internal/billing"
)

// Middleware enforces per-customer monthly billable-unit caps.
// MUST be mounted AFTER auth.Middleware (it depends on auth context).
//
// Fail-open on Redis errors — better to bill an over-quota request than to refuse
// service when our quota store blips. Operators see this via Prometheus / logs.
func Middleware(t *Tracker, plans *billing.PlanCache) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			key := auth.FromContext(r.Context())
			if key == nil {
				next.ServeHTTP(w, r)
				return
			}
			cap := plans.MonthlyCap(r.Context(), key.Customer.Plan)
			if cap == 0 {
				// 0 means unlimited (e.g. Business tier).
				next.ServeHTTP(w, r)
				return
			}
			current, err := t.Current(r.Context(), key.Customer.ID)
			if err != nil {
				// Fail-open and let the request through. Operators see this in observability.
				next.ServeHTTP(w, r)
				return
			}
			if int64(current) >= cap {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusTooManyRequests)
				_, _ = w.Write([]byte(`{"error":{"code":"QUOTA_EXCEEDED","message":"monthly usage quota reached","retryable":false}}`))
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
