package operator

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/rs/zerolog/log"

	"github.com/Unluckyathecking/crucible/gateway/internal/apierror"
	mwpkg "github.com/Unluckyathecking/crucible/gateway/internal/middleware"
	"github.com/Unluckyathecking/crucible/gateway/internal/paging"
)

// ListCustomersHandler handles GET /v1/admin/customers.
// Query params: plan_id (optional), page (default 1), per_page (default 20).
func ListCustomersHandler(s *Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rid, _ := r.Context().Value(mwpkg.RequestIDKey).(string)
		q := r.URL.Query()
		page, _ := strconv.Atoi(q.Get("page"))
		perPage, _ := strconv.Atoi(q.Get("per_page"))

		result, err := s.Customers(r.Context(), CustomersFilter{
			PlanID:  q.Get("plan_id"),
			Page:    page,
			PerPage: perPage,
		})
		if err != nil {
			if errors.Is(err, paging.ErrPageTooLarge) {
				apierror.Write(w, rid, http.StatusBadRequest, apierror.BAD_REQUEST, "page too large", false)
				return
			}
			log.Error().Err(err).Str("request_id", rid).Msg("operator: list customers failed")
			apierror.Write(w, rid, http.StatusInternalServerError, apierror.INTERNAL, "query failed", false)
			return
		}

		writeJSON(w, result)
	}
}

// GetCustomerHandler handles GET /v1/admin/customers/{id}.
func GetCustomerHandler(s *Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rid, _ := r.Context().Value(mwpkg.RequestIDKey).(string)

		id, err := uuid.Parse(chi.URLParam(r, "id"))
		if err != nil {
			apierror.Write(w, rid, http.StatusBadRequest, apierror.BAD_REQUEST, "invalid customer id", false)
			return
		}

		c, err := s.CustomerByID(r.Context(), id)
		if err != nil {
			if err == pgx.ErrNoRows {
				apierror.Write(w, rid, http.StatusNotFound, "NOT_FOUND", "customer not found", false)
				return
			}
			log.Error().Err(err).Str("request_id", rid).Msg("operator: get customer failed")
			apierror.Write(w, rid, http.StatusInternalServerError, apierror.INTERNAL, "query failed", false)
			return
		}

		writeJSON(w, c)
	}
}

// GetCustomerUsageHandler handles GET /v1/admin/customers/{id}/usage.
// Query params: start, end (RFC3339; optional, default = current UTC calendar month).
func GetCustomerUsageHandler(s *Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rid, _ := r.Context().Value(mwpkg.RequestIDKey).(string)

		id, err := uuid.Parse(chi.URLParam(r, "id"))
		if err != nil {
			apierror.Write(w, rid, http.StatusBadRequest, apierror.BAD_REQUEST, "invalid customer id", false)
			return
		}

		q := r.URL.Query()
		var start, end time.Time
		if s := q.Get("start"); s != "" {
			if start, err = time.Parse(time.RFC3339, s); err != nil {
				apierror.Write(w, rid, http.StatusBadRequest, apierror.BAD_REQUEST, "invalid start: use RFC3339", false)
				return
			}
		}
		if e := q.Get("end"); e != "" {
			if end, err = time.Parse(time.RFC3339, e); err != nil {
				apierror.Write(w, rid, http.StatusBadRequest, apierror.BAD_REQUEST, "invalid end: use RFC3339", false)
				return
			}
		}
		if start.IsZero() != end.IsZero() {
			apierror.Write(w, rid, http.StatusBadRequest, apierror.BAD_REQUEST, "start and end must be provided together", false)
			return
		}
		if !start.IsZero() && !end.IsZero() && !end.After(start) {
			apierror.Write(w, rid, http.StatusBadRequest, apierror.BAD_REQUEST, "end must be after start", false)
			return
		}

		if _, err := s.CustomerByID(r.Context(), id); err != nil {
			if err == pgx.ErrNoRows {
				apierror.Write(w, rid, http.StatusNotFound, "NOT_FOUND", "customer not found", false)
				return
			}
			log.Error().Err(err).Str("request_id", rid).Msg("operator: customer lookup failed")
			apierror.Write(w, rid, http.StatusInternalServerError, apierror.INTERNAL, "query failed", false)
			return
		}

		result, err := s.CustomerUsage(r.Context(), id, start, end)
		if err != nil {
			log.Error().Err(err).Str("request_id", rid).Msg("operator: customer usage failed")
			apierror.Write(w, rid, http.StatusInternalServerError, apierror.INTERNAL, "query failed", false)
			return
		}

		writeJSON(w, result)
	}
}

// ListAuditEventsHandler handles GET /v1/admin/audit.
// Query params: customer_id, action, start, end (RFC3339), page, per_page.
func ListAuditEventsHandler(s *Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rid, _ := r.Context().Value(mwpkg.RequestIDKey).(string)
		q := r.URL.Query()

		var f AuditFilter
		f.CustomerID = q.Get("customer_id")
		f.Action = q.Get("action")
		f.Page, _ = strconv.Atoi(q.Get("page"))
		f.PerPage, _ = strconv.Atoi(q.Get("per_page"))

		if s := q.Get("start"); s != "" {
			t, err := time.Parse(time.RFC3339, s)
			if err != nil {
				apierror.Write(w, rid, http.StatusBadRequest, apierror.BAD_REQUEST, "invalid start: use RFC3339", false)
				return
			}
			f.Start = &t
		}
		if e := q.Get("end"); e != "" {
			t, err := time.Parse(time.RFC3339, e)
			if err != nil {
				apierror.Write(w, rid, http.StatusBadRequest, apierror.BAD_REQUEST, "invalid end: use RFC3339", false)
				return
			}
			f.End = &t
		}
		if f.Start != nil && f.End != nil && !f.End.After(*f.Start) {
			apierror.Write(w, rid, http.StatusBadRequest, apierror.BAD_REQUEST, "end must be after start", false)
			return
		}

		result, err := s.AuditEvents(r.Context(), f)
		if err != nil {
			if errors.Is(err, paging.ErrPageTooLarge) {
				apierror.Write(w, rid, http.StatusBadRequest, apierror.BAD_REQUEST, "page too large", false)
				return
			}
			log.Error().Err(err).Str("request_id", rid).Msg("operator: list audit events failed")
			apierror.Write(w, rid, http.StatusInternalServerError, apierror.INTERNAL, "query failed", false)
			return
		}

		writeJSON(w, result)
	}
}

// ListPlansHandler handles GET /v1/admin/plans.
func ListPlansHandler(s *Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rid, _ := r.Context().Value(mwpkg.RequestIDKey).(string)

		plans, err := s.Plans(r.Context())
		if err != nil {
			log.Error().Err(err).Str("request_id", rid).Msg("operator: list plans failed")
			apierror.Write(w, rid, http.StatusInternalServerError, apierror.INTERNAL, "query failed", false)
			return
		}

		writeJSON(w, plans)
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(v)
}
