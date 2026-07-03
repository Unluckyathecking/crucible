package selfusage

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/rs/zerolog/log"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Unluckyathecking/crucible/gateway/internal/apierror"
	"github.com/Unluckyathecking/crucible/gateway/internal/auth"
	"github.com/Unluckyathecking/crucible/gateway/internal/billing"
	mw "github.com/Unluckyathecking/crucible/gateway/internal/middleware"
	"github.com/Unluckyathecking/crucible/gateway/internal/quota"
)

// Response is the JSON envelope for GET /v1/usage.
type Response struct {
	PlanID string `json:"plan_id"`
	// Used and Remaining are derived from quota.Tracker.Current — the exact
	// counter the quota middleware reserves against — so what the customer sees
	// here always matches what throttles them.
	Used uint64 `json:"used"`
	// Cap is billing.PlanCache.MonthlyCap for the customer's plan. 0 = unlimited.
	Cap int64 `json:"cap"`
	// Remaining is Cap-Used floored at 0. -1 = unlimited (Cap == 0).
	Remaining   int64            `json:"remaining"`
	PeriodStart time.Time        `json:"period_start"`
	PeriodEnd   time.Time        `json:"period_end"`
	TotalUnits  int64            `json:"total_units"`
	TotalCalls  int64            `json:"total_calls"`
	Breakdown   []OperationUsage `json:"breakdown"`
}

// Handler returns GET /v1/usage: the authenticated customer's current
// billing-period consumption against their plan's quota cap, plus a
// per-operation breakdown. Customer scope comes strictly from
// auth.FromContext — there is no path/query/body customer identifier, so this
// handler can only ever return the caller's own usage.
//
// db, tracker, and plans are each independently nil-safe: an unset dependency
// degrades its slice of the response to a zeroed value (empty breakdown, 0
// used, 0/unlimited cap) instead of panicking, mirroring
// idempotency.NewStore's nil-DB pass-through.
func Handler(db *pgxpool.Pool, tracker *quota.Tracker, plans *billing.PlanCache) http.HandlerFunc {
	store := NewStore(db)
	return func(w http.ResponseWriter, r *http.Request) {
		rid, _ := r.Context().Value(mw.RequestIDKey).(string)
		key := auth.FromContext(r.Context())
		if key == nil {
			apierror.Write(w, rid, http.StatusUnauthorized, apierror.UNAUTHORIZED, "no auth context", false)
			return
		}

		var capUnits int64
		if plans != nil {
			capUnits = plans.MonthlyCap(r.Context(), key.Customer.Plan)
		}

		var used uint64
		if tracker != nil {
			u, err := tracker.Current(r.Context(), key.Customer.ID)
			if err != nil {
				log.Error().Err(err).Str("request_id", rid).Msg("selfusage: quota tracker read failed")
				apierror.Write(w, rid, http.StatusInternalServerError, apierror.INTERNAL, "usage lookup failed", true)
				return
			}
			used = u
		}

		remaining := int64(-1)
		if capUnits > 0 {
			remaining = capUnits - int64(used)
			if remaining < 0 {
				remaining = 0
			}
		}

		start, end := CurrentBillingPeriod()
		breakdown, totalUnits, totalCalls, err := store.Breakdown(r.Context(), key.Customer.ID, start, end)
		if err != nil {
			log.Error().Err(err).Str("request_id", rid).Msg("selfusage: breakdown query failed")
			apierror.Write(w, rid, http.StatusInternalServerError, apierror.INTERNAL, "usage lookup failed", true)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(w).Encode(Response{
			PlanID:      key.Customer.Plan,
			Used:        used,
			Cap:         capUnits,
			Remaining:   remaining,
			PeriodStart: start,
			PeriodEnd:   end,
			TotalUnits:  totalUnits,
			TotalCalls:  totalCalls,
			Breakdown:   breakdown,
		})
	}
}
