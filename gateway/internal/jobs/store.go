package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Unluckyathecking/crucible/gateway/internal/tracing"
	"github.com/Unluckyathecking/crucible/gateway/internal/webhookout"
)

// defaultStuckJobTimeout is Store's built-in fallback for the crash-recovery
// sweep's per-row threshold (see DefaultJobTimeout) when the caller never
// overrides it. Matches config.Config's JOB_TIMEOUT_MS default (300000ms).
const defaultStuckJobTimeout = 5 * time.Minute

// stuckJobGrace is added on top of a row's own timeout (DefaultJobTimeout,
// or its timeout_seconds override) before the crash-recovery sweep
// considers it abandoned — slack for claim/dispatch/DB-write overhead
// around the worker call itself, so a job that finishes right at its
// deadline isn't racing the sweep.
const stuckJobGrace = 60 * time.Second

// Store is the durable Postgres-backed async job queue.
type Store struct {
	db *pgxpool.Pool
	// DefaultJobTimeout is the crash-recovery sweep's fallback threshold
	// (see Claim) for rows with timeout_seconds = 0 (no per-route
	// AsyncRoutes override) — i.e. the "how long can a legitimately running
	// job take" question, mirrored from jobs.ExecutorConfig.JobTimeout.
	//
	// A fixed threshold here (the original design) was a real bug: any row
	// still running past that fixed age — even though it's well within its
	// own configured budget — gets swept back to 'queued' and can be
	// claimed and executed a second time while the first attempt is still
	// in flight, racing their unscoped Complete/Fail updates and risking
	// duplicate work or double billing. Tying the threshold to each row's
	// own timeout (or this default) instead of a constant closes that gap.
	//
	// jobs.NewExecutor sets this to its own ExecutorConfig.JobTimeout so
	// the two stay in sync by construction; defaults to
	// defaultStuckJobTimeout for callers that construct a Store directly.
	DefaultJobTimeout time.Duration
	// emitter is optional; nil → Complete/Fail/DeadLetter enqueue no
	// outbound webhook event. Typed as the narrow txEmitter interface
	// (rather than *webhookout.Emitter directly) purely so tests can inject
	// a fake that fails deterministically; SetEmitter is still the only
	// production call path and preserves *webhookout.Emitter's own
	// nil-receiver safety (see SetEmitter's doc comment).
	emitter txEmitter
}

// txEmitter is the subset of *webhookout.Emitter's transactional API Store
// needs for the terminal-transition webhook enqueue.
type txEmitter interface {
	EmitTx(ctx context.Context, tx pgx.Tx, customerID uuid.UUID, eventType string, payload []byte) error
}

// SetEmitter wires the outbound webhook emitter used to notify customers of
// job.succeeded/job.failed terminal transitions, mirroring
// auth.Store.SetEmitter and billing.Webhook.SetEmitter. A nil e clears any
// previously set emitter rather than storing a typed-nil interface value —
// webhookout.NewEmitter returns nil when its DB dependency is unset, and
// Executor.SetEmitter forwards exactly that value here, so this must treat
// nil as "no emitter" the same way e.EmitTx(...) itself nil-checks its
// receiver, not skip emission via a non-nil interface wrapping a nil pointer.
func (s *Store) SetEmitter(e *webhookout.Emitter) {
	if s == nil {
		return
	}
	if e == nil {
		s.emitter = nil
		return
	}
	s.emitter = e
}

// NewStore returns nil when db is nil, matching the optional-Deps nil-safe
// pattern used by webhookout.NewEmitter — every exported method nil-checks
// its receiver, so callers need not nil-check the dependency first.
func NewStore(db *pgxpool.Pool) *Store {
	if db == nil {
		return nil
	}
	return &Store{db: db, DefaultJobTimeout: defaultStuckJobTimeout}
}

