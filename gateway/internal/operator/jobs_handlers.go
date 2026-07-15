package operator

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/zerolog/log"

	"github.com/Unluckyathecking/crucible/gateway/internal/apierror"
	"github.com/Unluckyathecking/crucible/gateway/internal/audit"
	mwpkg "github.com/Unluckyathecking/crucible/gateway/internal/middleware"
	"github.com/Unluckyathecking/crucible/gateway/internal/observability"
	"github.com/Unluckyathecking/crucible/gateway/internal/paging"
)

// jobsAdminActorID identifies the operator-token bearer in audit_log rows.
// Mirrors webhookout.replayActorID: the gateway currently validates operator
// access against a single static token, so there is no per-caller identity
// to attribute individual actions to yet.
const jobsAdminActorID = "operator"

// ActionJobRequeued and ActionJobsReleased are the stable audit_log actions
// recorded for AdminRequeueJobHandler and AdminReleaseJobsHandler respectively.
const (
	ActionJobRequeued  = "job.requeued"
	ActionJobsReleased = "jobs.released"
)

const (
	jobsAdminListDefaultPageSize = 20
	jobsAdminListMaxPageSize     = 100
)

// jobStatusQueued..jobStatusFailed mirror gateway/internal/jobs.Status* — the
// async_jobs.status CHECK constraint's frozen enum (see migrations/0019). Kept
// as local literals rather than imported from package jobs: see JobsAdminStore's
// doc comment for why this package cannot import jobs directly.
const (
	jobStatusSucceeded = "succeeded"
	jobStatusFailed    = "failed"
)

// validAdminJobStatuses is the accepted ?status= filter set for
// AdminListJobsHandler, mirroring server.validJobStatuses (which mirrors
// jobs.Status*) so an unrecognized value is rejected as 400 rather than
// silently matching zero rows.
var validAdminJobStatuses = map[string]bool{
	"queued": true, "running": true, jobStatusSucceeded: true, jobStatusFailed: true,
}

// AdminJob is the operator-visible, cross-customer projection of an
// async_jobs row that JobsAdminStore's methods operate on.
type AdminJob struct {
	ID            uuid.UUID
	CustomerID    uuid.UUID
	Operation     string
	Status        string
	Result        json.RawMessage
	UnitsLabel    string
	BillableUnits uint64
	ErrorCode     string
	ErrorMessage  string
	// ClaimedBy identifies the gateway instance currently holding a 'running'
	// row (nil otherwise) — what AdminReleaseJobsHandler's instance_id targets.
	ClaimedBy *uuid.UUID
	ClaimedAt *time.Time
	CreatedAt time.Time
	UpdatedAt time.Time
}

// JobsAdminStore is the subset of jobs.Store's admin surface these handlers
// need. Declared locally (the "accept interfaces" idiom) instead of taking a
// *jobs.Store parameter directly: package jobs imports webhookout for its
// job-completion webhook emission, and webhookout's own test suite
// (adminhttp_test.go, package webhookout) imports package operator for its
// cross-endpoint coverage. If this package imported jobs directly, building
// webhookout's test binary would require webhookout -> operator -> jobs ->
// webhookout, an import cycle. server/routes.go's jobsAdminAdapter wraps the
// real *jobs.Store to satisfy this interface — it can safely import both
// packages since nothing imports package server.
type JobsAdminStore interface {
	AdminList(ctx context.Context, status *string, limit, offset int) ([]AdminJob, int64, error)
	AdminGet(ctx context.Context, id uuid.UUID) (AdminJob, bool, error)
	Requeue(ctx context.Context, id uuid.UUID) error
	ReleaseClaimed(ctx context.Context, instanceID uuid.UUID) (int64, error)
}

// adminJobError mirrors server.asyncJobError's wire shape.
type adminJobError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// adminJobItem is the JSON shape shared by every /v1/admin/jobs* response
// that returns one job: the list, the single-job get, and the requeue
// action's echo of the row it just flipped. One converter
// (adminJobItemFrom) keeps all three in sync as AdminJob gains fields.
// Unlike server.jobListItem (customer-scoped), this surfaces customer_id and
// claimed_by/claimed_at — the cross-customer, operator-only view.
type adminJobItem struct {
	JobID         string          `json:"job_id"`
	CustomerID    string          `json:"customer_id"`
	Operation     string          `json:"operation"`
	Status        string          `json:"status"`
	Result        json.RawMessage `json:"result,omitempty"`
	BillableUnits uint64          `json:"billable_units,omitempty"`
	UnitsLabel    string          `json:"units_label,omitempty"`
	Error         *adminJobError  `json:"error,omitempty"`
	ClaimedBy     *string         `json:"claimed_by,omitempty"`
	ClaimedAt     *time.Time      `json:"claimed_at,omitempty"`
	CreatedAt     time.Time       `json:"created_at"`
	UpdatedAt     time.Time       `json:"updated_at"`
}

