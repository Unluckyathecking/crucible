package webhookout

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/zerolog/log"

	"github.com/Unluckyathecking/crucible/gateway/internal/apierror"
	mwpkg "github.com/Unluckyathecking/crucible/gateway/internal/middleware"
)

// CustomerIDFunc extracts the authenticated customer id from a request,
// returning ok=false when no customer is authenticated. Handlers take this as
// a parameter rather than importing gateway/internal/auth directly: auth
// imports this package (for the Emitter type used by Store.SetEmitter), so
// webhookout importing auth back would be an import cycle. routes.go supplies
// an auth.FromContext-backed implementation at route registration time.
type CustomerIDFunc func(r *http.Request) (uuid.UUID, bool)

// createEndpointRequest is the POST /v1/webhooks/endpoints request body.
// SubscribedEvents omitted (absent from the JSON body) decodes to nil,
// meaning "every event type"; an explicit (possibly empty) array restricts
// delivery to that subset — see Endpoint.SubscribedEvents.
type createEndpointRequest struct {
	URL              string   `json:"url"`
	SubscribedEvents []string `json:"subscribed_events"`
}

// CreateEndpointHandler handles POST /v1/webhooks/endpoints: registers a new
// outbound webhook endpoint for the authenticated customer. The response body
// carries the signing secret exactly once.
func CreateEndpointHandler(db *pgxpool.Pool, customerID CustomerIDFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rid, _ := r.Context().Value(mwpkg.RequestIDKey).(string)
		custID, ok := customerID(r)
		if !ok {
			apierror.Write(w, rid, http.StatusUnauthorized, apierror.UNAUTHORIZED, "no auth context", false)
			return
		}

		var body createEndpointRequest
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			apierror.Write(w, rid, http.StatusBadRequest, apierror.BAD_REQUEST, "invalid json body", false)
			return
		}

		created, err := CreateEndpoint(r.Context(), db, custID, body.URL, body.SubscribedEvents)
		if err != nil {
			var ve *validationError
			if errors.As(err, &ve) {
				apierror.Write(w, rid, http.StatusBadRequest, apierror.BAD_REQUEST, ve.Error(), false)
				return
			}
			log.Error().Err(err).Str("request_id", rid).Msg("webhookout: create endpoint failed")
			apierror.Write(w, rid, http.StatusInternalServerError, apierror.INTERNAL, "create endpoint failed", false)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(created)
	}
}

// ListEndpointsHandler handles GET /v1/webhooks/endpoints: the authenticated
// customer's active endpoints. Never serializes a secret field.
func ListEndpointsHandler(db *pgxpool.Pool, customerID CustomerIDFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rid, _ := r.Context().Value(mwpkg.RequestIDKey).(string)
		custID, ok := customerID(r)
		if !ok {
			apierror.Write(w, rid, http.StatusUnauthorized, apierror.UNAUTHORIZED, "no auth context", false)
			return
		}

		items, err := ListEndpoints(r.Context(), db, custID)
		if err != nil {
			log.Error().Err(err).Str("request_id", rid).Msg("webhookout: list endpoints failed")
			apierror.Write(w, rid, http.StatusInternalServerError, apierror.INTERNAL, "list endpoints failed", false)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(w).Encode(items)
	}
}

// DeleteEndpointHandler handles DELETE /v1/webhooks/endpoints/{id}: deactivates
// an endpoint owned by the authenticated customer. An id owned by another
// customer, or that doesn't exist, returns 404 either way (IDOR-safe).
func DeleteEndpointHandler(db *pgxpool.Pool, customerID CustomerIDFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rid, _ := r.Context().Value(mwpkg.RequestIDKey).(string)
		custID, ok := customerID(r)
		if !ok {
			apierror.Write(w, rid, http.StatusUnauthorized, apierror.UNAUTHORIZED, "no auth context", false)
			return
		}

		id, err := uuid.Parse(chi.URLParam(r, "id"))
		if err != nil {
			apierror.Write(w, rid, http.StatusBadRequest, apierror.BAD_REQUEST, "invalid endpoint id", false)
			return
		}

		if err := DeleteEndpoint(r.Context(), db, id, custID); err != nil {
			if errors.Is(err, ErrEndpointNotFound) {
				apierror.Write(w, rid, http.StatusNotFound, "NOT_FOUND", "webhook endpoint not found", false)
				return
			}
			log.Error().Err(err).Str("request_id", rid).Msg("webhookout: delete endpoint failed")
			apierror.Write(w, rid, http.StatusInternalServerError, apierror.INTERNAL, "delete endpoint failed", false)
			return
		}

		w.WriteHeader(http.StatusNoContent)
	}
}
