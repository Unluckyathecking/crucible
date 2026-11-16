// Terminal and retry state transitions for the async job store.

package jobs

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/rs/zerolog/log"

	"github.com/Unluckyathecking/crucible/gateway/internal/events"
)

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
