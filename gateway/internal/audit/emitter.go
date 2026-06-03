// Package audit writes append-only rows to the audit_log table.
// One shared emitter is used by the entire gateway; product workers never touch it.
package audit

import (
	"context"
	"encoding/json"
	"errors"
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

// nullOrString returns nil when s is the zero value, mapping to SQL NULL.
// Optional audit fields (actor_id, target_type, target_id) use Go's string zero value to
// mean "absent"; pgx maps nil *string to NULL, preserving that semantic in Postgres.
func nullOrString(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// Emit writes one append-only row to audit_log.
// ActorType is validated here (fail fast) and also enforced by the Postgres CHECK constraint.
// Optional fields (ActorID, TargetType, TargetID) become SQL NULL when left as zero values,
// consistent with the TS emitter which maps undefined → null.
func Emit(ctx context.Context, db *pgxpool.Pool, e Event) error {
	if e.ActorType != ActorCustomer && e.ActorType != ActorAdmin && e.ActorType != ActorSystem {
		return errors.New("audit: actor_type must be customer|admin|system")
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
	`, string(e.ActorType), nullOrString(e.ActorID), e.Action, nullOrString(e.TargetType), nullOrString(e.TargetID), detailsJSON)
	if err != nil {
		return fmt.Errorf("audit: insert: %w", err)
	}
	return nil
}
