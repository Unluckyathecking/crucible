// Fair-claim path for the async job store.

package jobs

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Unluckyathecking/crucible/gateway/internal/observability"
)

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
