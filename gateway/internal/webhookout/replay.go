// Dead-letter replay: operator-triggered recovery for webhook_deliveries rows
// that exhausted maxAttempts. Replay only ever requeues rows back to
// status='pending' — it never delivers directly. The already-running
// Emitter.processDue worker picks the row up on its next tick and delivers it
// through the existing egress.GuardedTransport, so no new outbound HTTP path
// is introduced here.
package webhookout

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ActionDeliveryReplayed is the stable audit_log action recorded for every
// row a replay call requeues, whether triggered via single or bulk replay.
const ActionDeliveryReplayed = "webhook.delivery.replayed"

// DeadLetterDelivery is the operator-visible projection of a dead_letter
// webhook_deliveries row, joined to its endpoint. Secrets are never selected.
type DeadLetterDelivery struct {
	ID               int64     `json:"id"`
	EventID          string    `json:"event_id"`
	EventType        string    `json:"event_type"`
	EndpointID       uuid.UUID `json:"endpoint_id"`
	EndpointURL      string    `json:"endpoint_url"`
	CustomerID       uuid.UUID `json:"customer_id"`
	Attempts         int       `json:"attempts"`
	LastResponseCode *int      `json:"last_response_code,omitempty"`
	CreatedAt        time.Time `json:"created_at"`
}

// Page is the standard paginated response envelope, mirroring operator.Page[T].
type Page[T any] struct {
	Items []T   `json:"items"`
	Total int64 `json:"total"`
}

// DeadLettersFilter constrains the ListDeadLetters query.
type DeadLettersFilter struct {
	Page    int // 1-based; < 1 treated as 1
	PerPage int // <= 0 or > 100 defaults to 20
}

func (f *DeadLettersFilter) normalize() {
	if f.PerPage <= 0 || f.PerPage > 100 {
		f.PerPage = 20
	}
	if f.Page < 1 {
		f.Page = 1
	}
}

// ListDeadLetters returns a paginated list of dead_letter webhook_deliveries
// rows, most-recent first, joined to their endpoint's url and customer_id.
func ListDeadLetters(ctx context.Context, db *pgxpool.Pool, f DeadLettersFilter) (Page[DeadLetterDelivery], error) {
	f.normalize()

	var total int64
	if err := db.QueryRow(ctx, `
		SELECT COUNT(*) FROM webhook_deliveries WHERE status = 'dead_letter'
	`).Scan(&total); err != nil {
		return Page[DeadLetterDelivery]{}, fmt.Errorf("webhookout: count dead letters: %w", err)
	}

	offset := (f.Page - 1) * f.PerPage
	rows, err := db.Query(ctx, `
		SELECT d.id, d.event_id, d.event_type, d.endpoint_id, we.url, we.customer_id,
		       d.attempts, d.last_response_code, d.created_at
		FROM webhook_deliveries d
		JOIN webhook_endpoints we ON we.id = d.endpoint_id
		WHERE d.status = 'dead_letter'
		ORDER BY d.created_at DESC
		LIMIT $1 OFFSET $2
	`, f.PerPage, offset)
	if err != nil {
		return Page[DeadLetterDelivery]{}, fmt.Errorf("webhookout: list dead letters: %w", err)
	}
	defer rows.Close()

	items, err := pgx.CollectRows(rows, pgx.RowToStructByPos[DeadLetterDelivery])
	if err != nil {
		return Page[DeadLetterDelivery]{}, fmt.Errorf("webhookout: scan dead letters: %w", err)
	}
	if items == nil {
		items = []DeadLetterDelivery{}
	}
	return Page[DeadLetterDelivery]{Items: items, Total: total}, nil
}

// requeueSQL resets a dead_letter row to the same state a freshly-inserted
// pending row would have, so the emitter worker treats it identically to a
// brand-new delivery on its next claim-due tick.
const requeueSQL = `
	UPDATE webhook_deliveries
	SET status = 'pending', attempts = 0, next_attempt_at = NOW(), claimed_at = NULL, last_response_code = NULL
	WHERE status = 'dead_letter'`

// ReplayByID requeues a single dead_letter row back to pending.
// Returns pgx.ErrNoRows when no dead_letter row matches id (already delivered,
// still pending/delivering, or the id does not exist) — callers surface this as 404.
func ReplayByID(ctx context.Context, db *pgxpool.Pool, id int64) error {
	tag, err := db.Exec(ctx, requeueSQL+` AND id = $1`, id)
	if err != nil {
		return fmt.Errorf("webhookout: replay by id: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	return nil
}

// ReplayByEndpoint requeues every dead_letter row belonging to endpointID and
// returns the ids that were requeued (empty slice, not an error, when none matched).
func ReplayByEndpoint(ctx context.Context, db *pgxpool.Pool, endpointID uuid.UUID) ([]int64, error) {
	rows, err := db.Query(ctx, requeueSQL+` AND endpoint_id = $1 RETURNING id`, endpointID)
	if err != nil {
		return nil, fmt.Errorf("webhookout: replay by endpoint: %w", err)
	}
	defer rows.Close()

	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("webhookout: scan requeued id: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("webhookout: requeue rows: %w", err)
	}
	if ids == nil {
		ids = []int64{}
	}
	return ids, nil
}
