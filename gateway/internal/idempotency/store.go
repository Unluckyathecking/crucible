// Package idempotency deduplicates POST /v1/* retries within a TTL window.
//
// First request: executes, stores 2xx response.
// Identical retry (same customer + key + fingerprint SHA-256(method + \x00 + requestURI + \x00 + body)): replays stored response.
// Different body fingerprint: 422 IDEMPOTENCY_KEY_REUSE.
// In-flight concurrent request: 409 IDEMPOTENCY_CONFLICT.
// Non-2xx responses are never stored; the row is deleted so genuine retries succeed.
package idempotency

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
// TTL comparison is done in SQL using the DB clock to avoid Go/Postgres clock skew.
// An expired row is deleted so the caller can re-Claim.
func (s *Store) Load(ctx context.Context, customerID uuid.UUID, key string) (*Entry, error) {
	var fingerprint []byte
	var statusCode *int
	var body []byte
	var headersJSON []byte

	// Filter expired rows in SQL so TTL uses a single clock domain (the DB).
	// Milliseconds (int64) avoids float64 precision issues at the TTL boundary.
	err := s.db.QueryRow(ctx, `
		SELECT fingerprint, status_code, response_body, response_headers
		FROM idempotency_keys
		WHERE customer_id = $1 AND idempotency_key = $2
		  AND created_at >= NOW() - ($3 * INTERVAL '1 millisecond')
	`, customerID, key, s.ttl.Milliseconds()).Scan(&fingerprint, &statusCode, &body, &headersJSON)
	if err == nil {
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
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}

	// Row absent or expired. Delete any expired row so a subsequent Claim can succeed.
	// Uses the same DB clock as the SELECT above, so both checks are consistent.
	if _, delErr := s.db.Exec(ctx, `
		DELETE FROM idempotency_keys
		WHERE customer_id = $1 AND idempotency_key = $2
		  AND created_at < NOW() - ($3 * INTERVAL '1 millisecond')
	`, customerID, key, s.ttl.Milliseconds()); delErr != nil {
		return nil, delErr
	}
	return nil, nil
}

// Finalize stores the 2xx response for a key owned by this request.
// Only called when the handler returned a success status.
func (s *Store) Finalize(ctx context.Context, customerID uuid.UUID, key string, statusCode int, body []byte, headers http.Header, fingerprint []byte) error {
	hdrsJSON, merr := json.Marshal(headers)
	if merr != nil {
		return fmt.Errorf("idempotency: marshal response headers: %w", merr)
	}
	result, err := s.db.Exec(ctx, `
		UPDATE idempotency_keys
		SET status_code = $1, response_body = $2, response_headers = $3
		WHERE customer_id = $4 AND idempotency_key = $5 AND fingerprint = $6 AND status_code IS NULL
	`, statusCode, body, hdrsJSON, customerID, key, fingerprint)
	if err != nil {
		return err
	}
	if result.RowsAffected() == 0 {
		return errors.New("idempotency: finalize affected 0 rows; key may have expired or been re-claimed")
	}
	return nil
}

// Release removes a pending (in-flight) entry so genuine retries can proceed.
// The AND status_code IS NULL guard is critical: if Finalize's UPDATE committed on
// the Postgres side but the connection dropped before Go read the acknowledgement,
// Finalize returns an error and the middleware calls Release as a fallback. Without
// the guard, Release would delete the now-finalized row, allowing the next retry to
// claim a fresh key and re-invoke the worker — exactly the double-billing this
// module exists to prevent.
func (s *Store) Release(ctx context.Context, customerID uuid.UUID, key string) error {
	_, err := s.db.Exec(ctx, `
		DELETE FROM idempotency_keys
		WHERE customer_id = $1 AND idempotency_key = $2 AND status_code IS NULL
	`, customerID, key)
	return err
}
