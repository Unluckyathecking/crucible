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
	"github.com/rs/zerolog/log"

	"github.com/Unluckyathecking/crucible/gateway/internal/audit"
	"github.com/Unluckyathecking/crucible/gateway/internal/events"
	"github.com/Unluckyathecking/crucible/gateway/internal/paging"
	"github.com/Unluckyathecking/crucible/gateway/internal/webhookout"
)

// ErrKeyNotFound is returned when a presented API key isn't recognised.
// Treated as 401 by the middleware. Always returned for both "no row" and
// "row exists but hash mismatch" to avoid leaking which prefix is real.
var ErrKeyNotFound = errors.New("api key not found")

// ErrKeyRotating is returned by Store.Rotate when the key exists and is owned
// by the caller but already has expires_at set — it was rotated once and is
// still in its grace window. Distinct from ErrKeyNotFound so the HTTP layer
// can return 409 instead of a misleading 404.
var ErrKeyRotating = errors.New("api key already rotated; in grace period")

// missTTL is how long an unknown prefix is cached as a negative sentinel to
// absorb repeated DB probes on random-but-plausible prefixes (Case B DoS path).
const missTTL = 30 * time.Second

// maxGrace caps the rotation grace window server-side so callers cannot hold
// both the old and the new key valid indefinitely.
const maxGrace = 7 * 24 * time.Hour

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
	// rootCtx roots the per-write timeouts; each write derives a short 2s timeout from it
	// instead of a detached context.Background(), so no write can outlive the Store.
	// cancel() is called by Close() via defer — after wg.Wait() returns, meaning after
	// processUpdates has finished its final write — so cancel() does not abort an in-flight
	// write; the per-write 2s WithTimeout is the only bound on a stuck write.
	rootCtx context.Context
	cancel  context.CancelFunc
	// emitter is optional; nil → Revoke/Rotate emit no outbound webhook event.
	emitter *webhookout.Emitter
}

// SetEmitter wires the outbound webhook emitter used to notify customers of
// key rotation/revocation. Called once from server.NewRouter, since the
// emitter is constructed there (from Deps.DB) after main.go builds the Store.
// A nil emitter (e.g. Deps.DB unset) makes emission a safe no-op — Emitter.Emit
// nil-checks its receiver.
func (s *Store) SetEmitter(e *webhookout.Emitter) {
	s.emitter = e
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
	var customerID uuid.UUID
	err := s.db.QueryRow(ctx, `
		UPDATE api_keys SET revoked_at = NOW()
		WHERE id = $1 AND revoked_at IS NULL
		RETURNING prefix, customer_id
	`, keyID).Scan(&prefix, &customerID)
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

	// Best-effort audit — failure must not fail the revocation. Mirrors Rotate's
	// audit.Emit call below.
	targetType := "api_key"
	targetID := keyID.String()
	_ = audit.Emit(ctx, s.db, audit.Event{
		ActorType:  audit.ActorCustomer,
		ActorID:    customerID.String(),
		Action:     "api_key.revoked",
		TargetType: &targetType,
		TargetID:   &targetID,
		Details:    map[string]any{"prefix": prefix},
	})

	// Best-effort outbound webhook emission. Never returned as an error: the
	// revocation itself already committed, so an Emit failure (or nil emitter)
	// must not fail or delay the caller.
	if s.emitter != nil {
		payload, err := json.Marshal(events.APIKeyRevokedPayload{CustomerID: customerID.String(), KeyID: keyID.String()})
		if err != nil {
			log.Warn().Err(err).Msg("webhook emit: api_key.revoked payload marshal failed")
		} else if err := s.emitter.Emit(ctx, customerID, events.APIKeyRevoked, payload); err != nil {
			log.Warn().Err(err).Str("key_id", keyID.String()).Msg("webhook emit failed for api_key.revoked")
		}
	}
	return nil
}

