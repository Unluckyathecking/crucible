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
	"github.com/rs/zerolog/log"

	"github.com/Unluckyathecking/crucible/gateway/internal/events"
	"github.com/Unluckyathecking/crucible/gateway/internal/observability"
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

// fairClaimOverfetchFactor/fairClaimMaxCandidates bound the candidate window
// Claim scans when maxInflightPerCustomer > 0: wide enough that a customer
// other than the one occupying the head of the FIFO queue is normally found
// within a single claim cycle, but capped so one claim can never lock an
// unbounded slice of the backlog. A row outside this window that's eligible
// only waits one extra poll cycle, not indefinitely — the window is
// reconsidered fresh (from the same oldest-first order) every tick.
const (
	fairClaimOverfetchFactor = 20
	fairClaimMaxCandidates   = 2000
)

// fairClaimAdvisoryLockKey is an arbitrary fixed key for the session-scoped
// Postgres advisory lock Claim takes for the whole gateway fleet whenever
// maxInflightPerCustomer > 0 — see its use in Claim for why the per-customer
// cap needs one. Picked as a distinctive constant unlikely to collide with
// any other advisory lock use in this codebase (there is none today).
const fairClaimAdvisoryLockKey int64 = 0x63727563_69626c65 // "crucible" in hex, truncated to fit int64

