package webhookout

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Unluckyathecking/crucible/gateway/internal/paging"
)

// Delivery is one row of a customer's outbound webhook delivery log, as
// served by GET /v1/webhooks/deliveries.
type Delivery struct {
	ID               string    `json:"id"`
	EventID          string    `json:"event_id"`
	EndpointURL      string    `json:"endpoint_url"`
	Status           string    `json:"status"`
	Attempts         int       `json:"attempts"`
	LastResponseCode *int      `json:"last_response_code,omitempty"`
	CreatedAt        time.Time `json:"created_at"`
}

// ListDeliveries returns one page of customerID's webhook deliveries across
// all of their endpoints, newest first, plus the total row count. page and
// perPage must already be clamped (see paging.Clamp); paging.ErrPageTooLarge
// is returned before any query runs if the offset would exceed paging.MaxOffset.
func ListDeliveries(ctx context.Context, db *pgxpool.Pool, customerID uuid.UUID, page, perPage int) (paging.Page[Delivery], error) {
	offset, err := paging.Offset(page, perPage)
	if err != nil {
		return paging.Page[Delivery]{}, err
	}

	var total int64
	if err := db.QueryRow(ctx, `
		SELECT COUNT(*)
		FROM webhook_deliveries d
		JOIN webhook_endpoints we ON we.id = d.endpoint_id
		WHERE we.customer_id = $1
	`, customerID).Scan(&total); err != nil {
		return paging.Page[Delivery]{}, fmt.Errorf("webhookout: count deliveries: %w", err)
	}

	rows, err := db.Query(ctx, `
		SELECT d.id::text, d.event_id, we.url, d.status, d.attempts,
		       d.last_response_code, d.created_at
		FROM webhook_deliveries d
		JOIN webhook_endpoints we ON we.id = d.endpoint_id
		WHERE we.customer_id = $1
		ORDER BY d.created_at DESC, d.id DESC
		LIMIT $2 OFFSET $3
	`, customerID, perPage, offset)
	if err != nil {
		return paging.Page[Delivery]{}, fmt.Errorf("webhookout: list deliveries: %w", err)
	}
	defer rows.Close()

	items, err := pgx.CollectRows(rows, pgx.RowToStructByPos[Delivery])
	if err != nil {
		return paging.Page[Delivery]{}, fmt.Errorf("webhookout: scan deliveries: %w", err)
	}
	if items == nil {
		items = []Delivery{}
	}
	return paging.Page[Delivery]{Items: items, Total: total}, nil
}