// Enqueue inserts a queued job row and returns its generated id.
// timeoutSeconds is the per-route override from routes_table.go's
// AsyncRoutes; <= 0 means "use the executor's configured default".
// idempotencyKey is the caller's Idempotency-Key header value, or "" if
// absent. When non-empty and a job already exists for
// (customerID, idempotencyKey) — because idempotency.Middleware's finalize
// step failed after a prior call to Enqueue already committed, and a client
// retry reached this handler again — Enqueue returns that existing job's id
// instead of inserting a duplicate, so the retry can't cause the worker to
// run (and bill) a second time for what the client intended as one request.
func (s *Store) Enqueue(ctx context.Context, customerID, apiKeyID uuid.UUID, operation, requestID, plan string, payload json.RawMessage, timeoutSeconds int, idempotencyKey string) (uuid.UUID, error) {
	if s == nil {
		return uuid.Nil, fmt.Errorf("jobs: store is nil")
	}
	if timeoutSeconds < 0 {
		timeoutSeconds = 0
	}
	// NULL, not '', when absent: the unique index is partial on
	// "idempotency_key IS NOT NULL", so every key-less request must store
	// NULL or they'd all collide with each other as if they shared one
	// empty key.
	var idemKeyParam *string
	if idempotencyKey != "" {
		idemKeyParam = &idempotencyKey
	}

	// Captured from whatever span (if any) is active in ctx when the
	// synchronous /v1 enqueue handler calls this — tracing.Middleware places
	// one there when OTEL tracing is enabled. "" (tracing disabled, or no
	// active span) stores NULL, never an empty string, so
	// TraceparentsByID/tracing.RestoreTraceparent's no-op-on-absent path is
	// exact rather than approximate.
	var traceparentParam *string
	if tv := tracing.CaptureTraceparent(ctx); tv != "" {
		traceparentParam = &tv
	}

	var id uuid.UUID
	err := s.db.QueryRow(ctx, `
		INSERT INTO async_jobs (customer_id, api_key_id, operation, request_id, plan, payload, timeout_seconds, idempotency_key, status, traceparent)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, 'queued', $9)
		ON CONFLICT (customer_id, idempotency_key) WHERE idempotency_key IS NOT NULL DO NOTHING
		RETURNING id
	`, customerID, apiKeyID, operation, requestID, plan, []byte(payload), timeoutSeconds, idemKeyParam, traceparentParam).Scan(&id)
	if err == nil {
		return id, nil
	}
	if idemKeyParam != nil && errors.Is(err, pgx.ErrNoRows) {
		// ON CONFLICT DO NOTHING found an existing row and inserted
		// nothing, so RETURNING produced no row — look the existing job up.
		if lookupErr := s.db.QueryRow(ctx, `
			SELECT id FROM async_jobs WHERE customer_id = $1 AND idempotency_key = $2
		`, customerID, idempotencyKey).Scan(&id); lookupErr != nil {
			return uuid.Nil, fmt.Errorf("jobs: enqueue: lookup existing idempotent job: %w", lookupErr)
		}
		return id, nil
	}
	return uuid.Nil, fmt.Errorf("jobs: enqueue: %w", err)
}

// Get returns the job scoped to customerID. Scoping is enforced in the SQL
// itself (AND customer_id = $2), not a post-fetch ownership check — mirrors
// webhookout's DeleteEndpoint/UpdateEndpointSubscription IDOR-safe pattern.
// ok is false both when the job doesn't exist and when it belongs to a
// different customer; the two cases are indistinguishable to the caller by
// design (a 404, not a 403, avoids leaking job-id existence across customers).
func (s *Store) Get(ctx context.Context, id, customerID uuid.UUID) (Job, bool, error) {
	if s == nil {
		return Job{}, false, nil
	}
	var j Job
	err := s.db.QueryRow(ctx, `
		SELECT id, customer_id, api_key_id, operation, request_id, plan, payload,
		       status, result, units_label, billable_units, error_code, error_message,
		       timeout_seconds, created_at, updated_at
		FROM async_jobs
		WHERE id = $1 AND customer_id = $2
	`, id, customerID).Scan(
		&j.ID, &j.CustomerID, &j.APIKeyID, &j.Operation, &j.RequestID, &j.Plan, &j.Payload,
		&j.Status, &j.Result, &j.UnitsLabel, &j.BillableUnits, &j.ErrorCode, &j.ErrorMessage,
		&j.TimeoutSeconds, &j.CreatedAt, &j.UpdatedAt,
	)
	if err != nil {
		if err == pgx.ErrNoRows {
			return Job{}, false, nil
		}
		return Job{}, false, fmt.Errorf("jobs: get: %w", err)
	}
	return j, true, nil
}

