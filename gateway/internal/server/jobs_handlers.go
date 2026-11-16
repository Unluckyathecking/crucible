package server

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/rs/zerolog/log"

	"github.com/Unluckyathecking/crucible/gateway/internal/apierror"
	"github.com/Unluckyathecking/crucible/gateway/internal/auth"
	"github.com/Unluckyathecking/crucible/gateway/internal/jobs"
	mw "github.com/Unluckyathecking/crucible/gateway/internal/middleware"
	"github.com/Unluckyathecking/crucible/gateway/internal/observability"
	"github.com/Unluckyathecking/crucible/gateway/internal/operator"
	"github.com/Unluckyathecking/crucible/gateway/internal/paging"
	"github.com/Unluckyathecking/crucible/gateway/internal/quota"
)

// jobsAdminAdapter bridges jobs.Store's admin methods to
// operator.JobsAdminStore, translating jobs.AdminJob into operator.AdminJob.
// Lives here — not in package operator or package jobs — because package
// jobs already imports webhookout, and webhookout's own test suite
// (adminhttp_test.go) imports package operator for its cross-endpoint
// coverage; operator importing jobs directly would create an import cycle
// in webhookout's test binary. Package server already imports both without
// issue, since nothing imports package server.
type jobsAdminAdapter struct{ store *jobs.Store }

