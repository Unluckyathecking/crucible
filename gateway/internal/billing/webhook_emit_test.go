package billing

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Unluckyathecking/crucible/gateway/internal/events"
	"github.com/Unluckyathecking/crucible/gateway/internal/webhookout"
)

// newTestPool returns a pgxpool connected to the local Postgres instance, or
// skips the test if unreachable. Follows the same pattern as auth/store_test.go.
func newTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, "postgres://crucible@localhost:5432/crucible?sslmode=disable")
	if err != nil {
		t.Skipf("postgres unavailable: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Skipf("postgres ping failed: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// insertEmitTestCustomer inserts a customer + one active webhook endpoint,
// returning the customer's internal UUID.
func insertEmitTestCustomer(t *testing.T, pool *pgxpool.Pool, planID, stripeCustomerID string) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	custID := uuid.New()
	email := fmt.Sprintf("emit-%s@example.com", uuid.NewString()[:8])
	if _, err := pool.Exec(ctx, `
		INSERT INTO customers (id, email, plan_id, stripe_customer_id) VALUES ($1, $2, $3, $4)
	`, custID, email, planID, stripeCustomerID); err != nil {
		t.Fatalf("insert customer: %v", err)
	}
	secret, err := webhookout.GenerateSecret()
	if err != nil {
		t.Fatalf("generate secret: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO webhook_endpoints (customer_id, url, secret, active) VALUES ($1, 'https://example.com/hook', $2, TRUE)
	`, custID, secret); err != nil {
		t.Fatalf("insert webhook endpoint: %v", err)
	}
	return custID
}

// queryOneDelivery asserts exactly one webhook_deliveries row exists for custID
// and returns its event_type and payload.
func queryOneDelivery(t *testing.T, pool *pgxpool.Pool, custID uuid.UUID) (eventType string, payload []byte) {
	t.Helper()
	var count int
	err := pool.QueryRow(context.Background(), `
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

// TestHandle_EmitsSubscriptionUpdatedWebhook is a real-Postgres integration test:
// dispatching a customer.subscription.updated event through the actual HTTP
// Handle() entrypoint must insert exactly one webhook_deliveries row for the
// customer's one active endpoint, with the subscription.updated event type and
// a well-formed SubscriptionEventPayload.
func TestHandle_EmitsSubscriptionUpdatedWebhook(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	stripeCustomerID := "cus_" + uuid.NewString()[:12]
	priceID := "price_" + uuid.NewString()[:12]
	planID := "emit-upsert-" + uuid.NewString()[:8]

	custID := insertEmitTestCustomer(t, pool, "free", stripeCustomerID)
	if _, err := pool.Exec(ctx, `
		INSERT INTO plans (id, display_name, stripe_price_id, rate_limit_per_minute, monthly_unit_cap)
		VALUES ($1, $1, $2, 60, 1000)
	`, planID, priceID); err != nil {
		t.Fatalf("insert plan: %v", err)
	}

	emitter := webhookout.NewEmitter(context.Background(), pool)
	t.Cleanup(emitter.Stop)

	const whsec = "whsec_emit_upsert_test"
	h := NewWebhook(whsec, pool)
	h.SetEmitter(emitter)

	body := []byte(fmt.Sprintf(
		`{"id":%q,"type":"customer.subscription.updated","data":{"object":{"customer":%q,"items":{"data":[{"price":{"id":%q}}]}}}}`,
		"evt_"+uuid.NewString(), stripeCustomerID, priceID))
	ts := time.Now().Unix()
	req := httptest.NewRequest("POST", "/webhooks/stripe", bytes.NewReader(body))
	req.Header.Set("Stripe-Signature", newStripeHeader(whsec, body, ts))
	rec := httptest.NewRecorder()
	h.Handle(rec, req)

	if rec.Code != 200 {
		t.Fatalf("Handle status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	eventType, payload := queryOneDelivery(t, pool, custID)
	if eventType != events.SubscriptionUpdated {
		t.Errorf("event_type = %q, want %q", eventType, events.SubscriptionUpdated)
	}
	var decoded events.SubscriptionEventPayload
	if err := json.Unmarshal(payload, &decoded); err != nil {
		t.Fatalf("payload is not valid SubscriptionEventPayload JSON: %v", err)
	}
	if decoded.CustomerID != custID.String() {
		t.Errorf("payload.customer_id = %q, want %q", decoded.CustomerID, custID.String())
	}
	if decoded.PlanID != planID {
		t.Errorf("payload.plan_id = %q, want %q", decoded.PlanID, planID)
	}
}

// TestHandle_EmitsSubscriptionDeletedWebhook mirrors the upsert test for the
// cancellation path: a canceled subscription reverts plan_id to "free" and must
// emit subscription.deleted with plan_id "free".
func TestHandle_EmitsSubscriptionDeletedWebhook(t *testing.T) {
	pool := newTestPool(t)

	stripeCustomerID := "cus_" + uuid.NewString()[:12]
	custID := insertEmitTestCustomer(t, pool, "pro", stripeCustomerID)

	emitter := webhookout.NewEmitter(context.Background(), pool)
	t.Cleanup(emitter.Stop)

	const whsec = "whsec_emit_deleted_test"
	h := NewWebhook(whsec, pool)
	h.SetEmitter(emitter)

	body := []byte(fmt.Sprintf(
		`{"id":%q,"type":"customer.subscription.deleted","data":{"object":{"customer":%q,"status":"canceled"}}}`,
		"evt_"+uuid.NewString(), stripeCustomerID))
	ts := time.Now().Unix()
	req := httptest.NewRequest("POST", "/webhooks/stripe", bytes.NewReader(body))
	req.Header.Set("Stripe-Signature", newStripeHeader(whsec, body, ts))
	rec := httptest.NewRecorder()
	h.Handle(rec, req)

	if rec.Code != 200 {
		t.Fatalf("Handle status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	eventType, payload := queryOneDelivery(t, pool, custID)
	if eventType != events.SubscriptionDeleted {
		t.Errorf("event_type = %q, want %q", eventType, events.SubscriptionDeleted)
	}
	var decoded events.SubscriptionEventPayload
	if err := json.Unmarshal(payload, &decoded); err != nil {
		t.Fatalf("payload is not valid SubscriptionEventPayload JSON: %v", err)
	}
	if decoded.CustomerID != custID.String() {
		t.Errorf("payload.customer_id = %q, want %q", decoded.CustomerID, custID.String())
	}
	if decoded.PlanID != "free" {
		t.Errorf("payload.plan_id = %q, want free", decoded.PlanID)
	}
}

// TestHandle_EmitErrorDoesNotAffectResponse verifies that a failing Emit (here:
// an emitter backed by an already-closed pool) never changes the 200 response
// Handle already committed to returning after a successful dispatch.
func TestHandle_EmitErrorDoesNotAffectResponse(t *testing.T) {
	pool := newTestPool(t)

	stripeCustomerID := "cus_" + uuid.NewString()[:12]
	insertEmitTestCustomer(t, pool, "pro", stripeCustomerID)

	brokenPool := newTestPool(t)
	brokenPool.Close()
	emitter := webhookout.NewEmitter(context.Background(), brokenPool)
	t.Cleanup(emitter.Stop)

	const whsec = "whsec_emit_error_test"
	h := NewWebhook(whsec, pool)
	h.SetEmitter(emitter)

	body := []byte(fmt.Sprintf(
		`{"id":%q,"type":"customer.subscription.deleted","data":{"object":{"customer":%q,"status":"canceled"}}}`,
		"evt_"+uuid.NewString(), stripeCustomerID))
	ts := time.Now().Unix()
	req := httptest.NewRequest("POST", "/webhooks/stripe", bytes.NewReader(body))
	req.Header.Set("Stripe-Signature", newStripeHeader(whsec, body, ts))
	rec := httptest.NewRecorder()
	h.Handle(rec, req)

	if rec.Code != 200 {
		t.Fatalf("Handle status = %d, want 200 despite Emit failing against a closed pool; body=%s", rec.Code, rec.Body.String())
	}
}
