// Package testdb provisions a dedicated, migrated crucible_test database so the
// gateway's integration tests never run against the live crucible dev database.
// Rows stranded there by killed test runs — a billable_units=MaxInt64 poison row
// that overflowed the reconcile SUM, plus accumulated webhook_deliveries that
// broke row-count assertions — otherwise leak into every run. Tests point their
// default DSN at DSN(t); an explicit POSTGRES_DSN/TEST_DATABASE_URL still wins.
package testdb

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Unluckyathecking/crucible/gateway/internal/db"
)

const (
	// DSNString is the connection string for the dedicated test database.
	DSNString = "postgres://crucible@localhost:5432/crucible_test?sslmode=disable"

	// maintenanceDSN connects to the default postgres database, which always
	// exists, so crucible_test can be created when absent.
	maintenanceDSN = "postgres://crucible@localhost:5432/postgres?sslmode=disable"

	testDatabaseName = "crucible_test"

	// duplicateDatabase is the SQLSTATE returned by CREATE DATABASE when the
	// target already exists — expected when another test binary won the race.
	duplicateDatabase = "42P04"
)

var (
	ensureOnce  sync.Once
	ensureErr   error
	unreachable bool
)

// DSN ensures crucible_test exists and is migrated, then returns its DSN. The
// provisioning runs once per test binary. If Postgres is unreachable the calling
// test is skipped (matching the per-package convention); a real provisioning
// failure fails the test.
func DSN(t testing.TB) string {
	t.Helper()
	ensureOnce.Do(ensure)
	if ensureErr != nil {
		if unreachable {
			t.Skipf("postgres unavailable, skipping: %v", ensureErr)
		}
		t.Fatalf("prepare %s database: %v", testDatabaseName, ensureErr)
	}
	return DSNString
}

func ensure() {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	admin, err := pgxpool.New(ctx, maintenanceDSN)
	if err != nil {
		ensureErr, unreachable = err, true
		return
	}
	defer admin.Close()
	if err := admin.Ping(ctx); err != nil {
		ensureErr, unreachable = err, true
		return
	}

	// CREATE DATABASE has no IF NOT EXISTS and cannot run in a transaction;
	// ignore duplicate-database so concurrent binaries race harmlessly.
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+testDatabaseName); err != nil {
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != duplicateDatabase {
			ensureErr = err
			return
		}
	}

	pool, err := pgxpool.New(ctx, DSNString)
	if err != nil {
		ensureErr = err
		return
	}
	defer pool.Close()

	// db.Apply serializes with a cross-process advisory lock, so concurrent
	// binaries migrating crucible_test at once is safe.
	if err := db.Apply(ctx, pool); err != nil {
		ensureErr = err
	}
}
