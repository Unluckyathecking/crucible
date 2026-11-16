package selferrors

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/rs/zerolog/log"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Unluckyathecking/crucible/gateway/internal/apierror"
	"github.com/Unluckyathecking/crucible/gateway/internal/auth"
	mw "github.com/Unluckyathecking/crucible/gateway/internal/middleware"
	"github.com/Unluckyathecking/crucible/gateway/internal/paging"
)

const (
	defaultPageSize = 50
	maxPageSize     = 200
	maxFilterLength = 128
)

// operationFilterRE and codeFilterRE mirror the dashboard's
// OPERATION_FILTER_RE/CODE_FILTER_RE (dashboard/app/api/errors/route.ts) so
// the two self-service surfaces accept and reject the same inputs.
var (
	operationFilterRE = regexp.MustCompile(`^/(?:[a-zA-Z0-9_-]+/)*[a-zA-Z0-9_-]+$`)
	codeFilterRE      = regexp.MustCompile(`^[A-Z0-9_]{1,128}$`)
)

// Response is the JSON envelope for GET /v1/errors.
type Response struct {
	Data    []Event `json:"data"`
	HasMore bool    `json:"has_more"`
	Page    int     `json:"page"`
	Limit   int     `json:"limit"`
}

// Handler returns GET /v1/errors: the authenticated customer's own
// error_events rows, newest-first, filtered by an optional date range,
// operation, and/or error code, and paginated. Customer scope comes strictly
// from auth.FromContext — there is no path/query/body customer identifier, so
// this handler can only ever return the caller's own error history.
func Handler(db *pgxpool.Pool) http.HandlerFunc {
	store := NewStore(db)
	return func(w http.ResponseWriter, r *http.Request) {
		rid := mw.RequestIDFrom(r.Context())
		key := auth.FromContext(r.Context())
		if key == nil {
			apierror.Write(w, rid, http.StatusUnauthorized, apierror.UNAUTHORIZED, "no auth context", false)
			return
		}

		q := r.URL.Query()

		from, toExclusive, dateErr := paging.ParseDateRange(q, time.Now())
		if dateErr != "" {
			apierror.Write(w, rid, http.StatusBadRequest, apierror.BAD_REQUEST, dateErr, false)
			return
		}

		var operation *string
		if v := strings.TrimSpace(q.Get("operation")); v != "" {
			if len(v) > maxFilterLength || !operationFilterRE.MatchString(v) {
				apierror.Write(w, rid, http.StatusBadRequest, apierror.BAD_REQUEST,
					fmt.Sprintf("invalid 'operation' filter: must be a /v1/... path (max %d chars)", maxFilterLength), false)
				return
			}
			operation = &v
		}

		var code *string
		if v := strings.TrimSpace(q.Get("code")); v != "" {
			if len(v) > maxFilterLength || !codeFilterRE.MatchString(v) {
				apierror.Write(w, rid, http.StatusBadRequest, apierror.BAD_REQUEST,
					fmt.Sprintf("invalid 'code' filter: must be uppercase letters, digits, and underscores (max %d chars)", maxFilterLength), false)
				return
			}
			code = &v
		}

		pp := paging.ParseQuery(q, "limit")
		page, limit := paging.Clamp(pp.Page, pp.PerPage, defaultPageSize, maxPageSize)
		offset, err := paging.Offset(page, limit)
		if err != nil {
			apierror.Write(w, rid, http.StatusBadRequest, apierror.BAD_REQUEST, "page too large", false)
			return
		}

		events, hasMore, err := store.Query(r.Context(), key.Customer.ID, from, toExclusive, operation, code, limit, offset)
		if err != nil {
			log.Error().Err(err).Str("request_id", rid).Msg("selferrors: query failed")
			apierror.Write(w, rid, http.StatusInternalServerError, apierror.INTERNAL, "query failed", false)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(w).Encode(Response{
			Data:    events,
			HasMore: hasMore,
			Page:    page,
			Limit:   limit,
		})
	}
}
