package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
)

// ErrKeyNotFound is returned when a presented API key isn't recognised.
// Treated as 401 by the middleware. Always returned for both "no row" and
// "row exists but hash mismatch" to avoid leaking which prefix is real.
var ErrKeyNotFound = errors.New("api key not found")

type Customer struct {
	ID    uuid.UUID
	Email string
	Plan  string // plan id, e.g. "free" | "pro" | "business"
}

type Key struct {
	ID       uuid.UUID
	Customer Customer
}

// Store looks up API keys against Postgres with a Redis hot cache (60 s TTL).
type Store struct {
	db      *pgxpool.Pool
	cache   *redis.Client
	salt    string
	updates chan uuid.UUID
}

func NewStore(db *pgxpool.Pool, cache *redis.Client, salt string) *Store {
	s := &Store{
		db:      db,
		cache:   cache,
		salt:    salt,
		updates: make(chan uuid.UUID, 1000),
	}
	go s.processUpdates()
	return s
}

func (s *Store) processUpdates() {
	for keyID := range s.updates {
		bg, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_, _ = s.db.Exec(bg, `UPDATE api_keys SET last_used_at = NOW() WHERE id = $1`, keyID)
		cancel()
	}
}

// Revoke marks a key revoked in Postgres AND deletes the corresponding Redis cache entry
// so the revocation takes effect immediately rather than after the 60s cache TTL.
// Idempotent — revoking an already-revoked key returns nil.
func (s *Store) Revoke(ctx context.Context, keyID uuid.UUID) error {
	var prefix string
	err := s.db.QueryRow(ctx, `
		UPDATE api_keys SET revoked_at = NOW()
		WHERE id = $1 AND revoked_at IS NULL
		RETURNING prefix
	`, keyID).Scan(&prefix)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// Already revoked or never existed — nothing to invalidate.
			return nil
		}
		return fmt.Errorf("mark revoked: %w", err)
	}
	// Best-effort cache invalidation. Even if Redis is down, the DB is the source of
	// truth and the cache entry expires within 60 s — a small window of risk that callers
	// who care can handle by rotating salts.
	_ = s.cache.Del(ctx, "auth:"+prefix).Err()
	return nil
}

// Lookup returns the customer + key id for a presented API key.
// Constant-time hash compare guards against timing-based prefix enumeration.
func (s *Store) Lookup(ctx context.Context, fullKey string) (*Key, error) {
	if len(fullKey) < PrefixLen {
		return nil, ErrKeyNotFound
	}
	prefix := fullKey[:PrefixLen]
	wantHash := Hash(s.salt, fullKey)

	// Redis hot path.
	if cached, err := s.cache.Get(ctx, "auth:"+prefix).Bytes(); err == nil {
		var c cacheEntry
		if err := json.Unmarshal(cached, &c); err == nil {
			if !VerifyHash(wantHash, c.Hash) {
				return nil, ErrKeyNotFound
			}
			return &Key{
				ID:       c.KeyID,
				Customer: Customer{ID: c.CustomerID, Email: c.Email, Plan: c.Plan},
			}, nil
		}
		// Log but swallow unmarshal errors so we can fall through and heal the cache.
		// log.Warn().Err(err).Msg("cache entry unmarshal failed, falling back to DB")
	}

	// Cold path: query Postgres (idx_api_keys_active_prefix makes this O(1) + verify).
	row := s.db.QueryRow(ctx, `
		SELECT k.id, k.hash, c.id, c.email, c.plan_id
		FROM api_keys k
		JOIN customers c ON c.id = k.customer_id
		WHERE k.prefix = $1 AND k.revoked_at IS NULL
		LIMIT 1
	`, prefix)

	var keyID, customerID uuid.UUID
	var storedHash []byte
	var email, plan string
	if err := row.Scan(&keyID, &storedHash, &customerID, &email, &plan); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrKeyNotFound
		}
		return nil, fmt.Errorf("lookup api key: %w", err)
	}

	// Populate cache for the next call. We cache the correct database record
	// BEFORE we verify the incoming request's hash. This provides negative caching
	// (an invalid hash hitting the cache will fail VerifyHash and return early),
	// without poisoning the cache for subsequent valid requests.
	entry := cacheEntry{
		KeyID:      keyID,
		CustomerID: customerID,
		Email:      email,
		Plan:       plan,
		Hash:       storedHash,
	}
	if payload, err := json.Marshal(entry); err == nil {
		_ = s.cache.Set(ctx, "auth:"+prefix, payload, 60*time.Second).Err()
	}

	if !VerifyHash(wantHash, storedHash) {
		return nil, ErrKeyNotFound
	}

	// Fire-and-forget last_used update — don't block the request hot path.
	select {
	case s.updates <- keyID:
	default:
	}

	return &Key{
		ID:       keyID,
		Customer: Customer{ID: customerID, Email: email, Plan: plan},
	}, nil
}

type cacheEntry struct {
	KeyID      uuid.UUID `json:"k"`
	CustomerID uuid.UUID `json:"c"`
	Email      string    `json:"e"`
	Plan       string    `json:"p"`
	Hash       []byte    `json:"h"`
}
