package db_test

import (
	"context"
	"fmt"
	"math/rand"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Unluckyathecking/crucible/gateway/internal/db"
)

const mainDSN = "postgres://crucible@localhost:5432/crucible?sslmode=disable"

// adminDSN connects to the postgres database so we can CREATE / DROP databases.
const adminDSN = "postgres://crucible@localhost:5432/postgres?sslmode=disable"

// newTestPostgres opens a pool to the local Postgres instance or skips the test
// if Postgres is unreachable. Mirrors the same pattern in internal/auth/store_test.go.
func newTestPostgres(t *testing.T, dsn string) *pgxpool.Pool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Skipf("postgres unavailable (%s), skipping: %v", dsn, err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Skipf("postgres ping failed (%s), skipping: %v", dsn, err)
	}
	return pool
}

// TestNewPool_ConnectsToPostgres verifies that NewPool opens a real connection
// and pings the server successfully.
func TestNewPool_ConnectsToPostgres(t *testing.T) {
	// Guard: skip if Postgres is not reachable in this environment.
	func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		pool, err := pgxpool.New(ctx, mainDSN)
		if err != nil {
			t.Skipf("postgres unavailable, skipping: %v", err)
		}
		if err := pool.Ping(ctx); err != nil {
			pool.Close()
			t.Skipf("postgres ping failed, skipping: %v", err)
		}
		pool.Close()
	}()

	ctx := context.Background()
	pool, err := db.NewPool(ctx, mainDSN, 2)
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	defer pool.Close()

	if err := pool.Ping(ctx); err != nil {
		t.Fatalf("Ping after NewPool: %v", err)
	}
}

// TestNewPool_BadDSN verifies that an invalid DSN returns an error rather than panicking.
// This test does not require Postgres — it uses a port that is never open (9999).
func TestNewPool_BadDSN(t *testing.T) {
	ctx := context.Background()
	_, err := db.NewPool(ctx, "postgres://nobody@localhost:9999/nodbhere?sslmode=disable&connect_timeout=1", 1)
	if err == nil {
		t.Fatal("expected error for bad DSN, got nil")
	}
}

// TestApply_Idempotent creates a throwaway database, runs Apply twice, and
// asserts that neither run returns an error. This exercises INVARIANT #8:
// every migration uses CREATE TABLE IF NOT EXISTS / ON CONFLICT DO NOTHING etc.
func TestApply_Idempotent(t *testing.T) {
	ctx := context.Background()

	adminPool := newTestPostgres(t, adminDSN)

	testDB := fmt.Sprintf("crucible_test_%d", rand.Intn(1_000_000))

	// CREATE DATABASE cannot run inside a transaction, so use Exec directly.
	if _, err := adminPool.Exec(ctx, fmt.Sprintf("CREATE DATABASE %s", testDB)); err != nil {
		adminPool.Close()
		t.Fatalf("create test db: %v", err)
	}

	testDSN := fmt.Sprintf("postgres://crucible@localhost:5432/%s?sslmode=disable", testDB)
	testPool := newTestPostgres(t, testDSN)
	t.Cleanup(func() {
		testPool.Close()
		// Terminate any remaining connections before dropping.
		_, _ = adminPool.Exec(ctx, fmt.Sprintf(
			"SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = '%s'", testDB,
		))
		if _, err := adminPool.Exec(ctx, fmt.Sprintf("DROP DATABASE IF EXISTS %s", testDB)); err != nil {
			t.Logf("warn: drop test db: %v", err)
		}
		adminPool.Close()
	})

	// First run.
	if err := db.Apply(ctx, testPool); err != nil {
		t.Fatalf("Apply (first run): %v", err)
	}

	// Second run — must succeed without error (idempotency).
	if err := db.Apply(ctx, testPool); err != nil {
		t.Fatalf("Apply (second run, idempotency check): %v", err)
	}
}

// TestApply_CreatesExpectedSchema verifies that Apply actually executes the
// migrations in the correct order by checking that tables created by early
// migrations are present after Apply runs on a fresh database.
//
// This replaces the previous TestApply_LexicalOrder which only tested that
// embed.FS.ReadDir returns sorted entries (a stdlib property, not our code).
// INVARIANT #8 coverage is preserved: we confirm Apply runs all migrations
// and that the schema produced by them is correct.
func TestApply_CreatesExpectedSchema(t *testing.T) {
	ctx := context.Background()

	adminPool := newTestPostgres(t, adminDSN)

	testDB := fmt.Sprintf("crucible_schema_test_%d", rand.Intn(1_000_000))

	if _, err := adminPool.Exec(ctx, fmt.Sprintf("CREATE DATABASE %s", testDB)); err != nil {
		adminPool.Close()
		t.Fatalf("create test db: %v", err)
	}

	testDSN := fmt.Sprintf("postgres://crucible@localhost:5432/%s?sslmode=disable", testDB)
	testPool := newTestPostgres(t, testDSN)
	t.Cleanup(func() {
		testPool.Close()
		_, _ = adminPool.Exec(ctx, fmt.Sprintf(
			"SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = '%s'", testDB,
		))
		if _, err := adminPool.Exec(ctx, fmt.Sprintf("DROP DATABASE IF EXISTS %s", testDB)); err != nil {
			t.Logf("warn: drop test db: %v", err)
		}
		adminPool.Close()
	})

	if err := db.Apply(ctx, testPool); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	// Assert that tables created by the first migration (0001_init.sql) exist.
	// If Apply had run migrations out of order or skipped any, later migrations
	// that reference these tables via FK would have failed.
	tables := []string{"plans", "customers", "api_keys"}
	for _, table := range tables {
		var exists bool
		err := testPool.QueryRow(ctx,
			"SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_schema='public' AND table_name=$1)",
			table,
		).Scan(&exists)
		if err != nil {
			t.Fatalf("check table %q exists: %v", table, err)
		}
		if !exists {
			t.Errorf("table %q missing after Apply — migration ordering or execution is broken", table)
		}
	}

	// Also assert that the batch_id column on usage_events (added by 0004_usage_batches.sql)
	// exists, confirming that all four migrations ran, not just the first.
	var batchIDExists bool
	err := testPool.QueryRow(ctx,
		"SELECT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_schema='public' AND table_name='usage_events' AND column_name='batch_id')",
	).Scan(&batchIDExists)
	if err != nil {
		t.Fatalf("check usage_events.batch_id exists: %v", err)
	}
	if !batchIDExists {
		t.Error("column 'usage_events.batch_id' missing after Apply — 0004_usage_batches.sql did not run")
	}
}