func adminJobItemFrom(j AdminJob) adminJobItem {
	it := adminJobItem{
		JobID:      j.ID.String(),
		CustomerID: j.CustomerID.String(),
		Operation:  j.Operation,
		Status:     j.Status,
		Result:     j.Result,
		UnitsLabel: j.UnitsLabel,
		ClaimedAt:  j.ClaimedAt,
		CreatedAt:  j.CreatedAt,
		UpdatedAt:  j.UpdatedAt,
	}
	if j.ClaimedBy != nil {
		claimedBy := j.ClaimedBy.String()
		it.ClaimedBy = &claimedBy
	}
	if j.Status == jobStatusSucceeded {
		it.BillableUnits = j.BillableUnits
	}
	if j.Status == jobStatusFailed {
		it.Error = &adminJobError{Code: j.ErrorCode, Message: j.ErrorMessage}
	}
	return it
}

// AdminListJobsHandler handles GET /v1/admin/jobs: a cross-customer,
// paginated view of async_jobs, optionally narrowed by ?status=. Unlike the
// customer-facing GET /v1/jobs, this intentionally has no customer scoping —
// that's the entire point of an operator console.
func AdminListJobsHandler(store JobsAdminStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rid, _ := r.Context().Value(mwpkg.RequestIDKey).(string)
		q := r.URL.Query()

		var status *string
		if v := q.Get("status"); v != "" {
			if !validAdminJobStatuses[v] {
				apierror.Write(w, rid, http.StatusBadRequest, apierror.BAD_REQUEST, "unknown status filter", false)
				return
			}
			status = &v
		}

		page, _ := strconv.Atoi(q.Get("page"))
		perPage, _ := strconv.Atoi(q.Get("per_page"))
		page, perPage = paging.Clamp(page, perPage, jobsAdminListDefaultPageSize, jobsAdminListMaxPageSize)
		offset, err := paging.Offset(page, perPage)
		if err != nil {
			apierror.Write(w, rid, http.StatusBadRequest, apierror.BAD_REQUEST, "page too large", false)
			return
		}

		adminJobs, total, err := store.AdminList(r.Context(), status, perPage, offset)
		if err != nil {
			log.Error().Err(err).Str("request_id", rid).Msg("operator: list jobs failed")
			apierror.Write(w, rid, http.StatusInternalServerError, apierror.INTERNAL, "query failed", false)
			return
		}

		items := make([]adminJobItem, 0, len(adminJobs))
		for _, j := range adminJobs {
			items = append(items, adminJobItemFrom(j))
		}
		writeJSON(w, paging.Page[adminJobItem]{Items: items, Total: total})
	}
}

// AdminGetJobHandler handles GET /v1/admin/jobs/{id}: a single job across any
// customer (unscoped read), 404 on unknown id.
func AdminGetJobHandler(store JobsAdminStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rid, _ := r.Context().Value(mwpkg.RequestIDKey).(string)

		id, err := uuid.Parse(chi.URLParam(r, "id"))
		if err != nil {
			apierror.Write(w, rid, http.StatusBadRequest, apierror.BAD_REQUEST, "invalid job id", false)
			return
		}

		job, ok, err := store.AdminGet(r.Context(), id)
		if err != nil {
			log.Error().Err(err).Str("request_id", rid).Msg("operator: get job failed")
			apierror.Write(w, rid, http.StatusInternalServerError, apierror.INTERNAL, "lookup failed", false)
			return
		}
		if !ok {
			apierror.Write(w, rid, http.StatusNotFound, "NOT_FOUND", "job not found", false)
			return
		}

		writeJSON(w, adminJobItemFrom(job))
	}
}

