package auth

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
)

const (
	testSalt   = "thirty-two-bytes-of-salt-padding"
	testPrefix = "cru_"
)

// newTestRedis returns a redis client pointed at localhost:6379 or skips the test
// if no Redis is reachable. Follows the same pattern as ratelimit/bucket_test.go.
func newTestRedis(t *testing.T) *redis.Client {
	t.Helper()
	c := redis.NewClient(&redis.Options{Addr: "localhost:6379"})
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	if err := c.Ping(ctx).Err(); err != nil {
		t.Skipf("redis unavailable on localhost:6379, skipping: %v", err)
	}
	return c
}

// newTestPostgres returns a pgxpool connected to the local Postgres instance or
// skips the test if unreachable.
func newTestPostgres(t *testing.T) *pgxpool.Pool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, "postgres://crucible@localhost:5432/crucible?sslmode=disable")
	if err != nil {
		t.Skipf("postgres unavailable, skipping: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Skipf("postgres ping failed, skipping: %v", err)
	}
	return pool
}

// insertTestKey creates a customer + active api_key row and returns everything
// needed to exercise Lookup and Revoke.
func insertTestKey(t *testing.T, ctx context.Context, pool *pgxpool.Pool, salt string) (keyID uuid.UUID, fullKey string, prefix string) {
	t.Helper()

	fullKey, prefix, err := Generate(testPrefix)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	custID := uuid.New()
	email := fmt.Sprintf("test-%s@example.com", uuid.NewString()[:8])
	_, err = pool.Exec(ctx, `INSERT INTO customers (id, email, plan_id) VALUES ($1, $2, 'free') ON CONFLICT DO NOTHING`, custID, email)
	if err != nil {
		t.Fatalf("insert customer: %v", err)
	}

	hash := Hash(salt, fullKey)
	keyID = uuid.New()
	_, err = pool.Exec(ctx, `
		INSERT INTO api_keys (id, customer_id, prefix, hash, name)
		VALUES ($1, $2, $3, $4, 'store test')
	`, keyID, custID, prefix, hash)
	if err != nil {
		t.Fatalf("insert api_key: %v", err)
	}

	return keyID, fullKey, prefix
}

func TestStore_Lookup(t *testing.T) {
	db := newTestPostgres(t)
	defer db.Close()
	rdb := newTestRedis(t)
	ctx := context.Background()

	s := NewStore(db, rdb, testSalt)

	t.Run("finds key by prefix and validates hash", func(t *testing.T) {
		_, fullKey, _ := insertTestKey(t, ctx, db, testSalt)

		got, err := s.Lookup(ctx, fullKey)
		if err != nil {
			t.Fatalf("Lookup(%q): %v", fullKey, err)
		}
		if got.ID == uuid.Nil {
			t.Error("returned key has zero-value ID")
		}
		if got.Customer.ID == uuid.Nil {
			t.Error("returned customer has zero-value ID")
		}
		if got.Customer.Email == "" {
			t.Error("returned customer has empty email")
		}
	})

	t.Run("returns ErrKeyNotFound for wrong full key with matching prefix", func(t *testing.T) {
		_, _, prefix := insertTestKey(t, ctx, db, testSalt)

		differentKey := prefix + "ZXYZWXYZWXYZWXYZWX"

		_, err := s.Lookup(ctx, differentKey)
		if err != ErrKeyNotFound {
			t.Errorf("Lookup(wrong-hash) = %v, want ErrKeyNotFound", err)
		}
	})

	t.Run("returns ErrKeyNotFound for non-existent prefix", func(t *testing.T) {
		_, err := s.Lookup(ctx, "cru_live_NONEXISTENTPREFIX123")
		if err != ErrKeyNotFound {
			t.Errorf("Lookup(nonexistent) = %v, want ErrKeyNotFound", err)
		}
	})

	t.Run("returns ErrKeyNotFound for key shorter than PrefixLen", func(t *testing.T) {
		_, err := s.Lookup(ctx, "short")
		if err != ErrKeyNotFound {
			t.Errorf("Lookup(short) = %v, want ErrKeyNotFound", err)
		}
	})
}

