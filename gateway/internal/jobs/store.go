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

	var id uuid.UUID
	err := s.db.QueryRow(ctx, `
		INSERT INTO async_jobs (customer_id, api_key_id, operation, request_id, plan, payload, timeout_seconds, idempotency_key, status)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, 'queued')
		ON CONFLICT (customer_id, idempotency_key) WHERE idempotency_key IS NOT NULL DO NOTHING
		RETURNING id
	`, customerID, apiKeyID, operation, requestID, plan, []byte(payload), timeoutSeconds, idemKeyParam).Scan(&id)
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
func (s *Store) Claim(ctx context.Context, limit int, instanceID uuid.UUID) ([]Job, error) {
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

	rows, err := tx.Query(ctx, `
		SELECT id, customer_id, api_key_id, operation, request_id, plan, payload, timeout_seconds, attempts
		FROM async_jobs
		WHERE status = 'queued' AND next_attempt_at <= NOW()
		ORDER BY created_at ASC
		LIMIT $1
		FOR UPDATE SKIP LOCKED
	`, limit)
	if err != nil {
		return nil, fmt.Errorf("jobs: claim select: %w", err)
	}

	var claimed []Job
	for rows.Next() {
		var j Job
		if err := rows.Scan(&j.ID, &j.CustomerID, &j.APIKeyID, &j.Operation, &j.RequestID, &j.Plan, &j.Payload, &j.TimeoutSeconds, &j.Attempts); err != nil {
			rows.Close()
			return nil, fmt.Errorf("jobs: claim scan: %w", err)
		}
		claimed = append(claimed, j)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("jobs: claim rows: %w", err)
	}
	rows.Close()

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

// Complete marks a claimed job succeeded with its worker result.
func (s *Store) Complete(ctx context.Context, id uuid.UUID, result json.RawMessage, billableUnits uint64, unitsLabel string) error {
	if s == nil {
		return fmt.Errorf("jobs: store is nil")
	}
	_, err := s.db.Exec(ctx, `
		UPDATE async_jobs
		SET status = 'succeeded', result = $2, billable_units = $3, units_label = $4,
		    claimed_at = NULL, claimed_by = NULL, updated_at = NOW()
		WHERE id = $1
	`, id, []byte(result), billableUnits, unitsLabel)
	if err != nil {
		return fmt.Errorf("jobs: complete: %w", err)
	}
	return nil
}

// Fail marks a claimed job permanently failed with a structured error.
func (s *Store) Fail(ctx context.Context, id uuid.UUID, code, message string) error {
	if s == nil {
		return fmt.Errorf("jobs: store is nil")
	}
	_, err := s.db.Exec(ctx, `
		UPDATE async_jobs
		SET status = 'failed', error_code = $2, error_message = $3,
		    claimed_at = NULL, claimed_by = NULL, updated_at = NOW()
		WHERE id = $1
	`, id, code, message)
	if err != nil {
		return fmt.Errorf("jobs: fail: %w", err)
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
// markDeadLetter. Distinct from Fail, which is used for deterministic
// failures (a worker structured business error, or a billable_units<1
// contract violation) that must never be retried and must leave attempts
// unchanged.
func (s *Store) DeadLetter(ctx context.Context, id uuid.UUID, attempts int, code, message string) error {
	if s == nil {
		return fmt.Errorf("jobs: store is nil")
	}
	_, err := s.db.Exec(ctx, `
		UPDATE async_jobs
		SET status = 'failed', attempts = $2, error_code = $3, error_message = $4,
		    claimed_at = NULL, claimed_by = NULL, updated_at = NOW()
		WHERE id = $1
	`, id, attempts, code, message)
	if err != nil {
		return fmt.Errorf("jobs: dead letter: %w", err)
	}
	return nil
}

// Requeue returns a claimed job to 'queued' without recording an error.
// Not called by Executor itself — see Run's doc comment for why a job
// interrupted by shutdown is left 'running' for the crash-recovery sweep to
// reclaim rather than requeued immediately (avoids a second, concurrent
// execution of a job whose worker call may still genuinely be in flight).
// Retained as a general primitive for callers that can positively confirm
// a job is safe to retry immediately (e.g. future operator tooling).
func (s *Store) Requeue(ctx context.Context, id uuid.UUID) error {
	if s == nil {
		return fmt.Errorf("jobs: store is nil")
	}
	_, err := s.db.Exec(ctx, `
		UPDATE async_jobs
		SET status = 'queued', claimed_at = NULL, claimed_by = NULL, updated_at = NOW()
		WHERE id = $1
	`, id)
	if err != nil {
		return fmt.Errorf("jobs: requeue: %w", err)
	}
	return nil
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
