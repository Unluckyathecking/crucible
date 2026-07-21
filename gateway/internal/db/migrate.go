package db

import (
	"context"
	"fmt"
	"io/fs"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/zerolog/log"

	"github.com/Unluckyathecking/crucible/gateway/migrations"
)

// migrationLockKey is a fixed advisory-lock key ("crucible" as 8 ASCII bytes)
// that serializes Apply across connections and processes. Two gateway replicas
// booting at once — or parallel test binaries sharing a database — otherwise race
// the CREATE EXTENSION / CREATE INDEX IF NOT EXISTS statements into a pg_class or
// pg_extension duplicate-key error (SQLSTATE 23505): both sessions observe the
// object as absent, both insert, one loses. The lock lets one Apply run the full
// set while the rest wait and then no-op through the idempotent files.
const migrationLockKey int64 = 0x6372756369626c65

// Apply runs every .sql file under gateway/migrations in lexical order.
// Files must be idempotent (CREATE TABLE IF NOT EXISTS, ON CONFLICT DO NOTHING).
// Per-product clones extend by adding 0002_seed_plans.sql etc — they run automatically on next boot.
//
// All files run on a single connection while a session-level advisory lock is
// held. The lock is session-scoped rather than a wrapping transaction because
// several files manage their own BEGIN/COMMIT (e.g. 0022_async_jobs_cancel.sql),
// which one outer transaction would break.
func Apply(ctx context.Context, pool *pgxpool.Pool) error {
	entries, err := migrations.FS.ReadDir(".")
	if err != nil {
		return fmt.Errorf("read migrations dir: %w", err)
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".sql") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)

	conn, err := pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("acquire migration connection: %w", err)
	}
	defer conn.Release()

	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock($1)", migrationLockKey); err != nil {
		return fmt.Errorf("acquire migration advisory lock: %w", err)
	}
	defer func() {
		// Unlock on a fresh context so a cancelled caller ctx still releases the
		// lock — a leaked session lock would block the next Apply on this pooled
		// connection, which pgxpool does not reset on release.
		unlockCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := conn.Exec(unlockCtx, "SELECT pg_advisory_unlock($1)", migrationLockKey); err != nil {
			log.Warn().Err(err).Msg("release migration advisory lock")
		}
	}()

	for _, name := range names {
		sql, err := fs.ReadFile(migrations.FS, name)
		if err != nil {
			return fmt.Errorf("read %s: %w", name, err)
		}
		log.Info().Str("migration", name).Msg("applying migration")
		if _, err := conn.Exec(ctx, string(sql)); err != nil {
			return fmt.Errorf("apply %s: %w", name, err)
		}
	}
	return nil
}