func TestStore_Revoke(t *testing.T) {
	db := newTestPostgres(t)
	defer db.Close()
	rdb := newTestRedis(t)
	ctx := context.Background()

	s := NewStore(db, rdb, testSalt)

	t.Run("updates revoked_at AND deletes cache entry", func(t *testing.T) {
		keyID, fullKey, prefix := insertTestKey(t, ctx, db, testSalt)

		if _, err := s.Lookup(ctx, fullKey); err != nil {
			t.Fatalf("populate cache: %v", err)
		}

		exists, err := rdb.Exists(ctx, "auth:"+prefix).Result()
		if err != nil {
			t.Fatalf("check cache exists: %v", err)
		}
		if exists != 1 {
			t.Fatalf("cache entry %q should exist after Lookup, got exists=%d", "auth:"+prefix, exists)
		}

		if err := s.Revoke(ctx, keyID); err != nil {
			t.Fatalf("Revoke: %v", err)
		}

		cached, err := rdb.Get(ctx, "auth:"+prefix).Result()
		if err == nil {
			t.Errorf("cache entry %q still exists after revoke: %q", "auth:"+prefix, cached)
		}
		if err != redis.Nil {
			t.Errorf("unexpected redis error after revoke: %v", err)
		}

		// Lookup must fail because revoked_at IS NOT NULL.
		// revoked_at IS NOT NULL → no rows returned → ErrKeyNotFound
		if _, err := s.Lookup(ctx, fullKey); err != ErrKeyNotFound {
			t.Errorf("Lookup after revoke = %v, want ErrKeyNotFound", err)
		}
	})

	t.Run("idempotent — revoking an already-revoked key returns nil", func(t *testing.T) {
		keyID, _, _ := insertTestKey(t, ctx, db, testSalt)

		if err := s.Revoke(ctx, keyID); err != nil {
			t.Fatalf("first Revoke: %v", err)
		}
		if err := s.Revoke(ctx, keyID); err != nil {
			t.Errorf("second Revoke should be idempotent, got: %v", err)
		}
	})

	t.Run("revoking a non-existent key returns nil", func(t *testing.T) {
		if err := s.Revoke(ctx, uuid.New()); err != nil {
			t.Errorf("Revoke(nonexistent) = %v, want nil", err)
		}
	})
}

func TestStore_CacheMissFallsThroughToPostgres(t *testing.T) {
	db := newTestPostgres(t)
	defer db.Close()
	rdb := newTestRedis(t)
	ctx := context.Background()

	s := NewStore(db, rdb, testSalt)

	t.Run("lookup succeeds with cold cache", func(t *testing.T) {
		_, fullKey, prefix := insertTestKey(t, ctx, db, testSalt)

		// Ensure the cache is cold.
		rdb.Del(ctx, "auth:"+prefix)

		got, err := s.Lookup(ctx, fullKey)
		if err != nil {
			t.Fatalf("Lookup with cold cache: %v", err)
		}

		// After the cold lookup, the cache should be populated.
		cached, err := rdb.Get(ctx, "auth:"+prefix).Result()
		if err != nil {
			t.Errorf("cache should be populated after cold lookup: %v", err)
		}
		if cached == "" {
			t.Error("cache entry is empty after cold lookup")
		}

		if got.ID == uuid.Nil {
			t.Error("returned key has zero-value ID from cold cache path")
		}
	})

	t.Run("cache hit returns same result as cache miss", func(t *testing.T) {
		_, fullKey, prefix := insertTestKey(t, ctx, db, testSalt)

		// Cold path first.
		rdb.Del(ctx, "auth:"+prefix)
		cold, err := s.Lookup(ctx, fullKey)
		if err != nil {
			t.Fatalf("cold lookup: %v", err)
		}

		// Warm path second — should hit cache.
		warm, err := s.Lookup(ctx, fullKey)
		if err != nil {
			t.Fatalf("warm lookup: %v", err)
		}

		if cold.ID != warm.ID {
			t.Errorf("cold ID=%s != warm ID=%s", cold.ID, warm.ID)
		}
		if cold.Customer.Email != warm.Customer.Email {
			t.Errorf("cold email=%q != warm email=%q", cold.Customer.Email, warm.Customer.Email)
		}
	})
}

func TestStore_ConstantTimeComparison(t *testing.T) {
	t.Run("VerifyHash rejects different inputs", func(t *testing.T) {
		salt := testSalt
		key := "cru_live_TESTKEY12345"
		h := Hash(salt, key)

		tests := []struct {
			name string
			a    []byte
			b    []byte
			want bool
		}{
			{"identical hashes match", h, h, true},
			{"different key produces mismatch", h, Hash(salt, "cru_live_OTHERKEY"), false},
			{"different salt produces mismatch", h, Hash("other-salt-bytes-aaaaaaaaaaaaaaa", key), false},
			{"empty hash vs valid hash", h, []byte{}, false},
			{"nil hash vs valid hash", h, nil, false},
		}

		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				got := VerifyHash(tt.a, tt.b)
				if got != tt.want {
					t.Errorf("VerifyHash(...) = %v, want %v", got, tt.want)
				}
			})
		}
	})
}

