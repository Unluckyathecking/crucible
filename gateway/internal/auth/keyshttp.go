// Customer-facing HTTP handlers for programmatic API-key self-management:
// list/rotate/revoke for an API-key-authenticated (headless) customer. This is
// the API-key-path counterpart to the dashboard's key management UI
// (dashboard/app/api/keys), mirroring the shape of
// webhookout/endpoints_http.go. Unlike webhookout, these handlers live in
// package auth itself rather than a separate package: Store.List/Owner/
// Revoke/Rotate and FromContext are already package-local here, so no
// CustomerIDFunc adapter is needed to avoid an import cycle.
package auth

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/rs/zerolog/log"

	"github.com/Unluckyathecking/crucible/gateway/internal/apierror"
	mwpkg "github.com/Unluckyathecking/crucible/gateway/internal/middleware"
	"github.com/Unluckyathecking/crucible/gateway/internal/paging"
)

// Default/max page size for GET /v1/keys. Store.List pushes page/per_page
// into a SQL LIMIT/OFFSET rather than slicing an already-materialized result.
const (
	defaultKeysPageSize = 20
	maxKeysPageSize     = 100
)

// Rotate grace bounds mirror dashboard/app/api/keys/[id]/rotate/route.ts's
// MIN/MAX/DEFAULT_GRACE_SECS so both self-service surfaces (dashboard UI,
// API-key path) clamp identically. Store.Rotate independently re-clamps to
// maxGrace server-side; enforcing the same bounds here too means an
// out-of-range value is clamped at the edge rather than only deep in Store.
const (
	rotateGraceDefault = time.Hour
	rotateGraceMin     = 0
	rotateGraceMax     = 7 * 24 * time.Hour
)

// keyItemResponse is the JSON projection of a KeyListItem. Field names mirror
// dashboard/lib/db.ts's ApiKeyRow for client parity. The hash and full key are
// never present — see KeyListItem.
type keyItemResponse struct {
	ID         string     `json:"id"`
	Prefix     string     `json:"prefix"`
	Name       *string    `json:"name"`
	LastUsedAt *time.Time `json:"last_used_at"`
	ExpiresAt  *time.Time `json:"expires_at"`
	CreatedAt  time.Time  `json:"created_at"`
}

// ListKeysHandler handles GET /v1/keys: the authenticated customer's active
// API keys. Never serializes a hash or full-key field.
func ListKeysHandler(store *Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rid, _ := r.Context().Value(mwpkg.RequestIDKey).(string)
		key := FromContext(r.Context())
		if key == nil {
			apierror.Write(w, rid, http.StatusUnauthorized, apierror.UNAUTHORIZED, "no auth context", false)
			return
		}

		pp := paging.ParseQuery(r.URL.Query(), "per_page")
		page, perPage := paging.Clamp(pp.Page, pp.PerPage, defaultKeysPageSize, maxKeysPageSize)

		result, err := store.List(r.Context(), key.Customer.ID, page, perPage)
		if err != nil {
			if errors.Is(err, paging.ErrPageTooLarge) {
				apierror.Write(w, rid, http.StatusBadRequest, apierror.BAD_REQUEST, "page too large", false)
				return
			}
			log.Error().Err(err).Str("request_id", rid).Msg("auth: list keys failed")
			apierror.Write(w, rid, http.StatusInternalServerError, apierror.INTERNAL, "list keys failed", false)
			return
		}

		out := make([]keyItemResponse, len(result.Items))
		for i, it := range result.Items {
			out[i] = keyItemResponse{
				ID:         it.ID.String(),
				Prefix:     it.Prefix,
				Name:       it.Name,
				LastUsedAt: it.LastUsedAt,
				ExpiresAt:  it.ExpiresAt,
				CreatedAt:  it.CreatedAt,
			}
		}

		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(w).Encode(paging.Page[keyItemResponse]{Items: out, Total: result.Total})
	}
}