// List returns a page of customerID's own jobs — SQL-scoped by customer_id,
// mirroring Get's IDOR-safe pattern — newest-first, optionally narrowed by an
// exact status and/or operation match, plus the total matching row count
// across all pages. status/operation nil means "no filter" for that field.
// Served via idx_async_jobs_customer (customer_id, created_at DESC),
// migration 0019's index reserved for exactly this endpoint.
//
// Deliberately omits the payload/result JSONB columns from the SELECT (the
// returned Job.Payload/Job.Result are always nil): jobsListHandler's
// jobListItem never reads them, and a page of up to 100 rows would otherwise
// force Postgres to read — and pgx to allocate — however large those blobs
// happen to be, on every list call. Get still selects them since the
// single-job response is exactly where a caller wants that payload.
func (s *Store) List(ctx context.Context, customerID uuid.UUID, status, operation *string, limit, offset int) ([]Job, int64, error) {
	if s == nil {
		return nil, 0, nil
	}

	var total int64
	if err := s.db.QueryRow(ctx, `
		SELECT COUNT(*)
		FROM async_jobs
		WHERE customer_id = $1
		  AND ($2::text IS NULL OR status = $2)
		  AND ($3::text IS NULL OR operation = $3)
	`, customerID, status, operation).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("jobs: list count: %w", err)
	}

	rows, err := s.db.Query(ctx, `
		SELECT id, customer_id, api_key_id, operation, request_id, plan,
		       status, units_label, billable_units, error_code, error_message,
		       timeout_seconds, created_at, updated_at
		FROM async_jobs
		WHERE customer_id = $1
		  AND ($2::text IS NULL OR status = $2)
		  AND ($3::text IS NULL OR operation = $3)
		ORDER BY created_at DESC, id DESC
		LIMIT $4 OFFSET $5
	`, customerID, status, operation, limit, offset)
	if err != nil {
		return nil, 0, fmt.Errorf("jobs: list query: %w", err)
	}
	defer rows.Close()

	jobs := []Job{}
	for rows.Next() {
		var j Job
		if err := rows.Scan(
			&j.ID, &j.CustomerID, &j.APIKeyID, &j.Operation, &j.RequestID, &j.Plan,
			&j.Status, &j.UnitsLabel, &j.BillableUnits, &j.ErrorCode, &j.ErrorMessage,
			&j.TimeoutSeconds, &j.CreatedAt, &j.UpdatedAt,
		); err != nil {
			return nil, 0, fmt.Errorf("jobs: list scan: %w", err)
		}
		jobs = append(jobs, j)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("jobs: list rows: %w", err)
	}
	return jobs, total, nil
}

// TraceparentsByID returns the captured W3C traceparent (see
// tracing.CaptureTraceparent) for each of ids that has one recorded, keyed
// by job id. A deliberate separate query rather than a Job struct field:
// Job (jobs.go) is shared with every Store/Executor caller across the
// codebase, and Claim's SELECT/scan is the fairness-critical claim path this
// module must not touch (see Claim's doc comment) — so Executor.claimAndDispatch
// calls this once per claimed batch instead. ids with no captured
// traceparent (enqueued before this column existed, tracing disabled, or
// enqueued outside any traced request) are simply absent from the returned
// map; callers treat a missing entry the same as an empty string, which
// tracing.RestoreTraceparent already no-ops on.
func (s *Store) TraceparentsByID(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]string, error) {
	if s == nil || len(ids) == 0 {
		return nil, nil
	}
	rows, err := s.db.Query(ctx, `
		SELECT id, traceparent FROM async_jobs WHERE id = ANY($1) AND traceparent IS NOT NULL
	`, ids)
	if err != nil {
		return nil, fmt.Errorf("jobs: traceparents: %w", err)
	}
	defer rows.Close()

	out := make(map[uuid.UUID]string, len(ids))
	for rows.Next() {
		var (
			id uuid.UUID
			tv string
		)
		if err := rows.Scan(&id, &tv); err != nil {
			return nil, fmt.Errorf("jobs: traceparents scan: %w", err)
		}
		out[id] = tv
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("jobs: traceparents rows: %w", err)
	}
	return out, nil
}

// CountActive returns the number of queued+running async_jobs rows currently
// owned by customerID — the backlog enqueueAsync (server/routes.go) compares
// against JobMaxQueuedPerCustomer before admitting a new enqueue. This is a
// plain read-then-compare, not enforced atomically with the following
// INSERT: under concurrent enqueues from the same customer right at the
// ceiling, a small number of requests can land slightly over it before the
// count catches up. That's an accepted, bounded tradeoff (the same shape as
// the framework's Redis-backed rate limit/quota checks) in exchange for
// keeping Enqueue's SQL simple; the ceiling is a fairness backstop, not a
// hard billing boundary.
func (s *Store) CountActive(ctx context.Context, customerID uuid.UUID) (int64, error) {
	if s == nil {
		return 0, nil
	}
	var n int64
	if err := s.db.QueryRow(ctx, `
		SELECT COUNT(*) FROM async_jobs WHERE customer_id = $1 AND status IN ('queued', 'running')
	`, customerID).Scan(&n); err != nil {
		return 0, fmt.Errorf("jobs: count active: %w", err)
	}
	return n, nil
}

// QueueDepth returns the current total number of 'queued' async_jobs rows
// across all customers — the crucible_jobs_queue_depth gauge's source of
// truth, refreshed once per Executor poll tick (see Executor.claimAndDispatch).
// Label-free and global by design: a per-customer breakdown would give
// customer_id unbounded cardinality in Prometheus, which the framework's
// other metrics (see observability.Middleware's RoutePattern-only path label)
// deliberately avoid.
func (s *Store) QueueDepth(ctx context.Context) (int64, error) {
	if s == nil {
		return 0, nil
	}
	var n int64
	if err := s.db.QueryRow(ctx, `
		SELECT COUNT(*) FROM async_jobs WHERE status = 'queued'
	`).Scan(&n); err != nil {
		return 0, fmt.Errorf("jobs: queue depth: %w", err)
	}
	return n, nil
}