func TestLookup_NegativePrefixCache(t *testing.T) {
	db := newTestPostgres(t)
	defer db.Close()
	rdb := newTestRedis(t)
	ctx := context.Background()

	s := NewStore(db, rdb, testSalt)
	defer s.Close()

	// Use a prefix guaranteed not to be in the database.
	unknownKey := "cru_XXXXXXXXXXXXXXXXXXXXXXXXXXXX"
	prefix := unknownKey[:PrefixLen]
	missKey := "auth:miss:" + prefix

	// Ensure clean state from any prior run.
	rdb.Del(ctx, missKey)

	t.Run("first call hits Postgres and creates sentinel", func(t *testing.T) {
		_, err := s.Lookup(ctx, unknownKey)
		if !errors.Is(err, ErrKeyNotFound) {
			t.Fatalf("Lookup(unknown) = %v, want ErrKeyNotFound", err)
		}
		ttl, err := rdb.TTL(ctx, missKey).Result()
		if err != nil {
			t.Fatalf("TTL(%q): %v", missKey, err)
		}
		if ttl <= 0 || ttl > 30*time.Second {
			t.Errorf("sentinel TTL = %v, want (0, 30s]", ttl)
		}
	})

	t.Run("second call reads sentinel from Redis, returns ErrKeyNotFound without DB query", func(t *testing.T) {
		// Confirm sentinel exists before the call so the subtest has a clear precondition.
		exists, err := rdb.Exists(ctx, missKey).Result()
		if err != nil {
			t.Fatalf("check sentinel exists: %v", err)
		}
		if exists != 1 {
			t.Fatalf("sentinel %q must exist before second call", missKey)
		}

		_, err = s.Lookup(ctx, unknownKey)
		if !errors.Is(err, ErrKeyNotFound) {
			t.Fatalf("Lookup(unknown, sentinel) = %v, want ErrKeyNotFound", err)
		}

		// Sentinel must still be alive — Lookup returns before the DB query and never
		// deletes or rewrites the sentinel on the hit path.
		ttl, err := rdb.TTL(ctx, missKey).Result()
		if err != nil {
			t.Fatalf("TTL(%q) after second call: %v", missKey, err)
		}
		if ttl <= 0 {
			t.Errorf("sentinel TTL = %v after second call, want > 0", ttl)
		}
	})

	t.Run("after Del sentinel, next call hits Postgres and recreates sentinel", func(t *testing.T) {
		if err := rdb.Del(ctx, missKey).Err(); err != nil {
			t.Fatalf("Del sentinel: %v", err)
		}

		_, err := s.Lookup(ctx, unknownKey)
		if !errors.Is(err, ErrKeyNotFound) {
			t.Fatalf("Lookup after Del = %v, want ErrKeyNotFound", err)
		}
		ttl, err := rdb.TTL(ctx, missKey).Result()
		if err != nil {
			t.Fatalf("TTL after sentinel re-creation: %v", err)
		}
		if ttl <= 0 || ttl > 30*time.Second {
			t.Errorf("re-created sentinel TTL = %v, want (0, 30s]", ttl)
		}
	})
}

func TestStore_PrefixLookupIsCaseSensitive(t *testing.T) {
	db := newTestPostgres(t)
	defer db.Close()
	rdb := newTestRedis(t)
	ctx := context.Background()

	s := NewStore(db, rdb, testSalt)

	_, fullKey, _ := insertTestKey(t, ctx, db, testSalt)

	t.Run("exact prefix match finds key", func(t *testing.T) {
		_, err := s.Lookup(ctx, fullKey)
		if err != nil {
			t.Errorf("exact prefix lookup failed: %v", err)
		}
	})

	t.Run("different prefix returns ErrKeyNotFound", func(t *testing.T) {
		// Prefix is the first PrefixLen characters. Any change to those chars
		// means it won't match the indexed prefix in the DB.
		differentPrefix := "cru_live_XXXXXXXXXXXXXXZZZZ" + fullKey[PrefixLen:]
		_, err := s.Lookup(ctx, differentPrefix)
		if err != ErrKeyNotFound {
			t.Errorf("different-prefix lookup = %v, want ErrKeyNotFound", err)
		}
	})
}
