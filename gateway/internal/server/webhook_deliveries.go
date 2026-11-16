package server

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/zerolog/log"

	"github.com/Unluckyathecking/crucible/gateway/internal/apierror"
	"github.com/Unluckyathecking/crucible/gateway/internal/auth"
	mw "github.com/Unluckyathecking/crucible/gateway/internal/middleware"
	"github.com/Unluckyathecking/crucible/gateway/internal/paging"
	"github.com/Unluckyathecking/crucible/gateway/internal/webhookout"
)

// authCustomerID adapts auth.FromContext to webhookout.CustomerIDFunc. Defined
// here (rather than in webhookout) because webhookout cannot import auth — see
// webhookout.CustomerIDFunc's doc comment for why.
func authCustomerID(r *http.Request) (uuid.UUID, bool) {
	key := auth.FromContext(r.Context())
	if key == nil {
		return uuid.Nil, false
	}
	return key.Customer.ID, true
}

// webhookDeliveriesHandler returns a paginated list of the authenticated
// customer's outbound webhook deliveries across all their registered endpoints.
// GET /v1/webhooks/deliveries?page=1&per_page=20
func webhookDeliveriesHandler(db *pgxpool.Pool) http.HandlerFunc {
	const (
		// default matches the previous hardcoded LIMIT 100 to avoid silently
		// truncating callers that omit per_page; explicit per_page can go lower.
		defaultPageSize = 100
		maxPageSize     = 100
	)
	return func(w http.ResponseWriter, r *http.Request) {
		rid := mw.RequestIDFrom(r.Context())
		key := auth.FromContext(r.Context())
		if key == nil {
			apierror.Write(w, rid, http.StatusUnauthorized, apierror.UNAUTHORIZED, "no auth context", false)
			return
		}

		pp := paging.ParseQuery(r.URL.Query(), "per_page")
		page, perPage := paging.Clamp(pp.Page, pp.PerPage, defaultPageSize, maxPageSize)
		result, err := webhookout.ListDeliveries(r.Context(), db, key.Customer.ID, page, perPage)
		if err != nil {
			if errors.Is(err, paging.ErrPageTooLarge) {
				apierror.Write(w, rid, http.StatusBadRequest, apierror.BAD_REQUEST, "page too large", false)
				return
			}
			log.Error().Err(err).Str("request_id", rid).Msg("webhook deliveries query failed")
			apierror.Write(w, rid, http.StatusInternalServerError, apierror.INTERNAL, "query failed", false)
			return
		}

		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(result)
	}
}
