package db

import (
	"context"
	"fmt"
	"io/fs"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/zerolog/log"

	"github.com/Unluckyathecking/crucible/gateway/migrations"
)

// Apply runs every .sql file under gateway/migrations in lexical order.
// Files must be idempotent (CREATE TABLE IF NOT EXISTS, ON CONFLICT DO NOTHING).
// Per-product clones extend by adding 0002_seed_plans.sql etc — they run automatically on next boot.
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
	for _, name := range names {
		sql, err := fs.ReadFile(migrations.FS, name)
		if err != nil {
			return fmt.Errorf("read %s: %w", name, err)
		}
		log.Info().Str("migration", name).Msg("applying migration")
		if _, err := pool.Exec(ctx, string(sql)); err != nil {
			return fmt.Errorf("apply %s: %w", name, err)
		}
	}
	return nil
}
