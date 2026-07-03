// Package selfusage implements the customer-scoped, read-only self-service usage
// endpoint (GET /v1/usage): the caller's current billing-period consumption, plan
// quota cap, remaining balance, period window, and per-operation breakdown — the
// same signals quota.Middleware enforces against, exposed to the API consumer
// that owns them.
//
// Read-only: no method here ever mutates billing/quota state. The customer is
// always the authenticated caller (auth.FromContext); there is no customer_id
// parameter, so cross-customer lookup (IDOR) is structurally impossible.
package selfusage

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// OperationUsage is one entry in the per-operation usage breakdown.
type OperationUsage struct {
	Operation  string `json:"operation"`
	TotalUnits int64  `json:"total_units"`
	Calls      int64  `json:"calls"`
}

// Store queries the per-operation usage_events breakdown for the self-service
// usage endpoint.
type Store struct {
	db *pgxpool.Pool
}

// NewStore returns a Store backed by db. db may be nil (Deps.DB unset in this
// clone); Breakdown then returns a zeroed result instead of querying, mirroring
// idempotency.NewStore's nil-DB pass-through — the endpoint degrades gracefully
// (used/remaining/cap still populate from quota.Tracker/billing.PlanCache) rather
// than 500ing outright.
func NewStore(db *pgxpool.Pool) *Store {
	return &Store{db: db}
}

// Breakdown returns customerID's usage_events aggregated by operation within
// [start, end), ordered by total_units descending, plus the grand totals across
// all operations. Mirrors operator.Store.CustomerUsage's query so the two
// surfaces (operator admin view, customer self-service view) never diverge.
func (s *Store) Breakdown(ctx context.Context, customerID uuid.UUID, start, end time.Time) ([]OperationUsage, int64, int64, error) {
	if s == nil || s.db == nil {
		return []OperationUsage{}, 0, 0, nil
	}

	rows, err := s.db.Query(ctx, `
		SELECT operation, SUM(billable_units) AS total_units, COUNT(*) AS calls
		FROM usage_events
		WHERE customer_id = $1
		  AND created_at >= $2
		  AND created_at < $3
		GROUP BY operation
		ORDER BY total_units DESC
	`, customerID, start, end)
	if err != nil {
		return nil, 0, 0, err
	}
	defer rows.Close()

	var (
		items      []OperationUsage
		totalUnits int64
		totalCalls int64
	)
	for rows.Next() {
		var op OperationUsage
		if err := rows.Scan(&op.Operation, &op.TotalUnits, &op.Calls); err != nil {
			return nil, 0, 0, err
		}
		items = append(items, op)
		totalUnits += op.TotalUnits
		totalCalls += op.Calls
	}
	if err := rows.Err(); err != nil {
		return nil, 0, 0, err
	}
	if items == nil {
		items = []OperationUsage{}
	}
	return items, totalUnits, totalCalls, nil
}

// CurrentBillingPeriod returns [start, end) for the current UTC calendar month.
func CurrentBillingPeriod() (start, end time.Time) {
	now := time.Now().UTC()
	start = time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	end = start.AddDate(0, 1, 0)
	return start, end
}
