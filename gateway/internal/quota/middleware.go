package quota

import (
	"context"
	"net/http"
	"time"

	"github.com/rs/zerolog/log"

	"github.com/Unluckyathecking/crucible/gateway/internal/auth"
	"github.com/Unluckyathecking/crucible/gateway/internal/billing"
)

// Middleware enforces per-customer monthly billable-unit caps via an ATOMIC reserve.
// MUST be mounted AFTER auth.Middleware (it depends on auth context).
//
// Flow:
//  1. Reserve(+1) atomically against the customer's monthly counter. Returns 429 on cap.
//  2. Seed a record-signal into context so the downstream usage recorder can flip
//     it true on a successful insert.
//  3. Run the handler.
//  4. After the handler returns, if the recorder never flipped the signal (worker
//     failed, response carried an error envelope, recorder write failed, etc.),
//     refund the reserve against the EXACT key Reserve used.
//
// Two non-obvious choices both motivated by Codex's PR #5 review:
//   - Signal-based refund (not HTTP-status-based) — a worker returning HTTP 200 with a
//     structured error envelope skips the recorder, which would have escaped a status-only
//     refund gate. The signal is set only when usage is actually persisted.
//   - Key-based refund (not customer+now-based) — a request that reserves at 23:59 UTC
//     and refunds at 00:01 the next day must release the previous month's counter,
//     not the (empty) new month key.
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
			admitted, reservedKey, err := t.Reserve(r.Context(), key.Customer.ID, cap)
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

			// Reserve succeeded. Plant a record-signal so the recorder can tell us whether
			// it actually wrote a usage row downstream.
			ctx, signal := withRecordSignal(r.Context())
			next.ServeHTTP(w, r.WithContext(ctx))

			if !signal.recorded.Load() {
				// No usage row was written for this request — refund the reserve against
				// the EXACT key Reserve used (handles midnight-UTC boundaries correctly).
				// Use a fresh background context: the request context may already be canceled
				// by client disconnect, but the refund still needs to run.
				bg, cancel := context.WithTimeout(context.Background(), 2*time.Second)
				defer cancel()
				if err := t.RefundAt(bg, reservedKey); err != nil {
					log.Warn().
						Err(err).
						Str("customer", key.Customer.ID.String()).
						Str("key", reservedKey).
						Msg("quota refund failed; counter may drift +1")
				}
			}
		})
	}
}