// AdminRequeueJobHandler handles POST /v1/admin/jobs/{id}/requeue: flips a
// claimed/failed/dead-lettered row back to queued via the underlying
// jobs.Store.Requeue. db is used only to emit a best-effort audit_log row.
func AdminRequeueJobHandler(store JobsAdminStore, db *pgxpool.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rid, _ := r.Context().Value(mwpkg.RequestIDKey).(string)

		id, err := uuid.Parse(chi.URLParam(r, "id"))
		if err != nil {
			apierror.Write(w, rid, http.StatusBadRequest, apierror.BAD_REQUEST, "invalid job id", false)
			return
		}

		// jobs.Store.Requeue is an unconditional UPDATE ... WHERE id=$1 with no
		// rows-affected signal, so a 404 on an unknown id has to be established
		// with a lookup here rather than inferred from Requeue's own return.
		if _, ok, err := store.AdminGet(r.Context(), id); err != nil {
			log.Error().Err(err).Str("request_id", rid).Msg("operator: requeue job lookup failed")
			apierror.Write(w, rid, http.StatusInternalServerError, apierror.INTERNAL, "lookup failed", false)
			return
		} else if !ok {
			apierror.Write(w, rid, http.StatusNotFound, "NOT_FOUND", "job not found", false)
			return
		}

		if err := store.Requeue(r.Context(), id); err != nil {
			log.Error().Err(err).Str("request_id", rid).Msg("operator: requeue job failed")
			apierror.Write(w, rid, http.StatusInternalServerError, apierror.INTERNAL, "requeue failed", false)
			return
		}
		observability.JobsRequeuedTotal.Inc()
		emitJobAudit(r.Context(), db, rid, ActionJobRequeued, "async_job", id.String())

		job, ok, err := store.AdminGet(r.Context(), id)
		if err != nil || !ok {
			log.Error().Err(err).Str("request_id", rid).Msg("operator: requeue job re-fetch failed")
			apierror.Write(w, rid, http.StatusInternalServerError, apierror.INTERNAL, "requeue succeeded but re-fetch failed", false)
			return
		}
		writeJSON(w, adminJobItemFrom(job))
	}
}

// releaseJobsRequest is the JSON body POST /v1/admin/jobs/release expects.
type releaseJobsRequest struct {
	InstanceID string `json:"instance_id"`
}

// releaseJobsResponse is the JSON body POST /v1/admin/jobs/release returns.
type releaseJobsResponse struct {
	Released int64 `json:"released"`
}

// AdminReleaseJobsHandler handles POST /v1/admin/jobs/release: force-releases
// every 'running' job claimed by instance_id back to 'queued' via the
// underlying jobs.Store.ReleaseClaimed, which is itself scoped to that
// instance id and so can never touch another gateway process's in-flight
// work. Intended for an operator who has positively confirmed the claiming
// instance is dead — releasing a still-live instance's jobs risks a second,
// concurrent execution (see ReleaseClaimed's doc comment in jobs/store.go).
func AdminReleaseJobsHandler(store JobsAdminStore, db *pgxpool.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rid, _ := r.Context().Value(mwpkg.RequestIDKey).(string)

		var body releaseJobsRequest
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			apierror.Write(w, rid, http.StatusBadRequest, apierror.BAD_REQUEST, "invalid json body", false)
			return
		}
		instanceID, err := uuid.Parse(body.InstanceID)
		if err != nil {
			apierror.Write(w, rid, http.StatusBadRequest, apierror.BAD_REQUEST, "invalid or missing instance_id", false)
			return
		}

		n, err := store.ReleaseClaimed(r.Context(), instanceID)
		if err != nil {
			log.Error().Err(err).Str("request_id", rid).Msg("operator: release jobs failed")
			apierror.Write(w, rid, http.StatusInternalServerError, apierror.INTERNAL, "release failed", false)
			return
		}
		observability.JobsReleasedTotal.Add(float64(n))
		if n > 0 {
			emitJobAudit(r.Context(), db, rid, ActionJobsReleased, "gateway_instance", instanceID.String())
		}

		writeJSON(w, releaseJobsResponse{Released: n})
	}
}

// emitJobAudit writes one audit_log row for an operator-triggered job
// mutation. Best-effort: the mutation itself already succeeded, so an
// audit-write failure is logged and swallowed rather than surfaced to the
// caller — mirrors webhookout.emitReplayAudit.
func emitJobAudit(ctx context.Context, db *pgxpool.Pool, requestID, action, targetType, targetID string) {
	if err := audit.Emit(ctx, db, audit.Event{
		ActorType:  audit.ActorAdmin,
		ActorID:    jobsAdminActorID,
		Action:     action,
		TargetType: &targetType,
		TargetID:   &targetID,
	}); err != nil {
		log.Warn().Err(err).Str("request_id", requestID).Str("target_id", targetID).
			Msg("operator: audit emit failed for job mutation")
	}
}
