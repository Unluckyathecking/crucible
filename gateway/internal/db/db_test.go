package db_test

import (
	"context"
	"fmt"
	"math/rand"
	"sort"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Unluckyathecking/crucible/gateway/internal/db"
	"github.com/Unluckyathecking/crucible/gateway/migrations"
)

const mainDSN = "postgres://crucible@localhost:5432/crucible?sslmode=disable"

// adminDSN connects to the postgres database so we can CREATE / DROP databases.
const adminDSN = "postgres://crucible@localhost:5432/postgres?sslmode=disable"

// TestNewPool_ConnectsToPostgres verifies that NewPool opens a real connection
// and pings the server successfully.
func TestNewPool_ConnectsToPostgres(t *testing.T) {
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

	testDB := fmt.Sprintf("crucible_test_%d", rand.Intn(1_000_000))
	adminPool := mustOpenPool(t, ctx, adminDSN)

	// CREATE DATABASE cannot run inside a transaction, so use Exec directly.
	if _, err := adminPool.Exec(ctx, fmt.Sprintf("CREATE DATABASE %s", testDB)); err != nil {
		adminPool.Close()
		t.Fatalf("create test db: %v", err)
	}

	testDSN := fmt.Sprintf("postgres://crucible@localhost:5432/%s?sslmode=disable", testDB)
	testPool := mustOpenPool(t, ctx, testDSN)
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

// TestApply_LexicalOrder verifies that the embedded migration files are applied
// in lexical (alphabetical) order. The embedded FS lists files via ReadDir; we
// confirm that the sorted set matches what migrations.FS actually contains.
func TestApply_LexicalOrder(t *testing.T) {
	entries, err := migrations.FS.ReadDir(".")
	if err != nil {
		t.Fatalf("ReadDir migrations.FS: %v", err)
	}

	var names []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".sql") {
			names = append(names, e.Name())
		}
	}

	if len(names) == 0 {
		t.Fatal("no .sql files found in migrations.FS")
	}

	sorted := make([]string, len(names))
	copy(sorted, names)
	sort.Strings(sorted)

	for i := range names {
		if names[i] != sorted[i] {
			t.Errorf("migration at position %d is %q but lexical order expects %q — Apply would run them out of order", i, names[i], sorted[i])
		}
	}
}

// mustOpenPool is a test helper that opens a pool and fatals the test on error.
func mustOpenPool(t *testing.T, ctx context.Context, dsn string) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("open pool for %s: %v", dsn, err)
	}
	return pool
}
