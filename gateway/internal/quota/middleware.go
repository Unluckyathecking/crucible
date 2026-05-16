package quota

import (
	"context"
	"net/http"
	"time"

	"github.com/rs/zerolog/log"

	"github.com/Unluckyathecking/crucible/gateway/internal/auth"
	"github.com/Unluckyathecking/crucible/gateway/internal/billing"
	"github.com/Unluckyathecking/crucible/gateway/internal/httputil"
)

// Middleware enforces per-customer monthly billable-unit caps via an ATOMIC reserve.
// MUST be mounted AFTER auth.Middleware (it depends on auth context).
//
// Flow:
//  1. Reserve(+1) atomically against the customer's monthly counter. Returns 429 on cap.
//  2. Run the handler with a status-capturing ResponseWriter wrapper.
//  3. After the handler returns, if the response indicated failure (non-2xx) the
//     request didn't produce billable usage, so refund the reserve. This is the
//     compensating decrement for the P1 issue Codex flagged: previously, a transient
//     worker outage would consume a slot from the cap without any usage_events row.
//
// The atomic INCR-and-rollback in Reserve closes the soft-overshoot race that the
// pre-fix non-atomic GET-then-check had.
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
			admitted, err := t.Reserve(r.Context(), key.Customer.ID, cap)
			if err != nil {
				// Fail-open and let the request through. Operators see this in observability.
				next.ServeHTTP(w, r)
				return
			}
			if !admitted {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusTooManyRequests)
				_, _ = w.Write([]byte(`{"error":{"code":"QUOTA_EXCEEDED","message":"monthly usage quota reached","retryable":false}}`))
				return
			}

			// Reserve succeeded. Capture response status so we can refund if the
			// request didn't produce billable usage.
			rec := httputil.NewStatusRecorder(w)
			next.ServeHTTP(rec, r)

			// Non-2xx means: worker failure, contract reject (502 WORKER_BAD_RESPONSE),
			// invalid request (400 BAD_REQUEST), upstream rejection — none of which
			// produced a usage_events row. Refund the reserved slot.
			//
			// Best-effort: a refund failure leaves the counter inflated by 1, which is
			// the same as not having the refund at all. Worth logging but never blocking.
			if rec.Status < 200 || rec.Status >= 300 {
				// Use a fresh background context — the request context may already be canceled
				// by the time the handler returns (e.g. client disconnect). Refund still needs to run.
				bg, cancel := context.WithTimeout(context.Background(), 2*time.Second)
				defer cancel()
				if err := t.Refund(bg, key.Customer.ID); err != nil {
					log.Warn().
						Err(err).
						Str("customer", key.Customer.ID.String()).
						Int("status", rec.Status).
						Msg("quota refund failed; counter may drift +1")
				}
			}
		})
	}
}
