// Package selferrors implements the customer-scoped, read-only error-history
// endpoint (GET /v1/errors): the caller's own error_events rows — populated by
// errorlog.ErrorRecorder on every non-2xx /v1 response — newest-first, filtered
// by date range/operation/code, and paginated.
//
// Read-only: no method here ever mutates error_events. The customer is always
// the authenticated caller (auth.FromContext); there is no customer_id
// parameter, so cross-customer lookup (IDOR) is structurally impossible.
package selferrors

import (
	"context"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// maxDisplayPayloadBytes bounds the request_payload string returned to
// callers. The gateway already truncates stored payloads at
// config.ErrorPayloadMaxBytes (default 4 KiB); this cap is higher so it never
// silently truncates an operator-enlarged limit, mirroring the dashboard's
// MAX_PAYLOAD_DISPLAY_BYTES.
const maxDisplayPayloadBytes = 8192

// Event is one error_events row scoped to a single customer.
type Event struct {
	ID             int64     `json:"id"`
	Operation      string    `json:"operation"`
	ErrorCode      string    `json:"error_code"`
	HTTPStatus     int       `json:"http_status"`
	Message        string    `json:"message"`
	RequestID      string    `json:"request_id"`
	CreatedAt      time.Time `json:"created_at"`
	RequestPayload *string   `json:"request_payload"`
}

// Store queries the caller-scoped error_events rows for the self-service
// error-history endpoint.
type Store struct {
	db *pgxpool.Pool
}

// NewStore returns a Store backed by db. Handler is only ever registered when
// d.DB is non-nil (routes.go's d.DB != nil block), so db is never nil here.
func NewStore(db *pgxpool.Pool) *Store {
	return &Store{db: db}
}

// Query returns customerID's error_events rows in [from, toExclusive), newest
// first, optionally filtered by exact operation and/or error code match,
// served via idx_error_events_customer_created. Fetches limit+1 rows so the
// caller can compute has_more without a second COUNT query; the extra row is
// trimmed before returning.
func (s *Store) Query(
	ctx context.Context,
	customerID uuid.UUID,
	from, toExclusive time.Time,
	operation, code *string,
	limit, offset int,
) ([]Event, bool, error) {
	rows, err := s.db.Query(ctx, `
		SELECT id, operation, error_code, http_status, message, request_id, created_at, request_payload
		FROM error_events
		WHERE customer_id = $1
		  AND created_at >= $2
		  AND created_at < $3
		  AND ($4::text IS NULL OR operation = $4)
		  AND ($5::text IS NULL OR error_code = $5)
		ORDER BY created_at DESC
		LIMIT $6 OFFSET $7
	`, customerID, from, toExclusive, operation, code, limit+1, offset)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()

	var events []Event
	for rows.Next() {
		var (
			e       Event
			payload []byte
		)
		if err := rows.Scan(&e.ID, &e.Operation, &e.ErrorCode, &e.HTTPStatus,
			&e.Message, &e.RequestID, &e.CreatedAt, &payload); err != nil {
			return nil, false, err
		}
		if payload != nil {
			p := boundedUTF8String(payload, maxDisplayPayloadBytes)
			e.RequestPayload = &p
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

// boundedUTF8String truncates b to at most maxBytes and replaces any invalid
// UTF-8 (including a partial multi-byte sequence left at the truncation
// boundary) so the result is always a valid string. request_payload is raw,
// untrusted request bytes (BYTEA) with no UTF-8 guarantee.
func boundedUTF8String(b []byte, maxBytes int) string {
	if len(b) > maxBytes {
		b = b[:maxBytes]
	}
	return strings.ToValidUTF8(string(b), "")
}
