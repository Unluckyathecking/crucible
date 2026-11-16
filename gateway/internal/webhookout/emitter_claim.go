// Fair-claim path for the webhook emitter.

package webhookout

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Unluckyathecking/crucible/gateway/internal/observability"
)

// webhookFairClaimAdvisoryLockKey is an arbitrary fixed key for the
// session-scoped Postgres advisory lock claimDue takes for the whole gateway
// fleet whenever maxInflightPerCustomer > 0 — see its use in claimDue for why
// the per-customer cap needs one. Deliberately distinct from
// jobs.fairClaimAdvisoryLockKey (0x63727563_69626c65, "crucible" in hex) so
// the two independent fair-claim paths can never contend on the same
// advisory lock key.
const webhookFairClaimAdvisoryLockKey int64 = 0x77656268_6f6f6b73 // "webhooks" in hex

// claimDue selects due rows to deliver within tx via SELECT … FOR UPDATE
// SKIP LOCKED — concurrent worker instances (multi-replica deployments) skip
// locked rows rather than blocking. The subscribed_events check is
// re-evaluated here (not just at Emit-time) so a customer narrowing an
// endpoint's subscription after a row was already queued stops that row
// from being delivered too, instead of only affecting events emitted
// afterward.
//
// e.maxInflightPerCustomer <= 0 (the zero-value default) runs the original
// single-query global-FIFO SELECT unchanged — every prior release's exact
// behaviour. A positive value switches to a fairness-aware path mirroring
// jobs.Store.Claim: it over-fetches a bounded candidate window (see
// webhookFairClaimOverfetchFactor/webhookFairClaimMaxCandidates), then in Go
// skips any candidate whose customer already has maxInflightPerCustomer rows
// 'delivering' — counted fresh via deliveringCountsByCustomer, plus any
// already selected this same cycle — so a customer with a deep backlog can
// never starve another customer's delivery out of every claim tick. Rows
// skipped this way stay 'pending' and are simply reconsidered next tick;
// nothing is lost.
func (e *Emitter) claimDue(ctx context.Context, tx pgx.Tx) ([]pendingRow, error) {
	if e.maxInflightPerCustomer <= 0 {
		rows, err := tx.Query(ctx, `
			SELECT d.id, d.event_id, d.event_type, d.payload, d.attempts, we.url, we.secret, we.id, d.traceparent
			FROM webhook_deliveries d
			JOIN webhook_endpoints we ON we.id = d.endpoint_id
			WHERE d.status = 'pending'
			  AND d.next_attempt_at <= NOW()
			  AND we.active = TRUE
			  AND (we.subscribed_events IS NULL OR d.event_type = ANY(we.subscribed_events))
			ORDER BY d.next_attempt_at ASC
			LIMIT $1
			FOR UPDATE OF d SKIP LOCKED
		`, claimPageSize)
		if err != nil {
			return nil, fmt.Errorf("webhookout: claim: %w", err)
		}
		defer rows.Close()

		var due []pendingRow
		for rows.Next() {
			var r pendingRow
			var tp *string
			if err := rows.Scan(&r.id, &r.eventID, &r.eventType, &r.payload, &r.attempts, &r.url, &r.secret, &r.endpointID, &tp); err != nil {
				return nil, fmt.Errorf("webhookout: scan: %w", err)
			}
			if tp != nil {
				r.traceparent = *tp
			}
			due = append(due, r)
		}
		if err := rows.Err(); err != nil {
			return nil, fmt.Errorf("webhookout: rows: %w", err)
		}
		return due, nil
	}

	// deliveringCountsByCustomer's read and this transaction's later
	// mark-delivering write are a check-then-act sequence: two concurrent
	// gateway replicas both fair-claiming at once would each read the SAME
	// pre-commit delivering count for a customer at the cap and could
	// jointly push it past maxInflightPerCustomer, since read-committed
	// isolation doesn't let either see the other's uncommitted claims. A
	// session-scoped advisory lock (auto-released at commit/rollback, never
	// needs an explicit unlock) serializes exactly the fairness-enabled
	// claim path across every replica so only one instance is ever
	// mid-decision at a time. The plain FOR UPDATE SKIP LOCKED path above
	// (maxInflightPerCustomer <= 0, still the default) is untouched and
	// stays fully concurrent — this lock is the price of the per-customer
	// cap's correctness, not of claiming in general.
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, webhookFairClaimAdvisoryLockKey); err != nil {
		return nil, fmt.Errorf("webhookout: fair claim lock: %w", err)
	}

	candidateLimit := claimPageSize * webhookFairClaimOverfetchFactor
	if candidateLimit > webhookFairClaimMaxCandidates {
		candidateLimit = webhookFairClaimMaxCandidates
	}

	rows, err := tx.Query(ctx, `
		SELECT d.id, d.event_id, d.event_type, d.payload, d.attempts, we.url, we.secret, we.customer_id, we.id, d.traceparent
		FROM webhook_deliveries d
		JOIN webhook_endpoints we ON we.id = d.endpoint_id
		WHERE d.status = 'pending'
		  AND d.next_attempt_at <= NOW()
		  AND we.active = TRUE
		  AND (we.subscribed_events IS NULL OR d.event_type = ANY(we.subscribed_events))
		ORDER BY d.next_attempt_at ASC
		LIMIT $1
		FOR UPDATE OF d SKIP LOCKED
	`, candidateLimit)
	if err != nil {
		return nil, fmt.Errorf("webhookout: claim: %w", err)
	}

	var candidates []pendingRow
	for rows.Next() {
		var r pendingRow
		var tp *string
		if err := rows.Scan(&r.id, &r.eventID, &r.eventType, &r.payload, &r.attempts, &r.url, &r.secret, &r.customerID, &r.endpointID, &tp); err != nil {
			rows.Close()
			return nil, fmt.Errorf("webhookout: scan: %w", err)
		}
		if tp != nil {
			r.traceparent = *tp
		}
		candidates = append(candidates, r)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("webhookout: rows: %w", err)
	}
	rows.Close()

	if len(candidates) == 0 {
		return nil, nil
	}

	delivering, err := e.deliveringCountsByCustomer(ctx, tx)
	if err != nil {
		return nil, err
	}
	selected, throttled := applyWebhookInflightCap(candidates, delivering, e.maxInflightPerCustomer, claimPageSize)
	if throttled > 0 {
		observability.WebhookDeliveriesThrottledTotal.WithLabelValues("inflight_cap").Add(float64(throttled))
	}
	return selected, nil
}

