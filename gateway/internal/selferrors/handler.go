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
	defaultPageSize  = 50
	maxPageSize      = 200
	defaultRangeDays = 30
	maxRangeDays     = 90
	maxFilterLength  = 128
)

// operationFilterRE and codeFilterRE mirror the dashboard's
// OPERATION_FILTER_RE/CODE_FILTER_RE (dashboard/app/api/errors/route.ts) so
// the two self-service surfaces accept and reject the same inputs.
var (
	operationFilterRE = regexp.MustCompile(`^/(?:[a-zA-Z0-9_-]+/)*[a-zA-Z0-9_-]+$`)
	codeFilterRE      = regexp.MustCompile(`^[A-Z0-9_]{1,128}$`)
	isoDateRE         = regexp.MustCompile(`^\d{4}-(0[1-9]|1[0-2])-(0[1-9]|[12]\d|3[01])$`)
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
		rid, _ := r.Context().Value(mw.RequestIDKey).(string)
		key := auth.FromContext(r.Context())
		if key == nil {
			apierror.Write(w, rid, http.StatusUnauthorized, apierror.UNAUTHORIZED, "no auth context", false)
			return
		}

		q := r.URL.Query()

		// Date-range defaults: [tomorrowMidnight - 30 days, tomorrowMidnight),
		// identical to the dashboard's /api/errors and /api/usage defaults.
		now := time.Now().UTC()
		tomorrowMidnight := time.Date(now.Year(), now.Month(), now.Day()+1, 0, 0, 0, 0, time.UTC)
		from := tomorrowMidnight.AddDate(0, 0, -defaultRangeDays)
		toExclusive := tomorrowMidnight

		if v := q.Get("from"); v != "" {
			parsed, ok := parseISODate(v)
			if !ok {
				apierror.Write(w, rid, http.StatusBadRequest, apierror.BAD_REQUEST, "invalid 'from' date, expected ISO 8601", false)
				return
			}
			from = parsed
		}
		if v := q.Get("to"); v != "" {
			parsed, ok := parseISODate(v)
			if !ok {
				apierror.Write(w, rid, http.StatusBadRequest, apierror.BAD_REQUEST, "invalid 'to' date, expected ISO 8601", false)
				return
			}
			// `to` is inclusive; advance to the next midnight for the exclusive DB bound.
			toExclusive = parsed.AddDate(0, 0, 1)
		}
		if toExclusive.After(tomorrowMidnight) {
			apierror.Write(w, rid, http.StatusBadRequest, apierror.BAD_REQUEST, "'to' date must not be in the future", false)
			return
		}
		if from.After(tomorrowMidnight) {
			apierror.Write(w, rid, http.StatusBadRequest, apierror.BAD_REQUEST, "'from' date must not be in the future", false)
			return
		}
		userVisibleTo := toExclusive.AddDate(0, 0, -1)
		if from.After(userVisibleTo) {
			apierror.Write(w, rid, http.StatusBadRequest, apierror.BAD_REQUEST, "'from' must not be after 'to'", false)
			return
		}
		rangeDays := int(userVisibleTo.Sub(from).Hours() / 24)
		if rangeDays < 0 || rangeDays > maxRangeDays {
			apierror.Write(w, rid, http.StatusBadRequest, apierror.BAD_REQUEST,
				fmt.Sprintf("date range exceeds maximum of %d days", maxRangeDays), false)
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

// parseISODate parses an ISO 8601 date (YYYY-MM-DD) as UTC midnight. Rejects
// calendar overflow (e.g. "2023-02-30") that time.Parse would otherwise
// silently normalize (Feb 30 -> Mar 2) via a round-trip format check.
func parseISODate(s string) (time.Time, bool) {
	if !isoDateRE.MatchString(s) {
		return time.Time{}, false
	}
	t, err := time.Parse("2006-01-02", s)
	if err != nil {
		return time.Time{}, false
	}
	if t.Format("2006-01-02") != s {
		return time.Time{}, false
	}
	return t, true
}