// Claim recovers any 'running' rows abandoned by a crashed process (no
// graceful shutdown), then atomically claims up to limit queued rows via
// SELECT ... FOR UPDATE SKIP LOCKED — concurrent gateway replicas skip rows
// locked by another instance's claim rather than blocking. Claimed rows are
// marked 'running' with claimed_by=instanceID and claimed_at=NOW() inside the
// same transaction. Mirrors webhookout.Emitter.processDue.
//
// A row scheduled for retry (next_attempt_at in the future, set by
// RequeueRetry after a transient failure) is skipped until that time
// arrives; the scan otherwise keeps its original oldest-created-first order.
//
// maxInflightPerCustomer <= 0 (the zero-value default) keeps the original
// single-query pure-FIFO path byte-identical to every prior release. A
// positive value switches to a fairness-aware path: it over-fetches a bounded
// window of the oldest queued rows (still via the same FOR UPDATE SKIP LOCKED
// SELECT, so multi-replica claim safety is unchanged — this is still one
// transaction, one claim), then in Go skips any row whose customer already
// has maxInflightPerCustomer jobs 'running' (counted fresh, plus any already
// selected this same cycle) — so a customer with a deep backlog can never
// starve another customer's job out of every claim batch. Rows skipped this
// way stay 'queued' and are simply reconsidered next tick; nothing is lost.
func (s *Store) Claim(ctx context.Context, limit int, instanceID uuid.UUID, maxInflightPerCustomer int) ([]Job, error) {
	if s == nil || limit <= 0 {
		return nil, nil
	}

	// Crash-recovery sweep runs outside any transaction, same as
	// webhookout.processDue's stuck-delivery reset. The threshold is
	// per-row: a job's own timeout_seconds override when set, else
	// DefaultJobTimeout, plus stuckJobGrace slack — never a single fixed
	// constant, which would sweep back to 'queued' (and risk a duplicate
	// claim/double-bill) any job still legitimately running within its own
	// configured budget.
	defaultSeconds := s.DefaultJobTimeout.Seconds()
	if defaultSeconds <= 0 {
		defaultSeconds = defaultStuckJobTimeout.Seconds()
	}
	if _, err := s.db.Exec(ctx, `
		UPDATE async_jobs
		SET status = 'queued', claimed_at = NULL, claimed_by = NULL, updated_at = NOW()
		WHERE status = 'running'
		  AND claimed_at IS NOT NULL
		  AND claimed_at < NOW() - (
		        (CASE WHEN timeout_seconds > 0 THEN timeout_seconds ELSE $1 END) + $2
		      ) * INTERVAL '1 second'
	`, defaultSeconds, stuckJobGrace.Seconds()); err != nil {
		return nil, fmt.Errorf("jobs: stuck-job sweep: %w", err)
	}

	tx, err := s.db.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("jobs: begin claim tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	if maxInflightPerCustomer > 0 {
		// runningCountsByCustomer's read and this transaction's later mark-running
		// write are a check-then-act sequence: two concurrent gateway replicas
		// both fair-claiming at once would each read the SAME pre-commit running
		// count for a customer at the cap and could jointly push it past
		// maxInflightPerCustomer, since read-committed isolation doesn't let
		// either see the other's uncommitted claims. A session-scoped advisory
		// lock (auto-released at commit/rollback, never needs an explicit
		// unlock) serializes exactly the fairness-enabled claim path across
		// every replica so only one instance is ever mid-decision at a time.
		// The plain FOR UPDATE SKIP LOCKED path (maxInflightPerCustomer <= 0,
		// still the default) is untouched and stays fully concurrent — this
		// lock is the price of the per-customer cap's correctness, not of
		// claiming in general.
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, fairClaimAdvisoryLockKey); err != nil {
			return nil, fmt.Errorf("jobs: fair claim lock: %w", err)
		}
	}

	// maxInflightPerCustomer <= 0 selects exactly limit rows, same as every
	// prior release. > 0 over-fetches a bounded candidate window (see
	// fairClaimOverfetchFactor/fairClaimMaxCandidates) so the in-Go filter
	// below has room to skip rows belonging to an already-at-cap customer
	// without starving the batch down to fewer than limit claims just
	// because the head of the FIFO queue happens to belong to one customer.
	candidateLimit := limit
	if maxInflightPerCustomer > 0 {
		candidateLimit = limit * fairClaimOverfetchFactor
		if candidateLimit > fairClaimMaxCandidates {
			candidateLimit = fairClaimMaxCandidates
		}
	}

	rows, err := tx.Query(ctx, `
		SELECT id, customer_id, api_key_id, operation, request_id, plan, payload, timeout_seconds, attempts
		FROM async_jobs
		WHERE status = 'queued' AND next_attempt_at <= NOW()
		ORDER BY created_at ASC
		LIMIT $1
		FOR UPDATE SKIP LOCKED
	`, candidateLimit)
	if err != nil {
		return nil, fmt.Errorf("jobs: claim select: %w", err)
	}

	var candidates []Job
	for rows.Next() {
		var j Job
		if err := rows.Scan(&j.ID, &j.CustomerID, &j.APIKeyID, &j.Operation, &j.RequestID, &j.Plan, &j.Payload, &j.TimeoutSeconds, &j.Attempts); err != nil {
			rows.Close()
			return nil, fmt.Errorf("jobs: claim scan: %w", err)
		}
		candidates = append(candidates, j)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("jobs: claim rows: %w", err)
	}
	rows.Close()

	if len(candidates) == 0 {
		return nil, nil
	}

	claimed := candidates
	if maxInflightPerCustomer > 0 {
		running, err := s.runningCountsByCustomer(ctx, tx)
		if err != nil {
			return nil, err
		}
		var throttled int
		claimed, throttled = applyInflightCap(candidates, running, maxInflightPerCustomer, limit)
		if throttled > 0 {
			observability.JobsCustomerThrottledTotal.WithLabelValues("inflight_cap").Add(float64(throttled))
		}
	}

	if len(claimed) == 0 {
		return nil, nil
	}

	ids := make([]uuid.UUID, len(claimed))
	for i, j := range claimed {
		ids[i] = j.ID
	}
	if _, err := tx.Exec(ctx, `
		UPDATE async_jobs
		SET status = 'running', claimed_at = NOW(), claimed_by = $2, updated_at = NOW()
		WHERE id = ANY($1)
	`, ids, instanceID); err != nil {
		return nil, fmt.Errorf("jobs: mark running: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("jobs: commit claim: %w", err)
	}
	for i := range claimed {
		claimed[i].Status = StatusRunning
	}
	return claimed, nil
}