// ownedKeyID parses the {id} path param and verifies it names an active key
// owned by custID, writing the appropriate error response and returning
// ok=false when it doesn't. Not-owned and doesn't-exist collapse into the same
// 404 so the caller can never learn that an id belongs to someone else —
// mirrors webhookout.DeleteEndpoint / ErrEndpointNotFound.
func ownedKeyID(w http.ResponseWriter, r *http.Request, store *Store, rid string, custID uuid.UUID) (uuid.UUID, bool) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		apierror.Write(w, rid, http.StatusBadRequest, apierror.BAD_REQUEST, "invalid key id", false)
		return uuid.Nil, false
	}
	owner, err := store.Owner(r.Context(), id)
	if err != nil {
		if errors.Is(err, ErrKeyNotFound) {
			apierror.Write(w, rid, http.StatusNotFound, "NOT_FOUND", "api key not found", false)
			return uuid.Nil, false
		}
		log.Error().Err(err).Str("request_id", rid).Msg("auth: key ownership lookup failed")
		apierror.Write(w, rid, http.StatusInternalServerError, apierror.INTERNAL, "key lookup failed", false)
		return uuid.Nil, false
	}
	if owner != custID {
		apierror.Write(w, rid, http.StatusNotFound, "NOT_FOUND", "api key not found", false)
		return uuid.Nil, false
	}
	return id, true
}

// rotateRequest is the POST /v1/keys/{id}/rotate request body. GraceSecs is
// optional; omitted (nil, or an absent/empty body) falls back to
// rotateGraceDefault. An out-of-range value is clamped, not rejected.
type rotateRequest struct {
	GraceSecs *float64 `json:"grace_secs"`
}

// RotateKeysHandler handles POST /v1/keys/{id}/rotate: issues a replacement
// key for an id owned by the authenticated customer via the existing
// Store.Rotate (grace window, audit, and webhook emission all preserved). The
// new full key is present in the response exactly once.
func RotateKeysHandler(store *Store, keyPrefix string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rid, _ := r.Context().Value(mwpkg.RequestIDKey).(string)
		key := FromContext(r.Context())
		if key == nil {
			apierror.Write(w, rid, http.StatusUnauthorized, apierror.UNAUTHORIZED, "no auth context", false)
			return
		}

		id, ok := ownedKeyID(w, r, store, rid, key.Customer.ID)
		if !ok {
			return
		}

		var body rotateRequest
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil && !errors.Is(err, io.EOF) {
			apierror.Write(w, rid, http.StatusBadRequest, apierror.BAD_REQUEST, "invalid json body", false)
			return
		}
		grace := rotateGraceDefault
		if body.GraceSecs != nil {
			grace = time.Duration(*body.GraceSecs) * time.Second
			if grace < rotateGraceMin {
				grace = rotateGraceMin
			}
			if grace > rotateGraceMax {
				grace = rotateGraceMax
			}
		}

		newFullKey, _, err := store.Rotate(r.Context(), id, keyPrefix, grace)
		if err != nil {
			if errors.Is(err, ErrKeyRotating) {
				apierror.Write(w, rid, http.StatusConflict, apierror.KEY_ALREADY_ROTATED, "key already rotated; in grace period", false)
				return
			}
			if errors.Is(err, ErrKeyNotFound) {
				// The key was revoked or deleted between the ownership check and this call.
				// Same IDOR-safe 404.
				apierror.Write(w, rid, http.StatusNotFound, "NOT_FOUND", "api key not found", false)
				return
			}
			log.Error().Err(err).Str("request_id", rid).Msg("auth: rotate key failed")
			apierror.Write(w, rid, http.StatusInternalServerError, apierror.INTERNAL, "rotate key failed", false)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(w).Encode(map[string]string{"key": newFullKey})
	}
}

// RevokeKeysHandler handles DELETE /v1/keys/{id}: revokes an id owned by the
// authenticated customer via the existing Store.Revoke (immediate Redis cache
// invalidation, audit, and webhook emission all preserved).
func RevokeKeysHandler(store *Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rid, _ := r.Context().Value(mwpkg.RequestIDKey).(string)
		key := FromContext(r.Context())
		if key == nil {
			apierror.Write(w, rid, http.StatusUnauthorized, apierror.UNAUTHORIZED, "no auth context", false)
			return
		}

		id, ok := ownedKeyID(w, r, store, rid, key.Customer.ID)
		if !ok {
			return
		}

		if err := store.Revoke(r.Context(), id); err != nil {
			log.Error().Err(err).Str("request_id", rid).Msg("auth: revoke key failed")
			apierror.Write(w, rid, http.StatusInternalServerError, apierror.INTERNAL, "revoke key failed", false)
			return
		}

		w.WriteHeader(http.StatusNoContent)
	}
}
