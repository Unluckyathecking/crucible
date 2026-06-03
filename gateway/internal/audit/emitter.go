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
// TargetType and TargetID are optional (*string nil → SQL NULL) to match the TS
// interface's optional targetType?/targetId? fields; pgx maps nil *string to NULL.
type Event struct {
	ActorType  ActorType
	ActorID    string
	Action     string         // e.g. "api_key.created", "api_key.revoked", "plan.changed"
	TargetType *string        // optional: nil → SQL NULL (e.g. "api_key", "customer")
	TargetID   *string        // optional: nil → SQL NULL (UUID or other stable identifier)
	Details    map[string]any // optional freeform context; stored as JSONB
}

// nullActorID converts the ActorID to nil only for system events, so pgx inserts
// SQL NULL for background jobs that have no individual actor. Non-system events are
// validated to have a non-empty ActorID before reaching SQL, but this function makes
// the intent explicit rather than relying on that invariant implicitly.
func nullActorID(actorType ActorType, id string) any {
	if actorType == ActorSystem && id == "" {
		return nil
	}
	return id
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
	// System events originate from background jobs with no individual actor; customer and
	// admin events must always carry an ActorID so audit rows remain attributable.
	if e.ActorType != ActorSystem && e.ActorID == "" {
		return fmt.Errorf("audit: actor_id must not be empty for actor_type %q", e.ActorType)
	}
	var detailsJSON []byte
	if e.Details != nil {
		b, err := json.Marshal(e.Details)
		if err != nil {
			return fmt.Errorf("audit: marshal details: %w", err)
		}
		detailsJSON = b
	}
	// insertSQL is a package-level constant: column names and parameter slots are
	// fixed at compile time, never constructed from user input or runtime data.
	const insertSQL = `INSERT INTO audit_log (actor_type, actor_id, action, target_type, target_id, details) VALUES ($1, $2, $3, $4, $5, $6)`
	_, err := db.Exec(ctx, insertSQL,
		string(e.ActorType), nullActorID(e.ActorType, e.ActorID), e.Action,
		e.TargetType, e.TargetID, detailsJSON)
	if err != nil {
		return fmt.Errorf("audit: insert: %w", err)
	}
	return nil
}
