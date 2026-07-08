// Crucible Enterprise Edition (EE) file.
// Licensed under the Crucible Enterprise License — see ee/LICENSE.md.
// Not covered by the repository's MIT license.

package operator

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/zerolog/log"

	"github.com/Unluckyathecking/crucible/gateway/internal/apierror"
	"github.com/Unluckyathecking/crucible/gateway/internal/audit"
	"github.com/Unluckyathecking/crucible/gateway/internal/license"
	mwpkg "github.com/Unluckyathecking/crucible/gateway/internal/middleware"
	"github.com/Unluckyathecking/crucible/gateway/internal/paging"
)

const (
	maxTokenNameLen = 128

	// actionTokenCreated / actionTokenRevoked follow the noun.verb audit action
	// convention (api_key.created, webhook.delivery.replayed, …).
	actionTokenCreated = "operator_token.created"
	actionTokenRevoked = "operator_token.revoked"

	// tokenTargetType labels the audit target for operator-token rows.
	tokenTargetType = "operator_token"

	// tokenActorID identifies the operator-token bearer in audit_log. The
	// audit_log CHECK constraint permits only customer|admin|system, so operator
	// actions are recorded as actor_type=admin with this stable actor_id — the
	// same idiom webhookout's replay path uses.
	tokenActorID = "operator"
)

// featureLicensed writes a 403 FEATURE_NOT_LICENSED and returns false when the
// deployment license does not grant FeatureOperatorTokens.
func featureLicensed(w http.ResponseWriter, rid string, lic *license.License) bool {
	if lic.Has(license.FeatureOperatorTokens) {
		return true
	}
	apierror.Write(w, rid, http.StatusForbidden, apierror.FEATURE_NOT_LICENSED,
		"operator tokens require a Crucible Enterprise license", false)
	return false
}

// CreateTokenHandler handles POST /v1/admin/tokens.
// Body: {"name":"..."}. Returns the row metadata plus the full token string,
// which is shown exactly once and never recoverable afterward.
func CreateTokenHandler(ts *TokenStore, db *pgxpool.Pool, salt string, lic *license.License) http.HandlerFunc {
	type request struct {
		Name string `json:"name"`
	}
	type response struct {
		Token
		Token_ string `json:"token"`
	}
	return func(w http.ResponseWriter, r *http.Request) {
		rid, _ := r.Context().Value(mwpkg.RequestIDKey).(string)
		if !featureLicensed(w, rid, lic) {
			return
		}

		var body request
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			apierror.Write(w, rid, http.StatusBadRequest, apierror.BAD_REQUEST, "invalid json body", false)
			return
		}
		name := strings.TrimSpace(body.Name)
		if name == "" || len(name) > maxTokenNameLen {
			apierror.Write(w, rid, http.StatusBadRequest, apierror.BAD_REQUEST,
				"name is required and must be at most 128 characters", false)
			return
		}

		tok, full, err := ts.Create(r.Context(), name, salt)
		if err != nil {
			log.Error().Err(err).Str("request_id", rid).Msg("operator: create token failed")
			apierror.Write(w, rid, http.StatusInternalServerError, apierror.INTERNAL, "create failed", false)
			return
		}

		targetID := tok.ID.String()
		if err := audit.Emit(r.Context(), db, audit.Event{
			ActorType:  audit.ActorAdmin,
			ActorID:    tokenActorID,
			Action:     actionTokenCreated,
			TargetType: strptr(tokenTargetType),
			TargetID:   &targetID,
			Details:    map[string]any{"name": tok.Name},
		}); err != nil {
			log.Warn().Err(err).Str("request_id", rid).Msg("operator: audit emit failed for token create")
		}

		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(response{Token: tok, Token_: full})
	}
}

// ListTokensHandler handles GET /v1/admin/tokens.
// Query params: page (default 1), per_page (default 20). Never returns hashes
// or token material.
func ListTokensHandler(ts *TokenStore, lic *license.License) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rid, _ := r.Context().Value(mwpkg.RequestIDKey).(string)
		if !featureLicensed(w, rid, lic) {
			return
		}

		pp := paging.ParseQuery(r.URL.Query(), "per_page")
		result, err := ts.List(r.Context(), TokensFilter{Page: pp.Page, PerPage: pp.PerPage})
		if err != nil {
			if errors.Is(err, paging.ErrPageTooLarge) {
				apierror.Write(w, rid, http.StatusBadRequest, apierror.BAD_REQUEST, "page too large", false)
				return
			}
			log.Error().Err(err).Str("request_id", rid).Msg("operator: list tokens failed")
			apierror.Write(w, rid, http.StatusInternalServerError, apierror.INTERNAL, "query failed", false)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(w).Encode(result)
	}
}

// RevokeTokenHandler handles DELETE /v1/admin/tokens/{id}. Revocation takes
// effect on the next request — there is no operator-token auth cache.
func RevokeTokenHandler(ts *TokenStore, db *pgxpool.Pool, lic *license.License) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rid, _ := r.Context().Value(mwpkg.RequestIDKey).(string)
		if !featureLicensed(w, rid, lic) {
			return
		}

		id, err := uuid.Parse(chi.URLParam(r, "id"))
		if err != nil {
			apierror.Write(w, rid, http.StatusBadRequest, apierror.BAD_REQUEST, "invalid token id", false)
			return
		}

		found, err := ts.Revoke(r.Context(), id)
		if err != nil {
			log.Error().Err(err).Str("request_id", rid).Msg("operator: revoke token failed")
			apierror.Write(w, rid, http.StatusInternalServerError, apierror.INTERNAL, "revoke failed", false)
			return
		}
		if !found {
			apierror.Write(w, rid, http.StatusNotFound, apierror.NOT_FOUND, "token not found", false)
			return
		}

		targetID := id.String()
		if err := audit.Emit(r.Context(), db, audit.Event{
			ActorType:  audit.ActorAdmin,
			ActorID:    tokenActorID,
			Action:     actionTokenRevoked,
			TargetType: strptr(tokenTargetType),
			TargetID:   &targetID,
		}); err != nil {
			log.Warn().Err(err).Str("request_id", rid).Msg("operator: audit emit failed for token revoke")
		}

		w.WriteHeader(http.StatusNoContent)
	}
}

func strptr(s string) *string { return &s }
