package jobs

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// stuckJobAge is the threshold past which a 'running' row is considered
// abandoned by a process that crashed without a graceful shutdown, and is
// reset to 'queued' by the crash-recovery sweep in Claim. Mirrors
// webhookout.stuckDeliveryAge. Must comfortably exceed a realistic job
// duration plus poll interval.
const stuckJobAge = 2 * time.Minute

// Store is the durable Postgres-backed async job queue.
type Store struct {
	db *pgxpool.Pool
}

// NewStore returns nil when db is nil, matching the optional-Deps nil-safe
// pattern used by webhookout.NewEmitter — every exported method nil-checks
// its receiver, so callers need not nil-check the dependency first.
func NewStore(db *pgxpool.Pool) *Store {
	if db == nil {
		return nil
	}
	return &Store{db: db}
}

// Enqueue inserts a queued job row and returns its generated id.
// timeoutSeconds is the per-route override from routes_table.go's
// AsyncRoutes; <= 0 means "use the executor's configured default".
func (s *Store) Enqueue(ctx context.Context, customerID, apiKeyID uuid.UUID, operation, requestID, plan string, payload json.RawMessage, timeoutSeconds int) (uuid.UUID, error) {
	if s == nil {
		return uuid.Nil, fmt.Errorf("jobs: store is nil")
	}
	if timeoutSeconds < 0 {
		timeoutSeconds = 0
	}
	var id uuid.UUID
	err := s.db.QueryRow(ctx, `
		INSERT INTO async_jobs (customer_id, api_key_id, operation, request_id, plan, payload, timeout_seconds, status)
		VALUES ($1, $2, $3, $4, $5, $6, $7, 'queued')
		RETURNING id
	`, customerID, apiKeyID, operation, requestID, plan, []byte(payload), timeoutSeconds).Scan(&id)
	if err != nil {
		return uuid.Nil, fmt.Errorf("jobs: enqueue: %w", err)
	}
	return id, nil
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
func (s *Store) Claim(ctx context.Context, limit int, instanceID uuid.UUID) ([]Job, error) {
	if s == nil || limit <= 0 {
		return nil, nil
	}

	// Crash-recovery sweep runs outside any transaction, same as
	// webhookout.processDue's stuck-delivery reset.
	if _, err := s.db.Exec(ctx, `
		UPDATE async_jobs
		SET status = 'queued', claimed_at = NULL, claimed_by = NULL, updated_at = NOW()
		WHERE status = 'running'
		  AND claimed_at IS NOT NULL
		  AND claimed_at < NOW() - ($1 * INTERVAL '1 second')
	`, stuckJobAge.Seconds()); err != nil {
		return nil, fmt.Errorf("jobs: stuck-job sweep: %w", err)
	}

	tx, err := s.db.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("jobs: begin claim tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	rows, err := tx.Query(ctx, `
		SELECT id, customer_id, api_key_id, operation, request_id, plan, payload, timeout_seconds
		FROM async_jobs
		WHERE status = 'queued'
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
		if err := rows.Scan(&j.ID, &j.CustomerID, &j.APIKeyID, &j.Operation, &j.RequestID, &j.Plan, &j.Payload, &j.TimeoutSeconds); err != nil {
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

// Requeue returns a claimed job to 'queued' without recording an error.
// Used when a job's worker invocation is interrupted by a graceful
// shutdown (context cancellation), not a genuine worker failure — the job
// is retried instead of permanently failing the customer's request.
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
// 'queued'. Called once by Executor.Run after its worker pool has drained,
// as a final safety net for graceful shutdown — no lost work. Scoped to
// instanceID so a multi-replica deployment never touches another gateway
// process's in-flight jobs. Returns the number of rows released.
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
