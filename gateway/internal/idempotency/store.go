// Package idempotency deduplicates POST /v1/* retries within a TTL window.
//
// First request: executes, stores 2xx response.
// Identical retry (same customer + key + body fingerprint): replays stored response.
// Different body fingerprint: 422 IDEMPOTENCY_KEY_REUSE.
// In-flight concurrent request: 409 IDEMPOTENCY_CONFLICT.
// Non-2xx responses are never stored; the row is deleted so genuine retries succeed.
package idempotency

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/rs/zerolog/log"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const defaultTTL = 24 * time.Hour

// Entry is a stored idempotency record.
// StatusCode == nil means the owning request is still in-flight.
type Entry struct {
	Fingerprint     []byte
	StatusCode      *int
	Body            []byte
	ResponseHeaders http.Header
}

// Store persists idempotency records in Postgres.
// Reuses the gateway's existing pool; no lifecycle of its own.
type Store struct {
	db  *pgxpool.Pool
	ttl time.Duration
}

// NewStore returns a Store backed by db.
// Returns nil when db is nil (feature disabled; middleware is a pass-through).
func NewStore(db *pgxpool.Pool) *Store {
	if db == nil {
		return nil
	}
	return &Store{db: db, ttl: defaultTTL}
}

// Claim tries to insert a pending entry (status_code NULL) for the given key.
// Returns true if this caller now owns the key.
// Returns false with nil error when the key already exists (UNIQUE conflict).
// The UNIQUE constraint is the concurrency gate: only one concurrent INSERT wins.
func (s *Store) Claim(ctx context.Context, customerID uuid.UUID, key string, fingerprint []byte) (bool, error) {
	tag, err := s.db.Exec(ctx, `
		INSERT INTO idempotency_keys (customer_id, idempotency_key, fingerprint)
		VALUES ($1, $2, $3)
		ON CONFLICT DO NOTHING
	`, customerID, key, fingerprint)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// Load returns the stored Entry for the given key, or nil when absent or TTL-expired.
// An expired row is deleted so the caller can re-Claim.
func (s *Store) Load(ctx context.Context, customerID uuid.UUID, key string) (*Entry, error) {
	var fingerprint []byte
	var statusCode *int
	var body []byte
	var headersJSON []byte
	var createdAt time.Time

	err := s.db.QueryRow(ctx, `
		SELECT fingerprint, status_code, response_body, response_headers, created_at
		FROM idempotency_keys
		WHERE customer_id = $1 AND idempotency_key = $2
	`, customerID, key).Scan(&fingerprint, &statusCode, &body, &headersJSON, &createdAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}

	if time.Since(createdAt) > s.ttl {
		// Expired — delete so the next Claim can succeed. Best-effort; ignore error.
		_, _ = s.db.Exec(ctx, `
			DELETE FROM idempotency_keys WHERE customer_id = $1 AND idempotency_key = $2
		`, customerID, key)
		return nil, nil
	}

	var hdrs http.Header
	if len(headersJSON) > 0 {
		if err := json.Unmarshal(headersJSON, &hdrs); err != nil {
			log.Warn().Err(err).Str("key", key).Msg("idempotency: corrupt response_headers, failing open")
			return nil, err
		}
	}

	return &Entry{
		Fingerprint:     fingerprint,
		StatusCode:      statusCode,
		Body:            body,
		ResponseHeaders: hdrs,
	}, nil
}

// Finalize stores the 2xx response for a key owned by this request.
// Only called when the handler returned a success status.
func (s *Store) Finalize(ctx context.Context, customerID uuid.UUID, key string, statusCode int, body []byte, headers http.Header) error {
	hdrsJSON, merr := json.Marshal(headers)
	if merr != nil {
		hdrsJSON = []byte("{}")
	}
	_, err := s.db.Exec(ctx, `
		UPDATE idempotency_keys
		SET status_code = $1, response_body = $2, response_headers = $3
		WHERE customer_id = $4 AND idempotency_key = $5
	`, statusCode, body, hdrsJSON, customerID, key)
	return err
}

// Release removes a pending entry so genuine retries can proceed.
// Called when the handler returned a non-2xx status.
func (s *Store) Release(ctx context.Context, customerID uuid.UUID, key string) error {
	_, err := s.db.Exec(ctx, `
		DELETE FROM idempotency_keys WHERE customer_id = $1 AND idempotency_key = $2
	`, customerID, key)
	return err
}