// runningCountsByCustomer returns the number of 'running' rows per customer,
// read inside the same transaction as Claim's candidate SELECT so the count
// reflects a consistent snapshot alongside the rows being considered for
// claim. Only customers with at least one running row appear in the map;
// applyInflightCap treats an absent entry as zero.
func (s *Store) runningCountsByCustomer(ctx context.Context, tx pgx.Tx) (map[uuid.UUID]int, error) {
	rows, err := tx.Query(ctx, `
		SELECT customer_id, COUNT(*) FROM async_jobs WHERE status = 'running' GROUP BY customer_id
	`)
	if err != nil {
		return nil, fmt.Errorf("jobs: running counts: %w", err)
	}
	defer rows.Close()

	counts := make(map[uuid.UUID]int)
	for rows.Next() {
		var (
			customerID uuid.UUID
			n          int
		)
		if err := rows.Scan(&customerID, &n); err != nil {
			return nil, fmt.Errorf("jobs: running counts scan: %w", err)
		}
		counts[customerID] = n
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("jobs: running counts rows: %w", err)
	}
	return counts, nil
}

// applyInflightCap walks candidates in their original oldest-first order and
// selects up to limit of them, skipping any whose customer already has
// maxInflightPerCustomer jobs accounted for — either already 'running'
// (from the running snapshot) or already selected earlier in this same
// walk. Skipped candidates are simply left out of the returned slice; Claim
// leaves them 'queued' for a later cycle. throttled counts how many
// candidates were skipped for exactly that reason, so Claim can report it via
// observability.JobsCustomerThrottledTotal.
func applyInflightCap(candidates []Job, running map[uuid.UUID]int, maxInflightPerCustomer, limit int) (selected []Job, throttled int) {
	selected = make([]Job, 0, limit)
	inflight := make(map[uuid.UUID]int, len(running))
	for k, v := range running {
		inflight[k] = v
	}
	for _, j := range candidates {
		if len(selected) >= limit {
			break
		}
		if inflight[j.CustomerID] >= maxInflightPerCustomer {
			throttled++
			continue
		}
		inflight[j.CustomerID]++
		selected = append(selected, j)
	}
	return selected, throttled
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

// Complete marks a claimed job succeeded with its worker result and, inside
// the same transaction, enqueues the job.succeeded webhook — so the terminal
// status write and the customer notification commit or roll back as one
// unit, closing the crash window between the old post-complete notify call
// and this write. A nil emitter (SetEmitter never called, or called with
// nil) makes the enqueue a no-op, same as every other optional-Deps emit
// call site in this codebase.
func (s *Store) Complete(ctx context.Context, id uuid.UUID, result json.RawMessage, billableUnits uint64, unitsLabel string) error {
	if s == nil {
		return fmt.Errorf("jobs: store is nil")
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("jobs: complete: begin tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	var customerID uuid.UUID
	var operation string
	err = tx.QueryRow(ctx, `
		UPDATE async_jobs
		SET status = 'succeeded', result = $2, billable_units = $3, units_label = $4,
		    claimed_at = NULL, claimed_by = NULL, updated_at = NOW()
		WHERE id = $1
		RETURNING customer_id, operation
	`, id, []byte(result), billableUnits, unitsLabel).Scan(&customerID, &operation)
	if err != nil {
		return fmt.Errorf("jobs: complete: %w", err)
	}

	if s.emitter != nil {
		payload, merr := json.Marshal(events.JobSucceededPayload{
			JobID: id.String(), Operation: operation, Status: StatusSucceeded,
		})
		if merr != nil {
			log.Warn().Err(merr).Str("job_id", id.String()).Msg("webhook emit: job.succeeded payload marshal failed")
		} else if err := s.emitter.EmitTx(ctx, tx, customerID, events.JobSucceeded, payload); err != nil {
			return fmt.Errorf("jobs: complete: emit tx: %w", err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("jobs: complete: commit: %w", err)
	}
	return nil
}

// Fail marks a claimed job permanently failed with a structured error and,
// inside the same transaction, enqueues the job.failed webhook. See
// Complete's doc comment for why this closes the crash window the old
// post-fail notify call left open.
func (s *Store) Fail(ctx context.Context, id uuid.UUID, code, message string) error {
	if s == nil {
		return fmt.Errorf("jobs: store is nil")
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("jobs: fail: begin tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	var customerID uuid.UUID
	var operation string
	err = tx.QueryRow(ctx, `
		UPDATE async_jobs
		SET status = 'failed', error_code = $2, error_message = $3,
		    claimed_at = NULL, claimed_by = NULL, updated_at = NOW()
		WHERE id = $1
		RETURNING customer_id, operation
	`, id, code, message).Scan(&customerID, &operation)
	if err != nil {
		return fmt.Errorf("jobs: fail: %w", err)
	}

	if s.emitter != nil {
		payload, merr := json.Marshal(events.JobFailedPayload{
			JobID: id.String(), Operation: operation, Status: StatusFailed, ErrorCode: code,
		})
		if merr != nil {
			log.Warn().Err(merr).Str("job_id", id.String()).Msg("webhook emit: job.failed payload marshal failed")
		} else if err := s.emitter.EmitTx(ctx, tx, customerID, events.JobFailed, payload); err != nil {
			return fmt.Errorf("jobs: fail: emit tx: %w", err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("jobs: fail: commit: %w", err)
	}
	return nil
}

// RequeueRetry returns a claimed job to 'queued' after a retryable
// (WORKER_UNREACHABLE / transport) failure, recording the new attempt count
// and the earliest time it may be claimed again — bounded exponential
// backoff, computed by the caller (jobs.Executor). Unlike Requeue (an
// immediate, error-free return to the queue), this is the retry path
// Executor.process calls between a transient failure and dead-lettering via
// DeadLetter once attempts is exhausted.
func (s *Store) RequeueRetry(ctx context.Context, id uuid.UUID, attempts int, nextAttemptAt time.Time) error {
	if s == nil {
		return fmt.Errorf("jobs: store is nil")
	}
	_, err := s.db.Exec(ctx, `
		UPDATE async_jobs
		SET status = 'queued', attempts = $2, next_attempt_at = $3,
		    claimed_at = NULL, claimed_by = NULL, updated_at = NOW()
		WHERE id = $1
	`, id, attempts, nextAttemptAt)
	if err != nil {
		return fmt.Errorf("jobs: requeue retry: %w", err)
	}
	return nil
}

// DeadLetter marks a claimed job permanently failed after its retry budget
// (ExecutorConfig.MaxAttempts) is exhausted, recording the final attempt
// count alongside the structured error — mirrors webhookout's
// markDeadLetter — and, inside the same transaction, enqueues the
// job.failed webhook (see Complete's doc comment for why). Distinct from
// Fail, which is used for deterministic failures (a worker structured
// business error, or a billable_units<1 contract violation) that must never
// be retried and must leave attempts unchanged.
func (s *Store) DeadLetter(ctx context.Context, id uuid.UUID, attempts int, code, message string) error {
	if s == nil {
		return fmt.Errorf("jobs: store is nil")
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("jobs: dead letter: begin tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	var customerID uuid.UUID
	var operation string
	err = tx.QueryRow(ctx, `
		UPDATE async_jobs
		SET status = 'failed', attempts = $2, error_code = $3, error_message = $4,
		    claimed_at = NULL, claimed_by = NULL, updated_at = NOW()
		WHERE id = $1
		RETURNING customer_id, operation
	`, id, attempts, code, message).Scan(&customerID, &operation)
	if err != nil {
		return fmt.Errorf("jobs: dead letter: %w", err)
	}

	if s.emitter != nil {
		payload, merr := json.Marshal(events.JobFailedPayload{
			JobID: id.String(), Operation: operation, Status: StatusFailed, ErrorCode: code,
		})
		if merr != nil {
			log.Warn().Err(merr).Str("job_id", id.String()).Msg("webhook emit: job.failed payload marshal failed")
		} else if err := s.emitter.EmitTx(ctx, tx, customerID, events.JobFailed, payload); err != nil {
			return fmt.Errorf("jobs: dead letter: emit tx: %w", err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("jobs: dead letter: commit: %w", err)
	}
	return nil
}

// CancelQueued transitions a job owned by customerID from 'queued' to the
// terminal 'cancelled' state, scoped and guarded entirely in the SQL's WHERE
// clause: AND customer_id = $2 (IDOR-safe, mirroring Get's SQL-level
// scoping) AND status = 'queued' (mirrors the operator requeue handler's
// running/succeeded guard in jobs_handlers.go, inverted — cancellation is
// only ever valid from 'queued', never 'running', which this cycle has no
// cooperative-cancel path for, nor any already-terminal status). ok is
// false, with no error, whenever the UPDATE affects zero rows — the id
// doesn't exist, belongs to another customer, or is not currently queued;
// the caller (jobsCancelHandler) distinguishes those cases with a follow-up
// Get for the 404-vs-409 response.
func (s *Store) CancelQueued(ctx context.Context, id, customerID uuid.UUID) (bool, error) {
	if s == nil {
		return false, fmt.Errorf("jobs: store is nil")
	}
	tag, err := s.db.Exec(ctx, `
		UPDATE async_jobs
		SET status = 'cancelled', updated_at = NOW()
		WHERE id = $1 AND customer_id = $2 AND status = 'queued'
	`, id, customerID)
	if err != nil {
		return false, fmt.Errorf("jobs: cancel queued: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// Requeue returns a job to 'queued' via a single status-guarded UPDATE,
// mirroring CancelQueued's queued-only guard inverted: a running, succeeded, or
// cancelled row is never touched. The guard lives in the WHERE clause so there
// is no window between reading the status and writing it — an operator requeue
// can never yank a job that a poller claimed a moment earlier back to 'queued'
// and trigger a second, concurrent execution (and second billing) of work
// already in flight. Returns whether a row was actually requeued; false with no
// error means the id is unknown or the job is in one of those non-requeuable
// states, which the operator requeue handler maps to 404-vs-409 with a
// follow-up read (see AdminRequeueJobHandler).
func (s *Store) Requeue(ctx context.Context, id uuid.UUID) (bool, error) {
	if s == nil {
		return false, fmt.Errorf("jobs: store is nil")
	}
	tag, err := s.db.Exec(ctx, `
		UPDATE async_jobs
		SET status = 'queued', claimed_at = NULL, claimed_by = NULL, updated_at = NOW()
		WHERE id = $1 AND status NOT IN ('running', 'succeeded', 'cancelled')
	`, id)
	if err != nil {
		return false, fmt.Errorf("jobs: requeue: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// ReleaseClaimed returns every 'running' job still claimed by instanceID to
// 'queued'. Not called by Executor.Run itself — see its doc comment for why
// an eager release on graceful shutdown risks a second, concurrent
// execution of a job whose worker call may still genuinely be running.
// Retained as an operator-facing primitive (e.g. a manual "force-release
// jobs claimed by a known-dead instance" action) for cases where an
// operator can positively confirm the claiming process is gone and it's
// safe to skip waiting out the normal crash-recovery sweep. Scoped to
// instanceID so it can never touch another gateway process's in-flight
// jobs. Returns the number of rows released.
func (s *Store) ReleaseClaimed(ctx context.Context, instanceID uuid.UUID) (int64, error) {
	if s == nil {
		return 0, nil
	}
	tag, err := s.db.Exec(ctx, `
		UPDATE async_jobs
		SET status = 'queued', claimed_at = NULL, claimed_by = NULL, updated_at = NOW()
		WHERE status = 'running' AND claimed_by = $1
	`, instanceID)
	if err != nil {
		return 0, fmt.Errorf("jobs: release claimed: %w", err)
	}
	return tag.RowsAffected(), nil
}
