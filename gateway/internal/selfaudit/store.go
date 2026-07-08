// Crucible Enterprise Edition (EE) file.
// Licensed under the Crucible Enterprise License — see ee/LICENSE.md.
// Not covered by the repository's MIT license.

// Package selfaudit implements the customer-scoped, read-only audit-export
// endpoint (GET /v1/audit): the caller's own audit_log rows — actions the
// customer performed, or actions performed against the customer — newest-first,
// optionally filtered by action, and paginated.
//
// Read-only: no method here mutates audit_log. The customer is always the
// authenticated caller (auth.FromContext); there is no customer_id parameter, so
// cross-customer lookup (IDOR) is structurally impossible. The scoping predicate
// additionally requires actor_type='customer' / target_type='customer' so a
// customer's own id can only ever match rows that are genuinely about them, and
// the admin actor_id behind a target-scoped row is never surfaced.
package selfaudit

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Event is one customer-scoped audit_log row. actor_id is intentionally omitted:
// on a target-scoped row (an admin acting against the customer) it would reveal
// the internal admin identity, which is not the customer's to see.
type Event struct {
	ID         int64           `json:"id"`
	ActorType  string          `json:"actor_type"`
	Action     string          `json:"action"`
	TargetType *string         `json:"target_type,omitempty"`
	TargetID   *string         `json:"target_id,omitempty"`
	Details    json.RawMessage `json:"details,omitempty"`
	CreatedAt  time.Time       `json:"created_at"`
}

// Store queries the caller-scoped audit_log rows for the self-service
// audit-export endpoint.
type Store struct {
	db *pgxpool.Pool
}

// NewStore returns a Store backed by db.
func NewStore(db *pgxpool.Pool) *Store {
	return &Store{db: db}
}

// Query returns customerID's audit_log rows, newest first, optionally filtered
// by exact action match. Scope is strictly the caller: rows where the customer
// is the actor (actor_type='customer' AND actor_id=customerID) OR the target
// (target_type='customer' AND target_id=customerID). Fetches limit+1 rows so the
// caller can compute has_more without a second COUNT query; the extra row is
// trimmed before returning.
func (s *Store) Query(
	ctx context.Context,
	customerID uuid.UUID,
	action *string,
	limit, offset int,
) ([]Event, bool, error) {
	cid := customerID.String()
	rows, err := s.db.Query(ctx, `
		SELECT id, actor_type, action, target_type, target_id, details, created_at
		FROM audit_log
		WHERE (
		      (actor_type  = 'customer' AND actor_id  = $1)
		   OR (target_type = 'customer' AND target_id = $1)
		)
		  AND ($2::text IS NULL OR action = $2)
		ORDER BY created_at DESC, id DESC
		LIMIT $3 OFFSET $4
	`, cid, action, limit+1, offset)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()

	var events []Event
	for rows.Next() {
		var (
			e       Event
			details []byte
		)
		if err := rows.Scan(&e.ID, &e.ActorType, &e.Action,
			&e.TargetType, &e.TargetID, &details, &e.CreatedAt); err != nil {
			return nil, false, err
		}
		e.Details = json.RawMessage(details)
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
