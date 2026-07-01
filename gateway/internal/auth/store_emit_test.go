package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Unluckyathecking/crucible/gateway/internal/events"
	"github.com/Unluckyathecking/crucible/gateway/internal/webhookout"
)

// insertEmitTestKey mirrors insertTestKey but also returns the customer UUID and
// registers one active webhook endpoint for that customer — needed to assert
// exactly-one-row webhook_deliveries fan-out for the emission tests below.
func insertEmitTestKey(t *testing.T, ctx context.Context, db *pgxpool.Pool, salt string) (keyID, custID uuid.UUID, fullKey, prefix string) {
	t.Helper()

	fullKey, prefix, err := Generate(testPrefix)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	custID = uuid.New()
	email := fmt.Sprintf("emit-%s@example.com", uuid.NewString()[:8])
	if _, err := db.Exec(ctx, `INSERT INTO customers (id, email, plan_id) VALUES ($1, $2, 'free')`, custID, email); err != nil {
		t.Fatalf("insert customer: %v", err)
	}

	secret, err := webhookout.GenerateSecret()
	if err != nil {
		t.Fatalf("generate secret: %v", err)
	}
	if _, err := db.Exec(ctx, `
		INSERT INTO webhook_endpoints (customer_id, url, secret, active) VALUES ($1, 'https://example.com/hook', $2, TRUE)
	`, custID, secret); err != nil {
		t.Fatalf("insert webhook endpoint: %v", err)
	}

	hash := Hash(salt, fullKey)
	keyID = uuid.New()
	if _, err := db.Exec(ctx, `
		INSERT INTO api_keys (id, customer_id, prefix, hash, name)
		VALUES ($1, $2, $3, $4, 'store emit test')
	`, keyID, custID, prefix, hash); err != nil {
		t.Fatalf("insert api_key: %v", err)
	}

	return keyID, custID, fullKey, prefix
}

// queryOneEmitDelivery asserts exactly one webhook_deliveries row exists for
// custID and returns its event_type and payload.
func queryOneEmitDelivery(t *testing.T, db *pgxpool.Pool, custID uuid.UUID) (eventType string, payload []byte) {
	t.Helper()
	var count int
	err := db.QueryRow(context.Background(), `
		SELECT d.event_type, d.payload, count(*) OVER()
		FROM webhook_deliveries d
		JOIN webhook_endpoints we ON we.id = d.endpoint_id
		WHERE we.customer_id = $1
	`, custID).Scan(&eventType, &payload, &count)
	if err != nil {
		t.Fatalf("query webhook_deliveries: %v", err)
	}
	if count != 1 {
		t.Fatalf("webhook_deliveries row count = %d, want exactly 1", count)
	}
	return eventType, payload
}

// TestStore_Revoke_EmitsWebhook is a real-Postgres integration test: Revoke must
// insert exactly one webhook_deliveries row for the customer's one active
// endpoint, with the api_key.revoked event type and a well-formed payload.
func TestStore_Revoke_EmitsWebhook(t *testing.T) {
	db := newTestPostgres(t)
	defer db.Close()
	rdb := newTestRedis(t)
	ctx := context.Background()

	emitter := webhookout.NewEmitter(context.Background(), db)
	t.Cleanup(emitter.Stop)

	s := NewStore(db, rdb, testSalt)
	s.SetEmitter(emitter)

	keyID, custID, _, _ := insertEmitTestKey(t, ctx, db, testSalt)

	if err := s.Revoke(ctx, keyID); err != nil {
		t.Fatalf("Revoke: %v", err)
	}

	eventType, payload := queryOneEmitDelivery(t, db, custID)
	if eventType != events.APIKeyRevoked {
		t.Errorf("event_type = %q, want %q", eventType, events.APIKeyRevoked)
	}
	var decoded events.APIKeyRevokedPayload
	if err := json.Unmarshal(payload, &decoded); err != nil {
		t.Fatalf("payload is not valid APIKeyRevokedPayload JSON: %v", err)
	}
	if decoded.CustomerID != custID.String() {
		t.Errorf("payload.customer_id = %q, want %q", decoded.CustomerID, custID.String())
	}
	if decoded.KeyID != keyID.String() {
		t.Errorf("payload.key_id = %q, want %q", decoded.KeyID, keyID.String())
	}
}