func (a jobsAdminAdapter) AdminList(ctx context.Context, status *string, limit, offset int) ([]operator.AdminJob, int64, error) {
	rows, total, err := a.store.AdminList(ctx, status, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	out := make([]operator.AdminJob, len(rows))
	for i, j := range rows {
		out[i] = adminJobFromStoreRow(j)
	}
	return out, total, nil
}

func (a jobsAdminAdapter) AdminGet(ctx context.Context, id uuid.UUID) (operator.AdminJob, bool, error) {
	j, ok, err := a.store.AdminGet(ctx, id)
	if err != nil || !ok {
		return operator.AdminJob{}, ok, err
	}
	return adminJobFromStoreRow(j), true, nil
}

func (a jobsAdminAdapter) Requeue(ctx context.Context, id uuid.UUID) (bool, error) {
	return a.store.Requeue(ctx, id)
}

func (a jobsAdminAdapter) ReleaseClaimed(ctx context.Context, instanceID uuid.UUID) (int64, error) {
	return a.store.ReleaseClaimed(ctx, instanceID)
}

func adminJobFromStoreRow(j jobs.AdminJob) operator.AdminJob {
	return operator.AdminJob{
		ID:            j.ID,
		CustomerID:    j.CustomerID,
		Operation:     j.Operation,
		Status:        j.Status,
		Result:        j.Result,
		UnitsLabel:    j.UnitsLabel,
		BillableUnits: j.BillableUnits,
		ErrorCode:     j.ErrorCode,
		ErrorMessage:  j.ErrorMessage,
		ClaimedBy:     j.ClaimedBy,
		ClaimedAt:     j.ClaimedAt,
		CreatedAt:     j.CreatedAt,
		UpdatedAt:     j.UpdatedAt,
	}
}

// asyncJobResponse is the JSON shape GET /v1/jobs/{id} returns.
type asyncJobResponse struct {
	JobID         string          `json:"job_id"`
	Status        string          `json:"status"`
	Result        json.RawMessage `json:"result,omitempty"`
	Error         *asyncJobError  `json:"error,omitempty"`
	BillableUnits uint64          `json:"billable_units,omitempty"`
	UnitsLabel    string          `json:"units_label,omitempty"`
	CreatedAt     time.Time       `json:"created_at"`
	UpdatedAt     time.Time       `json:"updated_at"`
}

type asyncJobError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// jobBacklogExceededCode is enqueueAsync's stable error code for a
// JOB_MAX_QUEUED_PER_CUSTOMER rejection. Declared here rather than added to
// apierror's shared constant block (out of this feature's file scope) —
// apierror.Write accepts any string code, so a package-local constant gives
// the same stability/discoverability for this one route without touching
// that file.
const jobBacklogExceededCode = "JOB_BACKLOG_EXCEEDED"

// enqueueAsync handles POST /v1/<op> for a route opted into AsyncRoutes: it
// decodes the request body exactly like invoke does, then persists a queued
// job row instead of calling the worker inline, and returns 202 {job_id}.
// The worker invocation, billable_units contract check, and usage.Recorder
// call happen later, out of band, in jobs.Executor.process — the caller
// polls GET /v1/jobs/{id} for the eventual result.
//
// maxQueuedPerCustomer <= 0 (the config.Config.JobMaxQueuedPerCustomer
// zero-value default) admits unconditionally, exactly as before this knob
// existed. > 0 ceilings the caller's queued+running backlog
// (jobs.Store.CountActive); once reached, enqueue is rejected with 429
// jobBacklogExceededCode rather than growing an unbounded backlog for one
// customer at every other tenant's expense.
func enqueueAsync(store *jobs.Store, operation string, timeoutSeconds, maxQueuedPerCustomer int) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rid, _ := r.Context().Value(mw.RequestIDKey).(string)
		if store == nil {
			apierror.Write(w, rid, http.StatusServiceUnavailable, apierror.NOT_CONFIGURED, "async jobs not configured", false)
			return
		}
		key := auth.FromContext(r.Context())
		if key == nil {
			apierror.Write(w, rid, http.StatusUnauthorized, apierror.UNAUTHORIZED, "no auth context", false)
			return
		}

		if maxQueuedPerCustomer > 0 {
			active, err := store.CountActive(r.Context(), key.Customer.ID)
			if err != nil {
				log.Error().Err(err).Str("request_id", rid).Str("operation", operation).Msg("jobs: backlog check failed")
				apierror.Write(w, rid, http.StatusInternalServerError, apierror.INTERNAL, "enqueue failed", false)
				return
			}
			if active >= int64(maxQueuedPerCustomer) {
				observability.JobsCustomerThrottledTotal.WithLabelValues("backlog_ceiling").Inc()
				apierror.Write(w, rid, http.StatusTooManyRequests, jobBacklogExceededCode, "async job backlog limit exceeded", true)
				return
			}
		}

		var payload json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			apierror.Write(w, rid, http.StatusBadRequest, apierror.BAD_REQUEST, "invalid json body", false)
			return
		}

		// Passing the Idempotency-Key header through to Store.Enqueue (not
		// consuming/stripping it — idempotency.Middleware still owns the
		// response-replay behavior above this handler) closes a narrower
		// race: if that middleware's own finalize step fails after this
		// insert already committed, it releases the key so a client retry
		// reaches this handler again; Enqueue returns the existing job's id
		// instead of inserting a duplicate for the retry.
		idempotencyKey := r.Header.Get("Idempotency-Key")
		id, err := store.Enqueue(r.Context(), key.Customer.ID, key.ID, operation, rid, key.Customer.Plan, payload, timeoutSeconds, idempotencyKey)
		if err != nil {
			log.Error().Err(err).Str("request_id", rid).Str("operation", operation).Msg("jobs: enqueue failed")
			apierror.Write(w, rid, http.StatusInternalServerError, apierror.INTERNAL, "enqueue failed", false)
			return
		}
		observability.JobsEnqueuedTotal.WithLabelValues(operation).Inc()
		// quota.Middleware already reserved +1 up front and will refund it the
		// instant this handler returns unless the signal is flipped — a plain
		// enqueue-and-return-202 would let a capped-plan customer enqueue an
		// unbounded number of jobs (each reserve is refunded before the next
		// request's Reserve ever sees it), bypassing the admission check this
		// route is supposed to inherit. Marking recorded makes the +1 reserve
		// stick per enqueued job — mirroring the sync path's own "1 (reserve) +
		// units (recorder)" per-call overhead (see quota.Tracker.Reserve's doc
		// comment) — and the real billable_units still land on top of it when
		// the job completes (usage.Recorder.Record, called from the executor).
		// A job that later fails still consumes this 1 unit; that's a
		// deliberate fail-closed tradeoff over silently exempting async
		// enqueues from the cap entirely.
		quota.MarkRecorded(r.Context())

		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(map[string]string{"job_id": id.String()})
	}
}

