// Package audit writes append-only rows to the audit_log table.
// One shared emitter is used by the entire gateway; product workers never touch it.
package audit

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
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

// allowedDetailKeys mirrors dashboard/lib/audit.ts ALLOWED_DETAIL_KEYS.
// Both sides must stay in sync — add keys here and there together.
var allowedDetailKeys = map[string]bool{
	"name": true, "prefix": true, "plan_id": true, "attempt": true,
}

// sanitizeDetails redacts map entries whose key is not in allowedDetailKeys, and
// replaces complex-typed values (maps, slices, channels, structs) with "[REDACTED:complex]"
// even for allowed keys — preventing nested objects from leaking secrets or PII into JSONB.
// Mirrors the TypeScript sanitizeDetails in dashboard/lib/audit.ts.
func sanitizeDetails(details map[string]any) map[string]any {
	sanitized := make(map[string]any, len(details))
	for k, v := range details {
		if !allowedDetailKeys[k] {
			sanitized[k] = "[REDACTED]"
			continue
		}
		switch v.(type) {
		case string, bool,
			int, int8, int16, int32, int64,
			uint, uint8, uint16, uint32, uint64, uintptr,
			float32, float64, nil:
			sanitized[k] = v
		default:
			// All other types — maps, slices, structs, channels, funcs, pointers —
			// are redacted to prevent nested or non-serialisable values from leaking
			// secrets or PII into the JSONB audit trail.
			sanitized[k] = "[REDACTED:complex]"
		}
	}
	return sanitized
}

// execer is the subset of *pgxpool.Pool and pgx.Tx that Emit/EmitTx need —
// extracted so the same insert logic runs against either a pool (Emit) or an
// existing caller transaction (EmitTx).
type execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// Emit writes one append-only row to audit_log.
// ActorType is validated here (fail fast) and also enforced by the Postgres CHECK constraint.
// Validation order: invalid type → empty action → non-system empty ID → system non-empty ID.
// The TypeScript mirror (dashboard/lib/audit.ts) applies the same rules in the same order but
// logs-and-swallows instead of returning errors. Keep both sides in sync when modifying.
func Emit(ctx context.Context, db *pgxpool.Pool, e Event) error {
	if db == nil {
		return fmt.Errorf("audit: db pool is nil")
	}
	return emit(ctx, db, e)
}

// EmitTx writes one append-only row to audit_log on the caller's existing
// transaction tx, instead of a fresh pool connection — so the audit row
// commits or rolls back atomically with whatever state change tx also
// contains. Validation is identical to Emit; see its doc comment.
func EmitTx(ctx context.Context, tx pgx.Tx, e Event) error {
	if tx == nil {
		return fmt.Errorf("audit: tx is nil")
	}
	return emit(ctx, tx, e)
}

func emit(ctx context.Context, x execer, e Event) error {
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
		b, err := json.Marshal(sanitizeDetails(e.Details))
		if err != nil {
			return fmt.Errorf("audit: details not serializable: %w", err)
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
	const insertSQL = `INSERT INTO audit_log (actor_type, actor_id, action, target_type, target_id, details) VALUES ($1, $2, $3, $4, $5, $6::jsonb)`
	_, err := x.Exec(auditCtx, insertSQL,
		string(e.ActorType), actorIDParam, e.Action,
		e.TargetType, e.TargetID, detailsJSON)
	if err != nil {
		return fmt.Errorf("audit: insert failed: %w", err)
	}
	return nil
}