// TestStore_Rotate_EmitsWebhook mirrors the revoke test for Rotate: rotating a
// key must emit api_key.rotated with both the old and new key ids.
func TestStore_Rotate_EmitsWebhook(t *testing.T) {
	db := newTestPostgres(t)
	defer db.Close()
	rdb := newTestRedis(t)
	ctx := context.Background()

	emitter := webhookout.NewEmitter(context.Background(), db)
	t.Cleanup(emitter.Stop)

	s := NewStore(db, rdb, testSalt)
	s.SetEmitter(emitter)

	keyID, custID, _, _ := insertEmitTestKey(t, ctx, db, testSalt)

	_, newKeyID, err := s.Rotate(ctx, keyID, testPrefix, time.Hour)
	if err != nil {
		t.Fatalf("Rotate: %v", err)
	}

	eventType, payload := queryOneEmitDelivery(t, db, custID)
	if eventType != events.APIKeyRotated {
		t.Errorf("event_type = %q, want %q", eventType, events.APIKeyRotated)
	}
	var decoded events.APIKeyRotatedPayload
	if err := json.Unmarshal(payload, &decoded); err != nil {
		t.Fatalf("payload is not valid APIKeyRotatedPayload JSON: %v", err)
	}
	if decoded.CustomerID != custID.String() {
		t.Errorf("payload.customer_id = %q, want %q", decoded.CustomerID, custID.String())
	}
	if decoded.OldKeyID != keyID.String() {
		t.Errorf("payload.old_key_id = %q, want %q", decoded.OldKeyID, keyID.String())
	}
	if decoded.NewKeyID != newKeyID.String() {
		t.Errorf("payload.new_key_id = %q, want %q", decoded.NewKeyID, newKeyID.String())
	}
}

// TestStore_Revoke_EmitErrorDoesNotAffectResult verifies that a failing Emit
// (here: an emitter backed by an already-closed pool) never turns a successful
// Revoke into an error.
func TestStore_Revoke_EmitErrorDoesNotAffectResult(t *testing.T) {
	db := newTestPostgres(t)
	defer db.Close()
	rdb := newTestRedis(t)
	ctx := context.Background()

	brokenPool := newTestPostgres(t)
	brokenPool.Close()
	emitter := webhookout.NewEmitter(context.Background(), brokenPool)
	t.Cleanup(emitter.Stop)

	s := NewStore(db, rdb, testSalt)
	s.SetEmitter(emitter)

	keyID, _, _, _ := insertEmitTestKey(t, ctx, db, testSalt)

	if err := s.Revoke(ctx, keyID); err != nil {
		t.Fatalf("Revoke returned error despite Emit failing against a closed pool: %v", err)
	}
}

// TestStore_Rotate_EmitErrorDoesNotAffectResult mirrors the Revoke case for Rotate.
func TestStore_Rotate_EmitErrorDoesNotAffectResult(t *testing.T) {
	db := newTestPostgres(t)
	defer db.Close()
	rdb := newTestRedis(t)
	ctx := context.Background()

	brokenPool := newTestPostgres(t)
	brokenPool.Close()
	emitter := webhookout.NewEmitter(context.Background(), brokenPool)
	t.Cleanup(emitter.Stop)

	s := NewStore(db, rdb, testSalt)
	s.SetEmitter(emitter)

	keyID, _, _, _ := insertEmitTestKey(t, ctx, db, testSalt)

	if _, _, err := s.Rotate(ctx, keyID, testPrefix, time.Hour); err != nil {
		t.Fatalf("Rotate returned error despite Emit failing against a closed pool: %v", err)
	}
}
