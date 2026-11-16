package server

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/zerolog/log"

	"github.com/Unluckyathecking/crucible/gateway/internal/apierror"
	"github.com/Unluckyathecking/crucible/gateway/internal/auth"
	mw "github.com/Unluckyathecking/crucible/gateway/internal/middleware"
	"github.com/Unluckyathecking/crucible/gateway/internal/paging"
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
	type item struct {
		ID               string    `json:"id"`
		EventID          string    `json:"event_id"`
		EndpointURL      string    `json:"endpoint_url"`
		Status           string    `json:"status"`
		Attempts         int       `json:"attempts"`
		LastResponseCode *int      `json:"last_response_code,omitempty"`
		CreatedAt        time.Time `json:"created_at"`
	}
	const (
		// default matches the previous hardcoded LIMIT 100 to avoid silently
		// truncating callers that omit per_page; explicit per_page can go lower.
		defaultPageSize = 100
		maxPageSize     = 100
	)
	return func(w http.ResponseWriter, r *http.Request) {
		rid, _ := r.Context().Value(mw.RequestIDKey).(string)
		key := auth.FromContext(r.Context())
		if key == nil {
			apierror.Write(w, rid, http.StatusUnauthorized, apierror.UNAUTHORIZED, "no auth context", false)
			return
		}

		pp := paging.ParseQuery(r.URL.Query(), "per_page")
		page, perPage := paging.Clamp(pp.Page, pp.PerPage, defaultPageSize, maxPageSize)
		offset, err := paging.Offset(page, perPage)
		if err != nil {
			apierror.Write(w, rid, http.StatusBadRequest, apierror.BAD_REQUEST, "page too large", false)
			return
		}

		var total int64
		if err := db.QueryRow(r.Context(), `
			SELECT COUNT(*)
			FROM webhook_deliveries d
			JOIN webhook_endpoints we ON we.id = d.endpoint_id
			WHERE we.customer_id = $1
		`, key.Customer.ID).Scan(&total); err != nil {
			log.Error().Err(err).Str("request_id", rid).Msg("webhook deliveries count failed")
			apierror.Write(w, rid, http.StatusInternalServerError, apierror.INTERNAL, "query failed", false)
			return
		}

		rows, err := db.Query(r.Context(), `
			SELECT d.id::text, d.event_id, we.url, d.status, d.attempts,
			       d.last_response_code, d.created_at
			FROM webhook_deliveries d
			JOIN webhook_endpoints we ON we.id = d.endpoint_id
			WHERE we.customer_id = $1
			ORDER BY d.created_at DESC, d.id DESC
			LIMIT $2 OFFSET $3
		`, key.Customer.ID, perPage, offset)
		if err != nil {
			log.Error().Err(err).Str("request_id", rid).Msg("webhook deliveries query failed")
			apierror.Write(w, rid, http.StatusInternalServerError, apierror.INTERNAL, "query failed", false)
			return
		}
		defer rows.Close()
		items := []item{}
		for rows.Next() {
			var it item
			if err := rows.Scan(&it.ID, &it.EventID, &it.EndpointURL, &it.Status,
				&it.Attempts, &it.LastResponseCode, &it.CreatedAt); err != nil {
				log.Error().Err(err).Str("request_id", rid).Msg("webhook deliveries scan failed")
				apierror.Write(w, rid, http.StatusInternalServerError, apierror.INTERNAL, "scan failed", false)
				return
			}
			items = append(items, it)
		}
		if err := rows.Err(); err != nil {
			log.Error().Err(err).Str("request_id", rid).Msg("webhook deliveries rows error")
			apierror.Write(w, rid, http.StatusInternalServerError, apierror.INTERNAL, "rows error", false)
			return
		}

		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(paging.Page[item]{Items: items, Total: total})
	}
}