// jobsGetHandler handles GET /v1/jobs/{id}: the caller's own job status and
// result, scoped at the SQL level by jobs.Store.Get (IDOR-safe — a job id
// owned by another customer returns 404, indistinguishable from a
// nonexistent id).
func jobsGetHandler(store *jobs.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rid, _ := r.Context().Value(mw.RequestIDKey).(string)
		if store == nil {
			apierror.Write(w, rid, http.StatusServiceUnavailable, apierror.NOT_CONFIGURED, "async jobs not configured", false)
			return
		}
		key := auth.FromContext(r.Context())
		if key == nil {
			apierror.Write(w, rid, http.StatusUnauthorized, apierror.UNAUTHORIZED, "no auth context", false)
			return
		}

		id, err := uuid.Parse(chi.URLParam(r, "id"))
		if err != nil {
			apierror.Write(w, rid, http.StatusBadRequest, apierror.BAD_REQUEST, "invalid job id", false)
			return
		}

		job, ok, err := store.Get(r.Context(), id, key.Customer.ID)
		if err != nil {
			log.Error().Err(err).Str("request_id", rid).Msg("jobs: get failed")
			apierror.Write(w, rid, http.StatusInternalServerError, apierror.INTERNAL, "lookup failed", false)
			return
		}
		if !ok {
			apierror.Write(w, rid, http.StatusNotFound, "NOT_FOUND", "job not found", false)
			return
		}

		resp := asyncJobResponse{
			JobID:      job.ID.String(),
			Status:     job.Status,
			Result:     job.Result,
			UnitsLabel: job.UnitsLabel,
			CreatedAt:  job.CreatedAt,
			UpdatedAt:  job.UpdatedAt,
		}
		if job.Status == jobs.StatusSucceeded {
			resp.BillableUnits = job.BillableUnits
		}
		if job.Status == jobs.StatusFailed {
			resp.Error = &asyncJobError{Code: job.ErrorCode, Message: job.ErrorMessage}
		}

		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(w).Encode(resp)
	}
}

// jobsCancelHandler handles POST /v1/jobs/{id}/cancel: withdraws the
// caller's own job while it is still queued, via jobs.Store.CancelQueued's
// single atomic UPDATE ... WHERE id=$1 AND customer_id=$2 AND status='queued'
// (IDOR-safe and status-guarded in one statement, unlike the admin requeue
// handler's separate lookup-then-update — see AdminRequeueJobHandler). When
// CancelQueued reports no row changed, a follow-up Get (itself IDOR-safe)
// distinguishes the two possible causes: the job doesn't exist or isn't
// owned by this customer (404, indistinguishable by design, matching
// jobsGetHandler), or it exists but is no longer queued (409
// JOB_NOT_CANCELLABLE, mirroring AdminRequeueJobHandler's
// JOB_NOT_REQUEUABLE guard for the same reason — a running job may already
// be mid-flight, and a succeeded/failed/cancelled job is already terminal).
// This cycle has no cooperative cancel of a RUNNING job: it always 409s.
func jobsCancelHandler(store *jobs.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rid, _ := r.Context().Value(mw.RequestIDKey).(string)
		if store == nil {
			apierror.Write(w, rid, http.StatusServiceUnavailable, apierror.NOT_CONFIGURED, "async jobs not configured", false)
			return
		}
		key := auth.FromContext(r.Context())
		if key == nil {
			apierror.Write(w, rid, http.StatusUnauthorized, apierror.UNAUTHORIZED, "no auth context", false)
			return
		}

		id, err := uuid.Parse(chi.URLParam(r, "id"))
		if err != nil {
			apierror.Write(w, rid, http.StatusBadRequest, apierror.BAD_REQUEST, "invalid job id", false)
			return
		}

		cancelled, err := store.CancelQueued(r.Context(), id, key.Customer.ID)
		if err != nil {
			log.Error().Err(err).Str("request_id", rid).Msg("jobs: cancel failed")
			apierror.Write(w, rid, http.StatusInternalServerError, apierror.INTERNAL, "cancel failed", false)
			return
		}

		if !cancelled {
			_, ok, err := store.Get(r.Context(), id, key.Customer.ID)
			if err != nil {
				log.Error().Err(err).Str("request_id", rid).Msg("jobs: cancel lookup failed")
				apierror.Write(w, rid, http.StatusInternalServerError, apierror.INTERNAL, "lookup failed", false)
				return
			}
			if !ok {
				apierror.Write(w, rid, http.StatusNotFound, "NOT_FOUND", "job not found", false)
				return
			}
			apierror.Write(w, rid, http.StatusConflict, "JOB_NOT_CANCELLABLE", "job cannot be cancelled in its current state", false)
			return
		}

		job, ok, err := store.Get(r.Context(), id, key.Customer.ID)
		if err != nil || !ok {
			log.Error().Err(err).Str("request_id", rid).Msg("jobs: cancel re-fetch failed")
			apierror.Write(w, rid, http.StatusInternalServerError, apierror.INTERNAL, "cancel succeeded but re-fetch failed", false)
			return
		}

		resp := asyncJobResponse{
			JobID:      job.ID.String(),
			Status:     job.Status,
			Result:     job.Result,
			UnitsLabel: job.UnitsLabel,
			CreatedAt:  job.CreatedAt,
			UpdatedAt:  job.UpdatedAt,
		}

		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(w).Encode(resp)
	}
}

