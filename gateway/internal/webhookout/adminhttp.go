package webhookout

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/zerolog/log"

	"github.com/Unluckyathecking/crucible/gateway/internal/apierror"
	"github.com/Unluckyathecking/crucible/gateway/internal/audit"
	mwpkg "github.com/Unluckyathecking/crucible/gateway/internal/middleware"
)

// replayActorID identifies the operator-token bearer in audit_log rows. The
// gateway currently validates operator access against a single static token
// (see operator.Middleware and the operator_tokens migration comment), so
// there is no per-caller identity to attribute individual replays to yet.
const replayActorID = "operator"

// requeuedResponse is the JSON body for both single and bulk replay endpoints.
type requeuedResponse struct {
	Requeued int `json:"requeued"`
}

// ListDeadLettersHandler handles GET /v1/admin/webhooks/deadletters.
// Query params: page (default 1), per_page (default 20).
func ListDeadLettersHandler(db *pgxpool.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rid, _ := r.Context().Value(mwpkg.RequestIDKey).(string)
		q := r.URL.Query()
		page, _ := strconv.Atoi(q.Get("page"))
		perPage, _ := strconv.Atoi(q.Get("per_page"))

		result, err := ListDeadLetters(r.Context(), db, DeadLettersFilter{Page: page, PerPage: perPage})
		if err != nil {
			log.Error().Err(err).Str("request_id", rid).Msg("webhookout: list dead letters failed")
			apierror.Write(w, rid, http.StatusInternalServerError, apierror.INTERNAL, "query failed", false)
			return
		}

		writeJSON(w, result)
	}
}

// ReplaySingleHandler handles POST /v1/admin/webhooks/deadletters/{id}/replay.
func ReplaySingleHandler(db *pgxpool.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rid, _ := r.Context().Value(mwpkg.RequestIDKey).(string)

		id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
		if err != nil {
			apierror.Write(w, rid, http.StatusBadRequest, apierror.BAD_REQUEST, "invalid delivery id", false)
			return
		}

		if err := ReplayByID(r.Context(), db, id); err != nil {
			if err == pgx.ErrNoRows {
				apierror.Write(w, rid, http.StatusNotFound, "NOT_FOUND", "dead-letter delivery not found", false)
				return
			}
			if err == ErrEndpointInactive {
				apierror.Write(w, rid, http.StatusConflict, "ENDPOINT_INACTIVE", "endpoint is inactive; reactivate it before replaying", false)
				return
			}
			log.Error().Err(err).Str("request_id", rid).Msg("webhookout: replay by id failed")
			apierror.Write(w, rid, http.StatusInternalServerError, apierror.INTERNAL, "replay failed", false)
			return
		}

		emitReplayAudit(r.Context(), db, rid, id)
		writeJSON(w, requeuedResponse{Requeued: 1})
	}
}

// ReplayBulkHandler handles POST /v1/admin/webhooks/deadletters/replay?endpoint_id=<uuid>.
func ReplayBulkHandler(db *pgxpool.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rid, _ := r.Context().Value(mwpkg.RequestIDKey).(string)

		endpointID, err := uuid.Parse(r.URL.Query().Get("endpoint_id"))
		if err != nil {
			apierror.Write(w, rid, http.StatusBadRequest, apierror.BAD_REQUEST, "invalid or missing endpoint_id", false)
			return
		}

		ids, err := ReplayByEndpoint(r.Context(), db, endpointID)
		if err != nil {
			log.Error().Err(err).Str("request_id", rid).Msg("webhookout: replay by endpoint failed")
			apierror.Write(w, rid, http.StatusInternalServerError, apierror.INTERNAL, "replay failed", false)
			return
		}

		for _, id := range ids {
			emitReplayAudit(r.Context(), db, rid, id)
		}
		writeJSON(w, requeuedResponse{Requeued: len(ids)})
	}
}

// emitReplayAudit writes one audit_log row per requeued delivery. Best-effort:
// the requeue itself already succeeded, so an audit-write failure is logged
// and swallowed rather than surfaced to the caller (mirrors usage.Recorder's
// and errorlog.ErrorRecorder's best-effort-write convention elsewhere in the gateway).
func emitReplayAudit(ctx context.Context, db *pgxpool.Pool, requestID string, deliveryID int64) {
	targetType := "webhook_delivery"
	targetID := strconv.FormatInt(deliveryID, 10)
	if err := audit.Emit(ctx, db, audit.Event{
		ActorType:  audit.ActorAdmin,
		ActorID:    replayActorID,
		Action:     ActionDeliveryReplayed,
		TargetType: &targetType,
		TargetID:   &targetID,
	}); err != nil {
		log.Warn().Err(err).Str("request_id", requestID).Int64("delivery_id", deliveryID).
			Msg("webhookout: audit emit failed for replay")
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(v)
}