// deliveringCountsByCustomer returns the number of 'delivering' rows per
// customer, read inside the same transaction as claimDue's candidate SELECT
// so the count reflects a consistent snapshot alongside the rows being
// considered for claim. Only customers with at least one delivering row
// appear in the map; applyWebhookInflightCap treats an absent entry as zero.
func (e *Emitter) deliveringCountsByCustomer(ctx context.Context, tx pgx.Tx) (map[uuid.UUID]int, error) {
	rows, err := tx.Query(ctx, `
		SELECT we.customer_id, COUNT(*)
		FROM webhook_deliveries d
		JOIN webhook_endpoints we ON we.id = d.endpoint_id
		WHERE d.status = 'delivering'
		GROUP BY we.customer_id
	`)
	if err != nil {
		return nil, fmt.Errorf("webhookout: delivering counts: %w", err)
	}
	defer rows.Close()

	counts := make(map[uuid.UUID]int)
	for rows.Next() {
		var (
			customerID uuid.UUID
			n          int
		)
		if err := rows.Scan(&customerID, &n); err != nil {
			return nil, fmt.Errorf("webhookout: delivering counts scan: %w", err)
		}
		counts[customerID] = n
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("webhookout: delivering counts rows: %w", err)
	}
	return counts, nil
}

// applyWebhookInflightCap walks candidates in their original
// oldest-next_attempt_at-first order and selects up to limit of them,
// skipping any whose customer already has maxInflightPerCustomer rows
// accounted for — either already 'delivering' (from the delivering
// snapshot) or already selected earlier in this same walk. Skipped
// candidates are simply left out of the returned slice; claimDue leaves
// them 'pending' for a later tick. throttled counts how many candidates
// were skipped for exactly that reason, so claimDue can report it via
// observability.WebhookDeliveriesThrottledTotal. Mirrors jobs.applyInflightCap.
func applyWebhookInflightCap(candidates []pendingRow, delivering map[uuid.UUID]int, maxInflightPerCustomer, limit int) (selected []pendingRow, throttled int) {
	selected = make([]pendingRow, 0, limit)
	inflight := make(map[uuid.UUID]int, len(delivering))
	for k, v := range delivering {
		inflight[k] = v
	}
	for _, r := range candidates {
		if len(selected) >= limit {
			break
		}
		if inflight[r.customerID] >= maxInflightPerCustomer {
			throttled++
			continue
		}
		inflight[r.customerID]++
		selected = append(selected, r)
	}
	return selected, throttled
}
