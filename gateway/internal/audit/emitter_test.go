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

// newTestPostgres returns a pgxpool connected to the local Postgres instance or
// skips the test if unreachable. Mirrors the helper in gateway/internal/auth.
func newTestPostgres(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		dsn = "postgres://crucible@localhost:5432/crucible?sslmode=disable"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Skipf("postgres unavailable, skipping: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Skipf("postgres ping failed, skipping: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func TestEmit_RejectsInvalidActorType(t *testing.T) {
	// No Postgres needed — validation fires before any DB call.
	err := audit.Emit(context.Background(), nil, audit.Event{
		ActorType: "invalid",
		Action:    "test.action",
	})
	if err == nil {
		t.Fatal("expected error for invalid actor_type, got nil")
	}
}

func TestEmit_RoundTrip(t *testing.T) {
	db := newTestPostgres(t)
	ctx := context.Background()

	uniqueActorID := fmt.Sprintf("test-cust-%d", time.Now().UnixNano())

	e := audit.Event{
		ActorType:  audit.ActorCustomer,
		ActorID:    uniqueActorID,
		Action:     "api_key.created",
		TargetType: "api_key",
		TargetID:   "test-key-id",
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
	)
	err := db.QueryRow(ctx, `
		SELECT actor_type, actor_id, action, target_type, target_id, details
		FROM audit_log
		WHERE actor_id = $1 AND action = $2
		ORDER BY id DESC LIMIT 1
	`, uniqueActorID, e.Action).Scan(&actorType, &actorID, &action, &targetType, &targetID, &detailsJSON)
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
	if targetType != e.TargetType {
		t.Errorf("target_type = %q, want %q", targetType, e.TargetType)
	}
	if targetID != e.TargetID {
		t.Errorf("target_id = %q, want %q", targetID, e.TargetID)
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
}

func TestEmit_AllActorTypes(t *testing.T) {
	db := newTestPostgres(t)
	ctx := context.Background()

	for _, at := range []audit.ActorType{audit.ActorCustomer, audit.ActorAdmin, audit.ActorSystem} {
		t.Run(string(at), func(t *testing.T) {
			err := audit.Emit(ctx, db, audit.Event{
				ActorType:  at,
				ActorID:    "actor-id",
				Action:     "test.event",
				TargetType: "resource",
				TargetID:   "resource-id",
			})
			if err != nil {
				t.Errorf("Emit with actor_type=%q: %v", at, err)
			}
		})
	}
}

func TestEmit_NilDetails(t *testing.T) {
	db := newTestPostgres(t)
	ctx := context.Background()

	if err := audit.Emit(ctx, db, audit.Event{
		ActorType:  audit.ActorSystem,
		ActorID:    "system",
		Action:     "api_key.revoked",
		TargetType: "api_key",
		TargetID:   "some-key-id",
	}); err != nil {
		t.Fatalf("Emit with nil Details: %v", err)
	}
}
