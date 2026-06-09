package quota

import (
	"context"
	"net/http"
	"time"

	"github.com/rs/zerolog/log"

	"github.com/Unluckyathecking/crucible/gateway/internal/apierror"
	"github.com/Unluckyathecking/crucible/gateway/internal/auth"
	"github.com/Unluckyathecking/crucible/gateway/internal/billing"
	"github.com/Unluckyathecking/crucible/gateway/internal/httputil"
	mwpkg "github.com/Unluckyathecking/crucible/gateway/internal/middleware"
	"github.com/Unluckyathecking/crucible/gateway/internal/observability"
)

// Middleware enforces per-customer monthly billable-unit caps via an ATOMIC reserve
// and sets X-Quota-* headers on every response where the count is known.
// MUST be mounted AFTER auth.Middleware (it depends on auth context).
//
// Flow:
//  1. Reserve(+1) atomically against the customer's monthly counter. Returns 429 on cap.
//  2. Emit X-Quota-* headers (both admit and deny paths); omit on Redis error or
//     unlimited plan so no fabricated values escape.
//  3. Seed a record-signal into context so the downstream usage recorder can flip
//     it true on a successful insert.
//  4. Run the handler.
//  5. After the handler returns, if the recorder never flipped the signal (worker
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
				// 0 means unlimited (e.g. Business tier); omit quota headers to avoid implying a cap.
				next.ServeHTTP(w, r)
				return
			}
			admitted, reservedKey, current, resetAt, err := t.Reserve(r.Context(), key.Customer.ID, cap)
			if err != nil {
				// Fail-open and let the request through. No reliable count, so omit quota
				// headers to avoid emitting fabricated values. Count it so operators can alert.
				observability.QuotaFailOpenTotal.Inc()
				next.ServeHTTP(w, r)
				return
			}

			// Clamp to zero: cap-current should never go negative given the Lua contract
			// (denied returns {0, cap} so remaining = cap - cap = 0), but be defensive at
			// this trust boundary so we never emit X-Quota-Remaining: -1 to a customer.
			remaining := cap - current
			if remaining < 0 {
				// This should never happen given the Lua contract, but log so operators
				// can detect a script bug rather than silently papering over it.
				log.Warn().
					Str("customer", key.Customer.ID.String()).
					Int64("cap", cap).
					Int64("current", current).
					Msg("quota remaining went negative; clamping to zero (Lua contract breach?)")
				remaining = 0
			}

			if !admitted {
				// Set all headers before writing the status code. Any Header().Set call after
				// WriteHeader is silently ignored by http.ResponseWriter.
				// Use resetAt from Reserve so the header matches the actual Redis EXPIREAT.
				httputil.SetQuotaHeaders(w, cap, remaining, resetAt)
				rid, _ := r.Context().Value(mwpkg.RequestIDKey).(string)
				apierror.Write(w, rid, http.StatusTooManyRequests, apierror.QUOTA_EXCEEDED, "monthly usage quota reached", false)
				return
			}

			// Reserve succeeded — set quota headers before the inner handler writes the
			// response. Headers must be set before the first WriteHeader/Write call.
			// Use resetAt from Reserve so the header matches the actual Redis EXPIREAT.
			httputil.SetQuotaHeaders(w, cap, remaining, resetAt)

			// Plant a record-signal so the recorder can tell us whether it actually wrote
			// a usage row downstream.
			ctx, signal := withRecordSignal(r.Context())
			next.ServeHTTP(w, r.WithContext(ctx))

			if !signal.recorded.Load() {
				// No usage row was written for this request — refund the reserve against
				// the EXACT key Reserve used (handles midnight-UTC boundaries correctly).
				// Use a fresh background context: the request context may already be canceled
				// by client disconnect, but the refund still needs to run.
				backgroundRefund(t, key.Customer.ID.String(), reservedKey)
			}
		})
	}
}

func backgroundRefund(t *Tracker, customerID string, reservedKey string) {
	bg, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := t.RefundAt(bg, reservedKey); err != nil {
		log.Warn().
			Err(err).
			Str("customer", customerID).
			Str("key", reservedKey).
			Msg("quota refund failed; counter may drift +1")
	}
}
