package selfusagedetail

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
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
			// Length-only bound: usage_events.operation is an opaque worker
			// operation string (e.g. "echo", see server.invoke and
			// routes_table.go), not a /v1/... path like selferrors'
			// error_events.operation, so it is validated as the dashboard's
			// validateUsageQueryParams (dashboard/lib/db.ts) does.
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

// wantsCSV reports whether the caller asked for the CSV representation: an
// explicit ?format=csv always wins outright, otherwise the Accept header is
// negotiated per RFC 7231 §5.3.2 — CSV is only chosen when it strictly
// outranks application/json's q-value, so "text/csv;q=0" (explicitly
// rejected) or "application/json, text/csv" (equal, unspecified preference —
// list order carries no meaning under the RFC) both fall back to JSON.
func wantsCSV(r *http.Request) bool {
	if strings.EqualFold(r.URL.Query().Get("format"), "csv") {
		return true
	}
	accept := r.Header.Get("Accept")
	if accept == "" {
		return false
	}
	csvQ := acceptQuality(accept, "text/csv")
	jsonQ := acceptQuality(accept, "application/json")
	return csvQ > 0 && csvQ > jsonQ
}

// acceptQuality returns the q-value (default 1.0) an RFC 7231 Accept header
// assigns to mediaType, matching the exact type, its type wildcard
// ("text/*"), or "*/*" — an exact match outranks a type wildcard, which
// outranks "*/*". Returns 0 if nothing in the header matches, meaning
// mediaType is unacceptable to (or simply unmentioned by) this caller.
func acceptQuality(accept, mediaType string) float64 {
	typ, _, _ := strings.Cut(mediaType, "/")
	bestQ := 0.0
	bestRank := -1
	for _, entry := range strings.Split(accept, ",") {
		fields := strings.Split(entry, ";")
		candidate := strings.TrimSpace(fields[0])

		rank := -1
		switch {
		case strings.EqualFold(candidate, mediaType):
			rank = 2
		case strings.EqualFold(candidate, typ+"/*"):
			rank = 1
		case candidate == "*/*":
			rank = 0
		default:
			continue
		}
		if rank <= bestRank {
			continue
		}

		q := 1.0
		for _, param := range fields[1:] {
			if name, value, ok := strings.Cut(strings.TrimSpace(param), "="); ok && strings.EqualFold(name, "q") {
				if parsed, err := strconv.ParseFloat(value, 64); err == nil {
					q = parsed
				}
			}
		}
		bestRank, bestQ = rank, q
	}
	return bestQ
}

// writeCSV renders events as RFC-4180 CSV with a fixed header row
// (id,operation,billable_units,created_at). encoding/csv handles quoting of
// fields containing commas, quotes, or newlines. created_at uses
// RFC3339Nano (not RFC3339) to preserve the same sub-second precision the
// JSON path already carries via time.Time's default marshaling — RFC3339
// alone would silently truncate rows that share a whole second, which
// matters for a CSV meant for invoice reconciliation.
func writeCSV(w http.ResponseWriter, events []Event) {
	w.Header().Set("Content-Type", "text/csv")
	cw := csv.NewWriter(w)
	_ = cw.Write(csvHeader)
	for _, e := range events {
		_ = cw.Write([]string{
			e.ID,
			e.Operation,
			e.BillableUnits,
			e.CreatedAt.Format(time.RFC3339Nano),
		})
	}
	cw.Flush()
}
