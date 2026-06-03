// Package audit writes append-only rows to the audit_log table.
// One shared emitter is used by the entire gateway; product workers never touch it.
package audit

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// ActorType restricts audit rows to the three values the 0001_init CHECK constraint allows.
type ActorType string

const (
	ActorCustomer ActorType = "customer"
	ActorAdmin    ActorType = "admin"
	ActorSystem   ActorType = "system"
)

// defaultInsertTimeout caps the audit INSERT so a slow Postgres cannot stall callers.
const defaultInsertTimeout = 2 * time.Second

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

// Emit writes one append-only row to audit_log.
// ActorType is validated here (fail fast) and also enforced by the Postgres CHECK constraint.
func Emit(ctx context.Context, db *pgxpool.Pool, e Event) error {
	if e.ActorType != ActorCustomer && e.ActorType != ActorAdmin && e.ActorType != ActorSystem {
		return fmt.Errorf("audit: invalid actor_type %q: must be customer|admin|system", e.ActorType)
	}
	if e.Action == "" {
		return fmt.Errorf("audit: action must not be empty (got %q)", e.Action)
	}
	// System events originate from background jobs with no individual actor; customer and
	// admin events must always carry an ActorID so audit rows remain attributable.
	if e.ActorType != ActorSystem && e.ActorID == "" {
		return fmt.Errorf("audit: actor_id must not be empty for actor_type %q", e.ActorType)
	}
	// Symmetric: system events must NOT carry an ActorID; a non-empty ActorID on a
	// system event indicates a caller bug (e.g. passing a customer ID to a background job).
	if e.ActorType == ActorSystem && e.ActorID != "" {
		return fmt.Errorf("audit: actor_id must be empty for actor_type %q", e.ActorType)
	}
	var detailsJSON []byte
	if e.Details != nil {
		b, err := json.Marshal(e.Details)
		if err != nil {
			return fmt.Errorf("audit: details not serializable")
		}
		detailsJSON = b
	}
	// actor_id is NULL for system events; validation above ensures non-system events always
	// carry a non-empty ActorID. Using a typed *string (not any) lets pgx map (*string)(nil)
	// to SQL NULL without a typed pgtype sentinel.
	actorIDParam := (*string)(nil)
	if e.ActorType != ActorSystem {
		s := e.ActorID
		actorIDParam = &s
	}
	// Cap the INSERT at 2 s so a slow or unresponsive Postgres cannot stall the caller.
	// Callers using fire-and-forget (goroutine + context.Background) won't be affected,
	// but request-path callers are protected from audit logging blocking the response.
	// auditCtx avoids shadowing the caller's ctx; the caller's context is still respected
	// as the parent of the timeout, but the local name is distinct for clarity.
	auditCtx, cancel := context.WithTimeout(ctx, defaultInsertTimeout)
	defer cancel()
	// insertSQL is a package-level constant: column names and parameter slots are
	// fixed at compile time, never constructed from user input or runtime data.
	const insertSQL = `INSERT INTO audit_log (actor_type, actor_id, action, target_type, target_id, details) VALUES ($1, $2, $3, $4, $5, $6)`
	_, err := db.Exec(auditCtx, insertSQL,
		string(e.ActorType), actorIDParam, e.Action,
		e.TargetType, e.TargetID, detailsJSON)
	if err != nil {
		return fmt.Errorf("audit: insert failed")
	}
	return nil
}
