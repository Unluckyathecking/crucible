// Crucible Enterprise Edition (EE) file.
// Licensed under the Crucible Enterprise License — see ee/LICENSE.md.
// Not covered by the repository's MIT license.

package selfaudit

import (
	"encoding/json"
	"net/http"
	"regexp"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/zerolog/log"

	"github.com/Unluckyathecking/crucible/gateway/internal/apierror"
	"github.com/Unluckyathecking/crucible/gateway/internal/auth"
	"github.com/Unluckyathecking/crucible/gateway/internal/license"
	mw "github.com/Unluckyathecking/crucible/gateway/internal/middleware"
	"github.com/Unluckyathecking/crucible/gateway/internal/paging"
)

const (
	defaultPageSize = 50
	maxPageSize     = 200
	maxActionLen    = 128
)

// actionFilterRE bounds the action filter to the audit action naming convention
// (noun.verb with dotted/underscore/hyphen segments, e.g. "api_key.created").
var actionFilterRE = regexp.MustCompile(`^[a-zA-Z0-9_.-]{1,128}$`)

// Response is the JSON envelope for GET /v1/audit, mirroring GET /v1/errors.
type Response struct {
	Data    []Event `json:"data"`
	HasMore bool    `json:"has_more"`
	Page    int     `json:"page"`
	Limit   int     `json:"limit"`
}

// Handler returns GET /v1/audit: the authenticated customer's own audit_log
// rows, newest-first, optionally filtered by action, and paginated. This is an
// Enterprise Edition feature — a deployment without a license granting
// FeatureAuditExport gets 403 FEATURE_NOT_LICENSED. Customer scope comes strictly
// from auth.FromContext; there is no customer identifier in the path/query/body,
// so this handler can only ever return the caller's own audit history.
func Handler(db *pgxpool.Pool, lic *license.License) http.HandlerFunc {
	store := NewStore(db)
	return func(w http.ResponseWriter, r *http.Request) {
		rid, _ := r.Context().Value(mw.RequestIDKey).(string)

		if !lic.Has(license.FeatureAuditExport) {
			apierror.Write(w, rid, http.StatusForbidden, apierror.FEATURE_NOT_LICENSED,
				"audit export requires a Crucible Enterprise license", false)
			return
		}

		key := auth.FromContext(r.Context())
		if key == nil {
			apierror.Write(w, rid, http.StatusUnauthorized, apierror.UNAUTHORIZED, "no auth context", false)
			return
		}

		q := r.URL.Query()

		var action *string
		if v := strings.TrimSpace(q.Get("action")); v != "" {
			if len(v) > maxActionLen || !actionFilterRE.MatchString(v) {
				apierror.Write(w, rid, http.StatusBadRequest, apierror.BAD_REQUEST,
					"invalid 'action' filter: letters, digits, '.', '_', '-' only (max 128 chars)", false)
				return
			}
			action = &v
		}

		pp := paging.ParseQuery(q, "limit")
		page, limit := paging.Clamp(pp.Page, pp.PerPage, defaultPageSize, maxPageSize)
		offset, err := paging.Offset(page, limit)
		if err != nil {
			apierror.Write(w, rid, http.StatusBadRequest, apierror.BAD_REQUEST, "page too large", false)
			return
		}

		events, hasMore, err := store.Query(r.Context(), key.Customer.ID, action, limit, offset)
		if err != nil {
			log.Error().Err(err).Str("request_id", rid).Msg("selfaudit: query failed")
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
