package idempotency

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Unluckyathecking/crucible/gateway/internal/db"
)

func newTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, "postgres://crucible@localhost:5432/crucible?sslmode=disable")
	if err != nil {
		t.Skipf("postgres unavailable: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Skipf("postgres ping failed: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func applyMigrations(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	if err := db.Apply(context.Background(), pool); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}
}

// insertTestCustomer inserts a minimal customer row and returns the ID.
// The 'free' plan must exist (seeded by 0001_init.sql).
func insertTestCustomer(t *testing.T, pool *pgxpool.Pool) uuid.UUID {
	t.Helper()
	id := uuid.New()
	_, err := pool.Exec(context.Background(), `
		INSERT INTO customers (id, email, plan_id)
		VALUES ($1, $2, 'free')
		ON CONFLICT DO NOTHING
	`, id, id.String()+"@idemptest.local")
	if err != nil {
		t.Fatalf("insert customer: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM idempotency_keys WHERE customer_id = $1`, id)
		_, _ = pool.Exec(context.Background(), `DELETE FROM customers WHERE id = $1`, id)
	})
	return id
}

// TestNewStore_NilPool returns nil, enabling pass-through mode.
func TestNewStore_NilPool(t *testing.T) {
	if s := NewStore(nil); s != nil {
		t.Error("NewStore(nil) must return nil")
	}
}

func TestStore_ClaimAndLoad(t *testing.T) {
	pool := newTestPool(t)
	applyMigrations(t, pool)
	customerID := insertTestCustomer(t, pool)

	s := NewStore(pool)
	ctx := context.Background()
	key := "test-key-" + uuid.New().String()
	fp := []byte("fingerprint-abc")

	// First claim: must succeed.
	claimed, err := s.Claim(ctx, customerID, key, fp)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if !claimed {
		t.Fatal("first Claim: expected true, got false")
	}

	// Second claim (same key): must not succeed.
	claimed2, err := s.Claim(ctx, customerID, key, fp)
	if err != nil {
		t.Fatalf("second Claim: %v", err)
	}
	if claimed2 {
		t.Fatal("second Claim on same key: expected false (conflict), got true")
	}

	// Load must return the pending entry (status_code IS NULL).
	entry, err := s.Load(ctx, customerID, key)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if entry == nil {
		t.Fatal("Load: expected entry, got nil")
	}
	if entry.StatusCode != nil {
		t.Errorf("Load: expected nil StatusCode (in-flight), got %v", *entry.StatusCode)
	}
	if string(entry.Fingerprint) != string(fp) {
		t.Errorf("Load: fingerprint mismatch: got %q, want %q", entry.Fingerprint, fp)
	}
}

func TestStore_Finalize(t *testing.T) {
	pool := newTestPool(t)
	applyMigrations(t, pool)
	customerID := insertTestCustomer(t, pool)

	s := NewStore(pool)
	ctx := context.Background()
	key := "finalize-key-" + uuid.New().String()
	fp := []byte("fp-finalize")
	body := []byte(`{"result":"ok","billable_units":1}`)

	if _, err := s.Claim(ctx, customerID, key, fp); err != nil {
		t.Fatalf("Claim: %v", err)
	}

	if err := s.Finalize(ctx, customerID, key, 200, body); err != nil {
		t.Fatalf("Finalize: %v", err)
	}

	entry, err := s.Load(ctx, customerID, key)
	if err != nil {
		t.Fatalf("Load after Finalize: %v", err)
	}
	if entry == nil {
		t.Fatal("Load after Finalize: expected entry, got nil")
	}
	if entry.StatusCode == nil {
		t.Fatal("Load after Finalize: StatusCode must not be nil")
	}
	if *entry.StatusCode != 200 {
		t.Errorf("Load: StatusCode = %d, want 200", *entry.StatusCode)
	}
	if string(entry.Body) != string(body) {
		t.Errorf("Load: Body mismatch")
	}
}

func TestStore_Release(t *testing.T) {
	pool := newTestPool(t)
	applyMigrations(t, pool)
	customerID := insertTestCustomer(t, pool)

	s := NewStore(pool)
	ctx := context.Background()
	key := "release-key-" + uuid.New().String()
	fp := []byte("fp-release")

	if _, err := s.Claim(ctx, customerID, key, fp); err != nil {
		t.Fatalf("Claim: %v", err)
	}

	if err := s.Release(ctx, customerID, key); err != nil {
		t.Fatalf("Release: %v", err)
	}

	// After Release, Load must return nil.
	entry, err := s.Load(ctx, customerID, key)
	if err != nil {
		t.Fatalf("Load after Release: %v", err)
	}
	if entry != nil {
		t.Fatal("Load after Release: expected nil (row deleted), got entry")
	}

	// After Release, Claim must succeed again (row is gone).
	claimed, err := s.Claim(ctx, customerID, key, fp)
	if err != nil {
		t.Fatalf("Claim after Release: %v", err)
	}
	if !claimed {
		t.Fatal("Claim after Release: expected true, got false")
	}
}

func TestStore_Load_Absent(t *testing.T) {
	pool := newTestPool(t)
	applyMigrations(t, pool)
	customerID := insertTestCustomer(t, pool)

	s := NewStore(pool)
	entry, err := s.Load(context.Background(), customerID, "nonexistent-key")
	if err != nil {
		t.Fatalf("Load of absent key: %v", err)
	}
	if entry != nil {
		t.Fatal("Load of absent key: expected nil, got entry")
	}
}

func TestStore_Load_TTLExpired(t *testing.T) {
	pool := newTestPool(t)
	applyMigrations(t, pool)
	customerID := insertTestCustomer(t, pool)

	// Insert a row with created_at in the past (beyond TTL).
	key := "expired-key-" + uuid.New().String()
	fp := []byte("fp-expired")
	_, err := pool.Exec(context.Background(), `
		INSERT INTO idempotency_keys (customer_id, idempotency_key, fingerprint, created_at)
		VALUES ($1, $2, $3, NOW() - INTERVAL '25 hours')
		ON CONFLICT DO NOTHING
	`, customerID, key, fp)
	if err != nil {
		t.Fatalf("insert expired row: %v", err)
	}

	s := &Store{db: pool, ttl: 24 * time.Hour}
	entry, err := s.Load(context.Background(), customerID, key)
	if err != nil {
		t.Fatalf("Load expired: %v", err)
	}
	if entry != nil {
		t.Fatal("Load expired: expected nil (expired row deleted), got entry")
	}

	// Claim must now succeed since the expired row was deleted.
	claimed, err := s.Claim(context.Background(), customerID, key, fp)
	if err != nil {
		t.Fatalf("Claim after expired load: %v", err)
	}
	if !claimed {
		t.Fatal("Claim after expired load: expected true, got false")
	}
}

func TestStore_DifferentCustomers_SameKey(t *testing.T) {
	pool := newTestPool(t)
	applyMigrations(t, pool)

	cid1 := insertTestCustomer(t, pool)
	cid2 := insertTestCustomer(t, pool)

	s := NewStore(pool)
	ctx := context.Background()
	sharedKey := "shared-key-" + uuid.New().String()
	fp := []byte("fp-shared")

	claimed1, err := s.Claim(ctx, cid1, sharedKey, fp)
	if err != nil || !claimed1 {
		t.Fatalf("cid1 Claim: err=%v, claimed=%v", err, claimed1)
	}
	// Different customer — must also be able to claim the same key string.
	claimed2, err := s.Claim(ctx, cid2, sharedKey, fp)
	if err != nil || !claimed2 {
		t.Fatalf("cid2 Claim: err=%v, claimed=%v", err, claimed2)
	}
}