// Rotate issues a replacement key for keyID, setting the old key's expires_at to
// now+grace (server-clamped to maxGrace) so both keys authenticate during the grace
// window. The returned newFullKey is shown to the customer once; only the prefix and
// hash are persisted.
//
// The old key's Redis cache entry is deleted after commit so the next Lookup re-reads
// from Postgres and caches the new expires_at. This ensures the hot path can enforce
// the expiry deadline without waiting for the 60s cache TTL to lapse — the key subtlety
// the spec calls out: time-based validity with no event trigger.
func (s *Store) Rotate(ctx context.Context, keyID uuid.UUID, keyPrefix string, grace time.Duration) (newFullKey string, newKeyID uuid.UUID, err error) {
	if grace < 0 {
		grace = 0
	}
	if grace > maxGrace {
		grace = maxGrace
	}

	tx, err := s.db.Begin(ctx)
	if err != nil {
		return "", uuid.Nil, fmt.Errorf("rotate: begin tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	// Lock the old key for the duration of the transaction to prevent concurrent
	// revocations or double-rotations from interleaving.
	var oldPrefix string
	var customerID uuid.UUID
	var oldName *string
	err = tx.QueryRow(ctx, `
		SELECT prefix, customer_id, name FROM api_keys
		WHERE id = $1 AND revoked_at IS NULL
		  AND expires_at IS NULL
		FOR UPDATE
	`, keyID).Scan(&oldPrefix, &customerID, &oldName)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// Distinguish "already rotated (in-grace)" from "genuinely absent/revoked".
			// A key that passed ownedKeyID's ownership check (Owner only filters revoked_at)
			// but has expires_at set was rotated once and is still in its grace window —
			// re-rotating it must return ErrKeyRotating → 409, not a misleading 404.
			var inGrace bool
			_ = tx.QueryRow(ctx, `
				SELECT EXISTS(
					SELECT 1 FROM api_keys
					WHERE id = $1 AND revoked_at IS NULL AND expires_at IS NOT NULL
				)
			`, keyID).Scan(&inGrace)
			if inGrace {
				return "", uuid.Nil, ErrKeyRotating
			}
			return "", uuid.Nil, ErrKeyNotFound
		}
		return "", uuid.Nil, fmt.Errorf("rotate: lock old key: %w", err)
	}

	newFull, newPrefix, err := Generate(keyPrefix)
	if err != nil {
		return "", uuid.Nil, fmt.Errorf("rotate: generate: %w", err)
	}
	newHash := Hash(s.salt, newFull)

	err = tx.QueryRow(ctx, `
		INSERT INTO api_keys (customer_id, prefix, hash, name)
		VALUES ($1, $2, $3, $4) RETURNING id
	`, customerID, newPrefix, newHash, oldName).Scan(&newKeyID)
	if err != nil {
		return "", uuid.Nil, fmt.Errorf("rotate: insert new key: %w", err)
	}

	expiresAt := time.Now().Add(grace)
	_, err = tx.Exec(ctx, `UPDATE api_keys SET expires_at = $1 WHERE id = $2`, expiresAt, keyID)
	if err != nil {
		return "", uuid.Nil, fmt.Errorf("rotate: set expiry: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return "", uuid.Nil, fmt.Errorf("rotate: commit: %w", err)
	}

	// Best-effort cache invalidation: force the next Lookup of the old key to re-read
	// from Postgres so the new expires_at is stored in the cache entry. Mirrors Revoke.
	_ = s.cache.Del(ctx, "auth:"+oldPrefix).Err()

	// Best-effort audit — failure must not fail the rotation.
	targetType := "api_key"
	targetID := keyID.String()
	_ = audit.Emit(ctx, s.db, audit.Event{
		ActorType:  audit.ActorCustomer,
		ActorID:    customerID.String(),
		Action:     "api_key.rotated",
		TargetType: &targetType,
		TargetID:   &targetID,
		Details:    map[string]any{"prefix": oldPrefix},
	})

	// Best-effort outbound webhook emission — failure must not fail the rotation,
	// which already committed above.
	if s.emitter != nil {
		payload, err := json.Marshal(events.APIKeyRotatedPayload{
			CustomerID: customerID.String(),
			OldKeyID:   keyID.String(),
			NewKeyID:   newKeyID.String(),
		})
		if err != nil {
			log.Warn().Err(err).Msg("webhook emit: api_key.rotated payload marshal failed")
		} else if err := s.emitter.Emit(ctx, customerID, events.APIKeyRotated, payload); err != nil {
			log.Warn().Err(err).Str("key_id", keyID.String()).Msg("webhook emit failed for api_key.rotated")
		}
	}

	return newFull, newKeyID, nil
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
			// Reject expired-but-cached keys immediately without waiting for the 60s TTL.
			// expires_at is set during rotation; checking it here enforces the grace-window
			// deadline on the hot path rather than only on the cold DB path.
			if c.ExpiresAt != nil && time.Now().After(*c.ExpiresAt) {
				return nil, ErrKeyNotFound
			}
			return &Key{
				ID:       c.KeyID,
				Customer: Customer{ID: c.CustomerID, Email: c.Email, Plan: c.Plan},
			}, nil
		}
	}

	// Cold path: query Postgres (idx_api_keys_active_prefix makes this O(1) + verify).
	// Excludes revoked and expired keys so the trust boundary is enforced in one query.
	row := s.db.QueryRow(ctx, `
		SELECT k.id, k.hash, c.id, c.email, c.plan_id, k.expires_at
		FROM api_keys k
		JOIN customers c ON c.id = k.customer_id
		WHERE k.prefix = $1 AND k.revoked_at IS NULL
		  AND (k.expires_at IS NULL OR k.expires_at > NOW())
		LIMIT 1
	`, prefix)

	var keyID, customerID uuid.UUID
	var storedHash []byte
	var email, plan string
	var expiresAt *time.Time
	if err := row.Scan(&keyID, &storedHash, &customerID, &email, &plan, &expiresAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			_ = s.cache.Set(ctx, "auth:miss:"+prefix, "1", missTTL).Err()
			return nil, ErrKeyNotFound
		}
		return nil, fmt.Errorf("lookup api key: %w", err)
	}
	if !VerifyHash(wantHash, storedHash) {
		return nil, ErrKeyNotFound
	}

	// Populate cache for the next call. ExpiresAt is included so the hot path can
	// enforce the rotation grace-window deadline without another DB round-trip.
	entry := cacheEntry{
		KeyID:      keyID,
		CustomerID: customerID,
		Email:      email,
		Plan:       plan,
		Hash:       storedHash,
		ExpiresAt:  expiresAt,
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

// KeyListItem is the customer-visible projection of an active api_keys row —
// the hash is intentionally never selected, mirroring webhookout.Endpoint's
// exclusion of its secret column. Field order matches the SELECT in List for
// pgx.RowToStructByPos.
type KeyListItem struct {
	ID         uuid.UUID
	Prefix     string
	Name       *string
	LastUsedAt *time.Time
	ExpiresAt  *time.Time
	CreatedAt  time.Time
}

// List returns customerID's active (non-revoked, non-expired) API keys, most
// recently created first. Never selects the hash column.
// List returns a paginated page of customerID's active (non-revoked,
// unexpired) API keys, most-recently created first, plus the total matching
// row count across all pages. page/perPage must already be clamped (see
// paging.Clamp) — List only computes the SQL OFFSET, returning
// paging.ErrPageTooLarge if it would exceed paging.MaxOffset.
func (s *Store) List(ctx context.Context, customerID uuid.UUID, page, perPage int) (paging.Page[KeyListItem], error) {
	offset, err := paging.Offset(page, perPage)
	if err != nil {
		return paging.Page[KeyListItem]{}, err
	}

	var total int64
	if err := s.db.QueryRow(ctx, `
		SELECT COUNT(*) FROM api_keys
		WHERE customer_id = $1 AND revoked_at IS NULL
		  AND (expires_at IS NULL OR expires_at > NOW())
	`, customerID).Scan(&total); err != nil {
		return paging.Page[KeyListItem]{}, fmt.Errorf("count api keys: %w", err)
	}

	rows, err := s.db.Query(ctx, `
		SELECT id, prefix, name, last_used_at, expires_at, created_at
		FROM api_keys
		WHERE customer_id = $1 AND revoked_at IS NULL
		  AND (expires_at IS NULL OR expires_at > NOW())
		ORDER BY created_at DESC
		LIMIT $2 OFFSET $3
	`, customerID, perPage, offset)
	if err != nil {
		return paging.Page[KeyListItem]{}, fmt.Errorf("list api keys: %w", err)
	}
	defer rows.Close()

	items, err := pgx.CollectRows(rows, pgx.RowToStructByPos[KeyListItem])
	if err != nil {
		return paging.Page[KeyListItem]{}, fmt.Errorf("scan api keys: %w", err)
	}
	if items == nil {
		items = []KeyListItem{}
	}
	return paging.Page[KeyListItem]{Items: items, Total: total}, nil
}

// Owner returns the customer_id owning the active (non-revoked) key with id
// keyID, or ErrKeyNotFound if no such key exists.
//
// Revoke and Rotate are id-only — they don't themselves filter by customer_id
// (Revoke is also called from contexts where the caller's identity isn't yet
// resolved). Owner lets the HTTP layer perform an IDOR-safe ownership check
// before ever invoking those methods with a caller-supplied id: not-found and
// owned-by-someone-else must be indistinguishable to the caller.
func (s *Store) Owner(ctx context.Context, keyID uuid.UUID) (uuid.UUID, error) {
	var customerID uuid.UUID
	err := s.db.QueryRow(ctx, `
		SELECT customer_id FROM api_keys WHERE id = $1 AND revoked_at IS NULL
	`, keyID).Scan(&customerID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return uuid.Nil, ErrKeyNotFound
		}
		return uuid.Nil, fmt.Errorf("lookup key owner: %w", err)
	}
	return customerID, nil
}

type cacheEntry struct {
	KeyID      uuid.UUID  `json:"k"`
	CustomerID uuid.UUID  `json:"c"`
	Email      string     `json:"e"`
	Plan       string     `json:"p"`
	Hash       []byte     `json:"h"`
	ExpiresAt  *time.Time `json:"x,omitempty"`
}
