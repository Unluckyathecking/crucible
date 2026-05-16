// Package migrations exposes the gateway's SQL migrations as an embedded fs.FS.
//
// Per-product clones extend this directory by adding 0002_seed_plans.sql,
// 0003_..., etc. The Apply() function in gateway/internal/db runs them in
// lexical order on boot. Each file must be idempotent (CREATE TABLE IF NOT EXISTS,
// INSERT ... ON CONFLICT DO NOTHING) — there is no separate version-tracking table.
package migrations

import "embed"

//go:embed *.sql
var FS embed.FS
