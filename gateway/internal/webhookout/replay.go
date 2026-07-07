// Dead-letter replay: operator-triggered recovery for webhook_deliveries rows
// that exhausted maxAttempts. Replay only ever requeues rows back to
// status='pending' — it never delivers directly. The already-running
// Emitter.processDue worker picks the row up on its next tick and delivers it
// through the existing egress.GuardedTransport, so no new outbound HTTP path
// is introduced here.
package webhookout

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Unluckyathecking/crucible/gateway/internal/paging"
)

// ActionDeliveryReplayed is the stable audit_log action recorded for every
// row a replay call requeues, whether triggered via single or bulk replay.
const ActionDeliveryReplayed = "webhook.delivery.replayed"

// ErrEndpointInactive is returned by ReplayByID when the dead_letter row exists
// but its endpoint has been deactivated (webhook_endpoints.active = FALSE).
// Requeuing such a row to 'pending' would strand it: Emitter.processDue's claim
// query only ever selects rows joined to an active endpoint (we.active = TRUE),
// so the row would never be picked up and would silently vanish from the
// dead-letter list without ever being delivered.
var ErrEndpointInactive = errors.New("webhookout: endpoint is inactive")

// DeadLetterDelivery is the operator-visible projection of a dead_letter
// webhook_deliveries row, joined to its endpoint. Secrets are never selected.
// ID is encoded as a JSON string (not a number) so operator clients running in
// JavaScript/TypeScript — where numbers are IEEE-754 doubles — never silently
// round a BIGSERIAL id that has grown past Number.MAX_SAFE_INTEGER before
// echoing it back in the replay URL; mirrors the existing customer delivery
// log's `d.id::text` cast in routes.go's webhookDeliveriesHandler.
type DeadLetterDelivery struct {
	ID               int64     `json:"id,string"`
	EventID          string    `json:"event_id"`
	EventType        string    `json:"event_type"`
	EndpointID       uuid.UUID `json:"endpoint_id"`
	EndpointURL      string    `json:"endpoint_url"`
	EndpointActive   bool      `json:"endpoint_active"`
	CustomerID       uuid.UUID `json:"customer_id"`
	Attempts         int       `json:"attempts"`
	LastResponseCode *int      `json:"last_response_code,omitempty"`
	CreatedAt        time.Time `json:"created_at"`
}

// Page is the standard paginated response envelope, aliased to the shared
// paging.Page[T] so every /v1/admin list endpoint speaks the same wire
// format without duplicating the type.
type Page[T any] = paging.Page[T]

// DeadLettersFilter constrains the ListDeadLetters query.
type DeadLettersFilter struct {
	Page    int // 1-based; < 1 treated as 1
	PerPage int // <= 0 or > 100 defaults to 20
}

func (f *DeadLettersFilter) normalize() {
	f.Page, f.PerPage = paging.Clamp(f.Page, f.PerPage, 20, 100)
}

// ListDeadLetters returns a paginated list of dead_letter webhook_deliveries
// rows, most-recent first, joined to their endpoint's url and customer_id.
// Returns paging.ErrPageTooLarge if f.Page/f.PerPage would push the OFFSET
// past paging.MaxOffset.
func ListDeadLetters(ctx context.Context, db *pgxpool.Pool, f DeadLettersFilter) (Page[DeadLetterDelivery], error) {
	f.normalize()
	offset, err := paging.Offset(f.Page, f.PerPage)
	if err != nil {
		return Page[DeadLetterDelivery]{}, err
	}

	var total int64
	if err := db.QueryRow(ctx, `
		SELECT COUNT(*) FROM webhook_deliveries WHERE status = 'dead_letter'
	`).Scan(&total); err != nil {
		return Page[DeadLetterDelivery]{}, fmt.Errorf("webhookout: count dead letters: %w", err)
	}

	rows, err := db.Query(ctx, `
		SELECT d.id, d.event_id, d.event_type, d.endpoint_id, we.url, we.active, we.customer_id,
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
// brand-new delivery on its next claim-due tick. Joining on we.active = TRUE
// is load-bearing: Emitter.processDue's claim query only selects rows whose
// endpoint is active, so requeuing a row for a deactivated endpoint would
// move it to 'pending' and strand it there forever — it would never be
// claimed, delivered, or dead-lettered again, and would simply disappear
// from the dead-letter list without ever being redelivered.
const requeueSQL = `
	UPDATE webhook_deliveries d
	SET status = 'pending', attempts = 0, next_attempt_at = NOW(), claimed_at = NULL, last_response_code = NULL
	FROM webhook_endpoints we
	WHERE d.endpoint_id = we.id
	  AND we.active = TRUE
	  AND d.status = 'dead_letter'`

// ReplayByID requeues a single dead_letter row back to pending.
// Returns pgx.ErrNoRows when no dead_letter row matches id (already delivered,
// still pending/delivering, or the id does not exist) — callers surface this as 404.
// Returns ErrEndpointInactive when a dead_letter row with this id exists but its
// endpoint has been deactivated — callers surface this as 409, distinct from 404,
// so an operator can tell "nothing to replay" apart from "blocked, reactivate first".
func ReplayByID(ctx context.Context, db *pgxpool.Pool, id int64) error {
	tag, err := db.Exec(ctx, requeueSQL+` AND d.id = $1`, id)
	if err != nil {
		return fmt.Errorf("webhookout: replay by id: %w", err)
	}
	if tag.RowsAffected() > 0 {
		return nil
	}

	// Nothing was requeued. Disambiguate "no such dead_letter row" from "row
	// exists but its endpoint is inactive" with a second, targeted lookup —
	// only reached on the already-exceptional zero-rows path, so the extra
	// round trip never costs the common (successful) case anything.
	var active bool
	err = db.QueryRow(ctx, `
		SELECT we.active
		FROM webhook_deliveries d
		JOIN webhook_endpoints we ON we.id = d.endpoint_id
		WHERE d.id = $1 AND d.status = 'dead_letter'
	`, id).Scan(&active)
	if err != nil {
		if err == pgx.ErrNoRows {
			return pgx.ErrNoRows
		}
		return fmt.Errorf("webhookout: replay by id: disambiguate: %w", err)
	}
	// The row exists and matched status='dead_letter' but the first UPDATE still
	// affected 0 rows, so we.active must be FALSE (the only other requeueSQL condition).
	return ErrEndpointInactive
}

// ReplayByEndpoint requeues every dead_letter row belonging to endpointID and
// returns the ids that were requeued (empty slice, not an error, when none
// matched — including when endpointID itself is inactive, which the
// requeueSQL join excludes by construction).
func ReplayByEndpoint(ctx context.Context, db *pgxpool.Pool, endpointID uuid.UUID) ([]int64, error) {
	rows, err := db.Query(ctx, requeueSQL+` AND d.endpoint_id = $1 RETURNING d.id`, endpointID)
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
