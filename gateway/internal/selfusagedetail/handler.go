package selfusagedetail

import (
	"encoding/csv"
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

// isoDateRE mirrors selferrors' identically-named regex so every self-service
// date-filtered surface accepts and rejects the same date inputs.
//
// Unlike selferrors' operation filter (which matches error_events.operation —
// a chi route pattern like "/v1/echo"), usage_events.operation stores the
// opaque RouteDescriptor.Operation string passed to usage.Recorder.Record
// (e.g. "echo", see server.invoke and routes_table.go) — it is never
// path-shaped, so it is validated the same way the dashboard's usage export
// validates it (dashboard/lib/db.ts's validateUsageQueryParams): trimmed,
// non-empty, bounded by length only.
var isoDateRE = regexp.MustCompile(`^\d{4}-(0[1-9]|1[0-2])-(0[1-9]|[12]\d|3[01])$`)

// csvHeader is the fixed column order for the CSV export, per the module spec.
var csvHeader = []string{"id", "operation", "billable_units", "created_at"}

// Response is the JSON envelope for GET /v1/usage/events.
type Response struct {
	Data    []Event `json:"data"`
	HasMore bool    `json:"has_more"`
	Page    int     `json:"page"`
	Limit   int     `json:"limit"`
}

// Handler returns GET /v1/usage/events: the authenticated customer's own
// usage_events rows, newest-first, filtered by an optional date range and/or
// operation, and paginated. Customer scope comes strictly from
// auth.FromContext — there is no path/query/body customer identifier, so this
// handler can only ever return the caller's own usage history.
//
// Responds with the paginated JSON envelope by default, or an RFC-4180 CSV
// export when the request asks for it via ?format=csv or Accept: text/csv.
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
		// identical to selferrors' and the dashboard's /api/usage defaults.
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
			// Length-only bound: usage_events.operation is an opaque worker
			// operation string (e.g. "echo"), not a /v1/... path — see the
			// isoDateRE doc comment above for why this differs from selferrors.
			if len([]rune(v)) > maxFilterLength {
				apierror.Write(w, rid, http.StatusBadRequest, apierror.BAD_REQUEST,
					fmt.Sprintf("invalid 'operation' filter: max %d characters", maxFilterLength), false)
				return
			}
			operation = &v
		}

		pp := paging.ParseQuery(q, "limit")
		page, limit := paging.Clamp(pp.Page, pp.PerPage, defaultPageSize, maxPageSize)
		offset, err := paging.Offset(page, limit)
		if err != nil {
			apierror.Write(w, rid, http.StatusBadRequest, apierror.BAD_REQUEST, "page too large", false)
			return
		}

		events, hasMore, err := store.Query(r.Context(), key.Customer.ID, from, toExclusive, operation, limit, offset)
		if err != nil {
			log.Error().Err(err).Str("request_id", rid).Msg("selfusagedetail: query failed")
			apierror.Write(w, rid, http.StatusInternalServerError, apierror.INTERNAL, "query failed", false)
			return
		}

		w.Header().Set("Cache-Control", "no-store")
		if wantsCSV(r) {
			writeCSV(w, events)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(Response{
			Data:    events,
			HasMore: hasMore,
			Page:    page,
			Limit:   limit,
		})
	}
}

// wantsCSV reports whether the caller asked for the CSV representation, via
// ?format=csv or an Accept: text/csv header (checked as the caller's sole or
// first preference, ignoring quality-value suffixes).
func wantsCSV(r *http.Request) bool {
	if strings.EqualFold(r.URL.Query().Get("format"), "csv") {
		return true
	}
	for _, part := range strings.Split(r.Header.Get("Accept"), ",") {
		mediaType := strings.TrimSpace(strings.SplitN(part, ";", 2)[0])
		if strings.EqualFold(mediaType, "text/csv") {
			return true
		}
	}
	return false
}

// writeCSV renders events as RFC-4180 CSV with a fixed header row
// (id,operation,billable_units,created_at). encoding/csv handles quoting of
// fields containing commas, quotes, or newlines.
func writeCSV(w http.ResponseWriter, events []Event) {
	w.Header().Set("Content-Type", "text/csv")
	cw := csv.NewWriter(w)
	_ = cw.Write(csvHeader)
	for _, e := range events {
		_ = cw.Write([]string{
			e.ID,
			e.Operation,
			e.BillableUnits,
			e.CreatedAt.Format(time.RFC3339),
		})
	}
	cw.Flush()
}

// parseISODate parses an ISO 8601 date (YYYY-MM-DD) as UTC midnight. Rejects
// calendar overflow (e.g. "2023-02-30") that time.Parse would otherwise
// silently normalize (Feb 30 -> Mar 2) via a round-trip format check. Mirrors
// selferrors.parseISODate.
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