// validJobStatuses is the set of accepted ?status= filter values for
// jobsListHandler, mirrored from jobs.Status* so an unrecognized value can be
// rejected as 400 rather than silently matching zero rows.
var validJobStatuses = map[string]bool{
	jobs.StatusQueued:    true,
	jobs.StatusRunning:   true,
	jobs.StatusSucceeded: true,
	jobs.StatusFailed:    true,
	jobs.StatusCancelled: true,
}

const (
	jobsListDefaultPageSize = 20
	jobsListMaxPageSize     = 100
)

// jobListItem is one row of GET /v1/jobs's "items" list. Mirrors
// asyncJobResponse's status-conditional fields (billable_units only once
// succeeded, error only once failed) plus operation, so a caller can tell
// which POST /v1/<op> route enqueued each job without a second round trip;
// omits result/payload to keep the list response bounded regardless of how
// large an individual job's worker output is.
type jobListItem struct {
	JobID         string         `json:"job_id"`
	Operation     string         `json:"operation"`
	Status        string         `json:"status"`
	BillableUnits uint64         `json:"billable_units,omitempty"`
	UnitsLabel    string         `json:"units_label,omitempty"`
	Error         *asyncJobError `json:"error,omitempty"`
	CreatedAt     time.Time      `json:"created_at"`
	UpdatedAt     time.Time      `json:"updated_at"`
}

// jobsListHandler handles GET /v1/jobs: a paginated history of the caller's
// own async jobs, scoped at the SQL level by jobs.Store.List (IDOR-safe,
// mirroring jobsGetHandler), optionally narrowed by ?status= (validated
// against jobs.Status*) and/or ?operation= (opaque, unvalidated — matches an
// exact enqueued operation string). Paginated via the shared paging package,
// same page/per_page contract as webhookDeliveriesHandler.
func jobsListHandler(store *jobs.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rid, _ := r.Context().Value(mw.RequestIDKey).(string)
		if store == nil {
			apierror.Write(w, rid, http.StatusServiceUnavailable, apierror.NOT_CONFIGURED, "async jobs not configured", false)
			return
		}
		key := auth.FromContext(r.Context())
		if key == nil {
			apierror.Write(w, rid, http.StatusUnauthorized, apierror.UNAUTHORIZED, "no auth context", false)
			return
		}

		q := r.URL.Query()
		var status *string
		if v := q.Get("status"); v != "" {
			if !validJobStatuses[v] {
				apierror.Write(w, rid, http.StatusBadRequest, apierror.BAD_REQUEST, "unknown status filter", false)
				return
			}
			status = &v
		}
		var operation *string
		if v := q.Get("operation"); v != "" {
			operation = &v
		}

		pp := paging.ParseQuery(q, "per_page")
		page, perPage := paging.Clamp(pp.Page, pp.PerPage, jobsListDefaultPageSize, jobsListMaxPageSize)
		offset, err := paging.Offset(page, perPage)
		if err != nil {
			apierror.Write(w, rid, http.StatusBadRequest, apierror.BAD_REQUEST, "page too large", false)
			return
		}

		jobRows, total, err := store.List(r.Context(), key.Customer.ID, status, operation, perPage, offset)
		if err != nil {
			log.Error().Err(err).Str("request_id", rid).Msg("jobs: list failed")
			apierror.Write(w, rid, http.StatusInternalServerError, apierror.INTERNAL, "list failed", false)
			return
		}

		items := make([]jobListItem, 0, len(jobRows))
		for _, j := range jobRows {
			it := jobListItem{
				JobID:      j.ID.String(),
				Operation:  j.Operation,
				Status:     j.Status,
				UnitsLabel: j.UnitsLabel,
				CreatedAt:  j.CreatedAt,
				UpdatedAt:  j.UpdatedAt,
			}
			if j.Status == jobs.StatusSucceeded {
				it.BillableUnits = j.BillableUnits
			}
			if j.Status == jobs.StatusFailed {
				it.Error = &asyncJobError{Code: j.ErrorCode, Message: j.ErrorMessage}
			}
			items = append(items, it)
		}

		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(w).Encode(paging.Page[jobListItem]{Items: items, Total: total})
	}
}

// v1ErrorCapture returns a middleware that wraps /v1 responses with an
// errorlog.Capture recorder and fires an async error_events insert on status >= 400.
// When capturePayload is true, the request body is buffered before dispatch and
// stored on the row. The payload never appears in logs or metric labels.
