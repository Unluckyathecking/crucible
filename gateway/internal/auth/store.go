package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
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

// missTTL is how long an unknown prefix is cached as a negative sentinel to
// absorb repeated DB probes on random-but-plausible prefixes (Case B DoS path).
const missTTL = 30 * time.Second

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
	wg      sync.WaitGroup
	// rootCtx is the long-lived parent for best-effort last_used_at writes; each write
	// derives a short timeout from it instead of a detached context.Background(), so the
	// writes can never outlive the Store. Close cancels it once the queue has drained,
	// which also unblocks any write still stuck in the DB driver past the drain budget.
	rootCtx context.Context
	cancel  context.CancelFunc
}

func NewStore(db *pgxpool.Pool, cache *redis.Client, salt string) *Store {
	rootCtx, cancel := context.WithCancel(context.Background())
	s := &Store{
		db:      db,
		cache:   cache,
		salt:    salt,
		updates: make(chan uuid.UUID, 1000),
		rootCtx: rootCtx,
		cancel:  cancel,
	}
	s.wg.Add(1)
	go s.processUpdates()
	return s
}

// enqueueUpdate queues a best-effort last_used_at write. The send is non-blocking:
// when the buffer is full the update is dropped rather than blocking the request hot
// path. This is what bounds background work during a cold-cache storm (e.g. Redis
// outage) — concurrent DB writes never exceed the single processUpdates worker, and
// excess arrivals are shed. Returns true if the update was queued, false if dropped.
func (s *Store) enqueueUpdate(keyID uuid.UUID) bool {
	select {
	case s.updates <- keyID:
		return true
	default:
		return false
	}
}

func (s *Store) processUpdates() {
	defer s.wg.Done()
	for keyID := range s.updates {
		ctx, cancel := context.WithTimeout(s.rootCtx, 2*time.Second)
		_, _ = s.db.Exec(ctx, `UPDATE api_keys SET last_used_at = NOW() WHERE id = $1`, keyID)
		cancel()
	}
}

// Close stops accepting last_used_at updates, drains the queued ones best-effort, and
// waits for the background worker to finish. Call once during graceful shutdown.
//
// Semantics are drain-then-cancel, not abort: closing the channel lets processUpdates
// finish the already-queued writes — each capped by its own 2s timeout derived from
// rootCtx, so the whole drain is bounded and a single stuck write can hang shutdown for
// at most 2s — and then cancel() tears down rootCtx so no later derived context can
// outlive the Store. cancel() is deferred to guarantee that teardown on every return
// path. (This is graceful drain, not in-flight abort: a queued write that has already
// started commits; it is the per-write timeout, not cancel, that bounds shutdown.)
func (s *Store) Close() {
	defer s.cancel()
	close(s.updates)
	s.wg.Wait()
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

	// Negative-prefix sentinel: if this prefix was previously queried and found absent
	// in Postgres, skip both Redis and DB for missTTL to bound repeated-miss DB load.
	if v, err := s.cache.Get(ctx, "auth:miss:"+prefix).Result(); err == nil && v == "1" {
		return nil, ErrKeyNotFound
	}

	// Redis hot path.
	if cached, err := s.cache.Get(ctx, "auth:"+prefix).Bytes(); err == nil {
		var c cacheEntry
		if json.Unmarshal(cached, &c) == nil && VerifyHash(wantHash, c.Hash) {
			return &Key{
				ID:       c.KeyID,
				Customer: Customer{ID: c.CustomerID, Email: c.Email, Plan: c.Plan},
			}, nil
		}
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
			_ = s.cache.Set(ctx, "auth:miss:"+prefix, "1", missTTL).Err()
			return nil, ErrKeyNotFound
		}
		return nil, fmt.Errorf("lookup api key: %w", err)
	}
	if !VerifyHash(wantHash, storedHash) {
		return nil, ErrKeyNotFound
	}

	// Populate cache for the next call.
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

	// Fire-and-forget last_used update — don't block the request hot path.
	s.enqueueUpdate(keyID)

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
