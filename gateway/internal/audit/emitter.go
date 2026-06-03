// Package audit writes append-only rows to the audit_log table.
// One shared emitter is used by the entire gateway; product workers never touch it.
package audit

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// ActorType restricts audit rows to the three values the 0001_init CHECK constraint allows.
type ActorType string

const (
	ActorCustomer ActorType = "customer"
	ActorAdmin    ActorType = "admin"
	ActorSystem   ActorType = "system"
)

// Event is a single audit-log entry. Fields mirror audit_log exactly.
// Field set must stay identical with dashboard/lib/audit.ts.
type Event struct {
	ActorType  ActorType
	ActorID    string
	Action     string         // e.g. "api_key.created", "api_key.revoked", "plan.changed"
	TargetType string         // e.g. "api_key", "customer"
	TargetID   string         // UUID or other stable identifier
	Details    map[string]any // optional freeform context; stored as JSONB
}

// nullStr converts an empty string to nil so pgx inserts SQL NULL for optional
// fields, matching the TS emitter's `?? null` semantics for unset fields.
func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// Emit writes one append-only row to audit_log.
// ActorType is validated here (fail fast) and also enforced by the Postgres CHECK constraint.
func Emit(ctx context.Context, db *pgxpool.Pool, e Event) error {
	if e.ActorType != ActorCustomer && e.ActorType != ActorAdmin && e.ActorType != ActorSystem {
		return fmt.Errorf("audit: invalid actor_type %q: must be customer|admin|system", e.ActorType)
	}
	if e.Action == "" {
		return fmt.Errorf("audit: action must not be empty")
	}
	var detailsJSON []byte
	if e.Details != nil {
		b, err := json.Marshal(e.Details)
		if err != nil {
			return fmt.Errorf("audit: marshal details: %w", err)
		}
		detailsJSON = b
	}
	_, err := db.Exec(ctx, `
		INSERT INTO audit_log (actor_type, actor_id, action, target_type, target_id, details)
		VALUES ($1, $2, $3, $4, $5, $6)
	`, string(e.ActorType), e.ActorID, e.Action, nullStr(e.TargetType), nullStr(e.TargetID), detailsJSON)
	if err != nil {
		return fmt.Errorf("audit: insert: %w", err)
	}
	return nil
}
