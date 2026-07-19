package audit_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Unluckyathecking/crucible/gateway/internal/audit"
)

// strPtr returns a pointer to s, for constructing optional *string Event fields inline.
func strPtr(s string) *string { return &s }

// newTestPostgres returns a pgxpool connected to the local Postgres instance or
// skips the test if unreachable. Mirrors the helper in gateway/internal/auth.
// If TEST_DATABASE_URL is explicitly set, connection failures are fatal (not skip)
// so CI configuration errors surface immediately rather than silently passing.
func newTestPostgres(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	explicit := dsn != ""
	if !explicit {
		dsn = "postgres://crucible@localhost:5432/crucible?sslmode=disable"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		if explicit {
			t.Fatalf("TEST_DATABASE_URL set but postgres unavailable: %v", err)
		}
		t.Skipf("postgres unavailable, skipping: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		if explicit {
			t.Fatalf("TEST_DATABASE_URL set but postgres ping failed: %v", err)
		}
		t.Skipf("postgres ping failed, skipping: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func TestEmit_RejectsInvalidActorType(t *testing.T) {
	// nil pool is safe here: actor_type validation returns an error before Emit
	// touches db, so no DB call is ever issued. If Emit is refactored to query
	// db before validating, this test will panic and must be updated.
	err := audit.Emit(context.Background(), nil, audit.Event{
		ActorType: "invalid",
		Action:    "test.action",
	})
	if err == nil {
		t.Fatal("expected error for invalid actor_type, got nil")
	}
}

func TestEmit_RejectsEmptyAction(t *testing.T) {
	// nil pool is safe: action validation fires before any DB call.
	err := audit.Emit(context.Background(), nil, audit.Event{
		ActorType: audit.ActorCustomer,
		ActorID:   "cust-1",
		Action:    "",
	})
	if err == nil {
		t.Fatal("expected error for empty action, got nil")
	}
}

func TestEmit_RejectsEmptyActorIDForNonSystem(t *testing.T) {
	// nil pool is safe: actor_id validation fires before any DB call.
	for _, at := range []audit.ActorType{audit.ActorCustomer, audit.ActorAdmin} {
		t.Run(string(at), func(t *testing.T) {
			err := audit.Emit(context.Background(), nil, audit.Event{
				ActorType: at,
				ActorID:   "",
				Action:    "test.action",
			})
			if err == nil {
				t.Fatalf("expected error for empty actor_id with actor_type %q, got nil", at)
			}
		})
	}
}

func TestEmit_NonSerializableDetailsAreRedacted(t *testing.T) {
	// sanitizeDetails converts non-allowed keys and complex-typed values to "[REDACTED]"
	// or "[REDACTED:complex]" before json.Marshal, so non-serializable values never cause
	// a marshal error. The emit proceeds to the DB call and completes successfully.
	db := newTestPostgres(t)
	ctx := context.Background()
	uniqueAction := fmt.Sprintf("test.complex-details.%s", t.Name())
	err := audit.Emit(ctx, db, audit.Event{
		ActorType: audit.ActorCustomer,
		ActorID:   fmt.Sprintf("cust-%s", t.Name()),
		Action:    uniqueAction,
		Details:   map[string]any{"bad": make(chan int), "name": make(chan int)},
	})
	if err != nil {
		t.Fatalf("complex/non-allowed detail values should be redacted, not error: %v", err)
	}
}

func TestEmit_RoundTrip(t *testing.T) {
	db := newTestPostgres(t)
	ctx := context.Background()

	uniqueActorID := fmt.Sprintf("test-cust-%s", t.Name())

	e := audit.Event{
		ActorType:  audit.ActorCustomer,
		ActorID:    uniqueActorID,
		Action:     "api_key.created",
		TargetType: strPtr("api_key"),
		TargetID:   strPtr("test-key-id"),
		Details:    map[string]any{"name": "integration-test", "attempt": 1},
	}

	if err := audit.Emit(ctx, db, e); err != nil {
		t.Fatalf("Emit: %v", err)
	}

	var (
		actorType   string
		actorID     string
		action      string
		targetType  string
		targetID    string
		detailsJSON []byte
		createdAt   time.Time
	)
	beforeEmit := time.Now().Add(-time.Second)
	err := db.QueryRow(ctx, `
		SELECT actor_type, actor_id, action, target_type, target_id, details, created_at
		FROM audit_log
		WHERE actor_id = $1 AND action = $2
		ORDER BY id DESC LIMIT 1
	`, uniqueActorID, e.Action).Scan(&actorType, &actorID, &action, &targetType, &targetID, &detailsJSON, &createdAt)
	if err != nil {
		t.Fatalf("query round-trip row: %v", err)
	}

	if actorType != string(e.ActorType) {
		t.Errorf("actor_type = %q, want %q", actorType, e.ActorType)
	}
	if actorID != e.ActorID {
		t.Errorf("actor_id = %q, want %q", actorID, e.ActorID)
	}
	if action != e.Action {
		t.Errorf("action = %q, want %q", action, e.Action)
	}
	if e.TargetType != nil && targetType != *e.TargetType {
		t.Errorf("target_type = %q, want %q", targetType, *e.TargetType)
	}
	if e.TargetID != nil && targetID != *e.TargetID {
		t.Errorf("target_id = %q, want %q", targetID, *e.TargetID)
	}

	// Verify the Details JSONB round-trip.
	var gotDetails map[string]any
	if err := json.Unmarshal(detailsJSON, &gotDetails); err != nil {
		t.Fatalf("unmarshal details: %v", err)
	}
	if gotDetails["name"] != e.Details["name"] {
		t.Errorf("details.name = %v, want %v", gotDetails["name"], e.Details["name"])
	}
	// JSON numbers decode as float64.
	if gotDetails["attempt"] != float64(1) {
		t.Errorf("details.attempt = %v, want 1", gotDetails["attempt"])
	}

	// Verify created_at was set to a recent timestamp (within the last minute).
	if createdAt.IsZero() {
		t.Error("created_at is zero")
	}
	if createdAt.Before(beforeEmit) {
		t.Errorf("created_at %v is before test start %v", createdAt, beforeEmit)
	}
	if time.Since(createdAt) > time.Minute {
		t.Errorf("created_at %v is older than 1 minute", createdAt)
	}
}

func TestEmit_AllActorTypes(t *testing.T) {
	db := newTestPostgres(t)
	ctx := context.Background()

	// Customer and admin events carry a non-empty ActorID.
	for i, at := range []audit.ActorType{audit.ActorCustomer, audit.ActorAdmin} {
		t.Run(string(at), func(t *testing.T) {
			err := audit.Emit(ctx, db, audit.Event{
				ActorType:  at,
				ActorID:    fmt.Sprintf("actor-%s-%d", t.Name(), i),
				Action:     "test.event",
				TargetType: strPtr("resource"),
				TargetID:   strPtr("resource-id"),
			})
			if err != nil {
				t.Errorf("Emit with actor_type=%q: %v", at, err)
			}
		})
	}

	// System events must have empty ActorID (background jobs have no individual actor).
	t.Run(string(audit.ActorSystem), func(t *testing.T) {
		err := audit.Emit(ctx, db, audit.Event{
			ActorType:  audit.ActorSystem,
			ActorID:    "",
			Action:     "test.event",
			TargetType: strPtr("resource"),
			TargetID:   strPtr("resource-id"),
		})
		if err != nil {
			t.Errorf("Emit with actor_type=%q and empty actor_id: %v", audit.ActorSystem, err)
		}
	})
}

func TestEmit_RejectsNonEmptyActorIDForSystem(t *testing.T) {
	// nil pool is safe: validation fires before any DB call.
	err := audit.Emit(context.Background(), nil, audit.Event{
		ActorType: audit.ActorSystem,
		ActorID:   "should-not-have-id",
		Action:    "system.test",
	})
	if err == nil {
		t.Fatal("expected error for non-empty actor_id with actor_type system, got nil")
	}
}

func TestEmit_SystemWithEmptyActorID(t *testing.T) {
	// System events are emitted by background jobs with no individual actor.
	// Validation must accept ActorSystem with an empty ActorID, and the DB row
	// must store NULL (not empty string) for actor_id.
	db := newTestPostgres(t)
	ctx := context.Background()
	uniqueAction := fmt.Sprintf("system.test.%s", t.Name())
	err := audit.Emit(ctx, db, audit.Event{
		ActorType: audit.ActorSystem,
		ActorID:   "",
		Action:    uniqueAction,
	})
	if err != nil {
		t.Fatalf("ActorSystem with empty actor_id should be accepted: %v", err)
	}
	var actorID *string
	err = db.QueryRow(ctx,
		`SELECT actor_id FROM audit_log WHERE action = $1 ORDER BY id DESC LIMIT 1`,
		uniqueAction,
	).Scan(&actorID)
	if err != nil {
		t.Fatalf("query round-trip row: %v", err)
	}
	if actorID != nil {
		t.Fatalf("expected NULL actor_id for system event, got %q", *actorID)
	}
}

func TestEmit_NilTargetTypeAndID(t *testing.T) {
	db := newTestPostgres(t)
	ctx := context.Background()

	uniqueAction := fmt.Sprintf("test.nil-target.%s", t.Name())

	if err := audit.Emit(ctx, db, audit.Event{
		ActorType: audit.ActorCustomer,
		ActorID:   "test-cust-nil-target",
		Action:    uniqueAction,
		// TargetType and TargetID intentionally omitted — both must be stored as SQL NULL.
	}); err != nil {
		t.Fatalf("Emit with nil TargetType/TargetID: %v", err)
	}

	var targetType, targetID *string
	err := db.QueryRow(ctx,
		`SELECT target_type, target_id FROM audit_log WHERE action = $1 ORDER BY id DESC LIMIT 1`,
		uniqueAction,
	).Scan(&targetType, &targetID)
	if err != nil {
		t.Fatalf("query round-trip row: %v", err)
	}
	if targetType != nil {
		t.Fatalf("expected NULL target_type, got %q", *targetType)
	}
	if targetID != nil {
		t.Fatalf("expected NULL target_id, got %q", *targetID)
	}
}

// TestEmitTx_RollbackLeavesNoRow proves EmitTx's core atomicity guarantee:
// an audit row inserted on a caller transaction that is then rolled back
// never becomes visible, exactly as if EmitTx had never been called.
func TestEmitTx_RollbackLeavesNoRow(t *testing.T) {
	db := newTestPostgres(t)
	ctx := context.Background()
	uniqueAction := fmt.Sprintf("test.tx-rollback.%s", t.Name())

	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	if err := audit.EmitTx(ctx, tx, audit.Event{
		ActorType: audit.ActorCustomer,
		ActorID:   fmt.Sprintf("cust-%s", t.Name()),
		Action:    uniqueAction,
	}); err != nil {
		t.Fatalf("EmitTx: %v", err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("rollback: %v", err)
	}

	var exists bool
	if err := db.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM audit_log WHERE action = $1)`, uniqueAction,
	).Scan(&exists); err != nil {
		t.Fatalf("query: %v", err)
	}
	if exists {
		t.Fatal("audit row is visible despite the caller transaction being rolled back")
	}
}

// TestEmitTx_CommitPersistsRow is TestEmitTx_RollbackLeavesNoRow's positive
// counterpart: committing the caller transaction makes the audit row durable.
func TestEmitTx_CommitPersistsRow(t *testing.T) {
	db := newTestPostgres(t)
	ctx := context.Background()
	uniqueAction := fmt.Sprintf("test.tx-commit.%s", t.Name())

	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	if err := audit.EmitTx(ctx, tx, audit.Event{
		ActorType: audit.ActorCustomer,
		ActorID:   fmt.Sprintf("cust-%s", t.Name()),
		Action:    uniqueAction,
	}); err != nil {
		t.Fatalf("EmitTx: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}

	var exists bool
	if err := db.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM audit_log WHERE action = $1)`, uniqueAction,
	).Scan(&exists); err != nil {
		t.Fatalf("query: %v", err)
	}
	if !exists {
		t.Fatal("audit row missing after the caller transaction committed")
	}
}

// TestEmitTx_NilTx verifies the defensive nil-tx error path.
func TestEmitTx_NilTx(t *testing.T) {
	err := audit.EmitTx(context.Background(), nil, audit.Event{
		ActorType: audit.ActorCustomer,
		ActorID:   "cust-1",
		Action:    "test.action",
	})
	if err == nil {
		t.Fatal("expected error for nil tx, got nil")
	}
}

func TestEmit_NilDetails(t *testing.T) {
	db := newTestPostgres(t)
	ctx := context.Background()

	// Use a unique action to locate the row: system events always get NULL actor_id
	// (nullActorID returns nil for all ActorSystem events), so querying by actor_id
	// would not match any row.
	uniqueAction := fmt.Sprintf("system.test.nil-details.%s", t.Name())

	// Emit with nil TargetType, TargetID, and Details — all three should be SQL NULL.
	if err := audit.Emit(ctx, db, audit.Event{
		ActorType: audit.ActorSystem,
		Action:    uniqueAction,
	}); err != nil {
		t.Fatalf("Emit with nil optional fields: %v", err)
	}

	// Verify details IS NULL in the database (not an empty object or "null" literal).
	var detailsJSON []byte
	err := db.QueryRow(ctx,
		`SELECT details FROM audit_log WHERE action = $1 ORDER BY id DESC LIMIT 1`,
		uniqueAction,
	).Scan(&detailsJSON)
	if err != nil {
		t.Fatalf("query round-trip row: %v", err)
	}
	if detailsJSON != nil {
		t.Fatalf("expected NULL details, got %q", detailsJSON)
	}
}
