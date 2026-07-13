// Package selfusagedetail implements the customer-scoped, read-only per-event
// usage export endpoint (GET /v1/usage/events): the caller's own usage_events
// rows — id, operation, billable_units, created_at — newest-first, filtered by
// date range and/or operation, and paginated. The API-key counterpart to
// dashboard/app/api/usage/route.ts (session-auth, direct-Postgres), reachable
// by programmatic customers reconciling against Stripe invoices.
//
// Read-only: no method here ever mutates usage_events. The customer is always
// the authenticated caller (auth.FromContext); there is no customer_id
// parameter, so cross-customer lookup (IDOR) is structurally impossible.
package selfusagedetail

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Event is one usage_events row scoped to a single customer.
type Event struct {
	ID            int64     `json:"id"`
	Operation     string    `json:"operation"`
	BillableUnits int64     `json:"billable_units"`
	CreatedAt     time.Time `json:"created_at"`
}

// Store queries the caller-scoped usage_events rows for the self-service
// per-event usage export endpoint.
type Store struct {
	db *pgxpool.Pool
}

// NewStore returns a Store backed by db. Handler is only ever registered when
// d.DB is non-nil (routes.go's d.DB != nil block), so db is never nil here.
func NewStore(db *pgxpool.Pool) *Store {
	return &Store{db: db}
}

// Query returns customerID's usage_events rows in [from, toExclusive), newest
// first, optionally filtered by exact operation match, served via
// idx_usage_detail (customer_id, created_at, operation). Fetches limit+1 rows
// so the caller can compute has_more without a second COUNT query; the extra
// row is trimmed before returning. Mirrors selferrors.Store.Query's shape.
func (s *Store) Query(
	ctx context.Context,
	customerID uuid.UUID,
	from, toExclusive time.Time,
	operation *string,
	limit, offset int,
) ([]Event, bool, error) {
	rows, err := s.db.Query(ctx, `
		SELECT id, operation, billable_units, created_at
		FROM usage_events
		WHERE customer_id = $1
		  AND created_at >= $2
		  AND created_at < $3
		  AND ($4::text IS NULL OR operation = $4)
		ORDER BY created_at DESC
		LIMIT $5 OFFSET $6
	`, customerID, from, toExclusive, operation, limit+1, offset)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()

	var events []Event
	for rows.Next() {
		var e Event
		if err := rows.Scan(&e.ID, &e.Operation, &e.BillableUnits, &e.CreatedAt); err != nil {
			return nil, false, err
		}
		events = append(events, e)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}

	hasMore := len(events) > limit
	if hasMore {
		events = events[:limit]
	}
	if events == nil {
		events = []Event{}
	}
	return events, hasMore, nil
}
