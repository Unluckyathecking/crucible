package quota

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Unluckyathecking/crucible/gateway/internal/auth"
	"github.com/Unluckyathecking/crucible/gateway/internal/billing"
	"github.com/Unluckyathecking/crucible/gateway/internal/events"
	"github.com/Unluckyathecking/crucible/gateway/internal/webhookout"
)

// TestQuotaMiddleware_EmitsQuotaExceededWebhook is a real-Postgres integration test:
// triggering the quota-exceeded (429) path with a configured emitter must insert
// exactly one webhook_deliveries row for the customer's one active endpoint, with
// the quota.exceeded event type and a well-formed QuotaExceededPayload.
func TestQuotaMiddleware_EmitsQuotaExceededWebhook(t *testing.T) {
	rdb := newTestRedis(t)
	pool := newTestPool(t)
	plans := billing.NewPlanCache(pool)

	freeCap := plans.MonthlyCap(context.Background(), "free")
	if freeCap == 0 {
		t.Skip("free plan has unlimited cap in this environment; skipping emit test")
	}

	tr := New(rdb)
	emitter := webhookout.NewEmitter(context.Background(), pool)
	t.Cleanup(emitter.Stop)

	key := testKeyForPlan("free")
	ctx := context.Background()

	if _, err := pool.Exec(ctx, `INSERT INTO customers (id, email, plan_id) VALUES ($1, $2, 'free') ON CONFLICT DO NOTHING`,
		key.Customer.ID, key.Customer.Email); err != nil {
		t.Fatalf("insert customer: %v", err)
	}
	secret, err := webhookout.GenerateSecret()
	if err != nil {
		t.Fatalf("generate secret: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO webhook_endpoints (customer_id, url, secret, active) VALUES ($1, 'https://example.com/hook', $2, TRUE)`,
		key.Customer.ID, secret); err != nil {
		t.Fatalf("insert webhook endpoint: %v", err)
	}

	redisKey := monthKey(key.Customer.ID, time.Now().UTC())
	t.Cleanup(func() { rdb.Del(context.Background(), redisKey) })

	// Pre-fill the counter to cap so the next request through Middleware is rejected.
	for i := int64(0); i < freeCap; i++ {
		ok, _, _, _, err := tr.Reserve(ctx, key.Customer.ID, freeCap)
		if err != nil || !ok {
			t.Fatalf("pre-fill reserve %d: ok=%v err=%v", i, ok, err)
		}
	}

	handler := Middleware(tr, plans, emitter)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("inner handler must not be called when quota is exceeded")
	}))

	req := httptest.NewRequest(http.MethodPost, "/", nil).WithContext(auth.WithTestKey(ctx, key))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", rec.Code)
	}

	// Emit runs synchronously inside the middleware before ServeHTTP returns, so
	// the row is visible immediately with no polling required.
	var eventType string
	var payload []byte
	var count int
	err = pool.QueryRow(ctx, `
		SELECT d.event_type, d.payload, count(*) OVER()
		FROM webhook_deliveries d
		JOIN webhook_endpoints we ON we.id = d.endpoint_id
		WHERE we.customer_id = $1
	`, key.Customer.ID).Scan(&eventType, &payload, &count)
	if err != nil {
		t.Fatalf("query webhook_deliveries: %v", err)
	}
	if count != 1 {
		t.Fatalf("webhook_deliveries row count = %d, want exactly 1", count)
	}
	if eventType != events.QuotaExceeded {
		t.Errorf("event_type = %q, want %q", eventType, events.QuotaExceeded)
	}
	var decoded events.QuotaExceededPayload
	if err := json.Unmarshal(payload, &decoded); err != nil {
		t.Fatalf("payload is not valid QuotaExceededPayload JSON: %v", err)
	}
	if decoded.CustomerID != key.Customer.ID.String() {
		t.Errorf("payload.customer_id = %q, want %q", decoded.CustomerID, key.Customer.ID.String())
	}
	if decoded.Plan != "free" {
		t.Errorf("payload.plan = %q, want free", decoded.Plan)
	}
	if decoded.Cap != freeCap {
		t.Errorf("payload.cap = %d, want %d", decoded.Cap, freeCap)
	}
}

// TestQuotaMiddleware_EmitErrorDoesNotAffectResponse verifies that a failing Emit
// (here: an emitter backed by an already-closed pool) never changes the 429
// response the quota-exceeded path already committed to returning.
func TestQuotaMiddleware_EmitErrorDoesNotAffectResponse(t *testing.T) {
	rdb := newTestRedis(t)
	pool := newTestPool(t)
	plans := billing.NewPlanCache(pool)

	freeCap := plans.MonthlyCap(context.Background(), "free")
	if freeCap == 0 {
		t.Skip("free plan has unlimited cap in this environment; skipping emit-error test")
	}

	// A pool that is closed before use forces Emitter.Emit's Exec call to fail,
	// modeling an injected Emit error without touching the middleware's control flow.
	brokenPool := newTestPool(t)
	brokenPool.Close()
	emitter := webhookout.NewEmitter(context.Background(), brokenPool)
	t.Cleanup(emitter.Stop)

	tr := New(rdb)
	key := testKeyForPlan("free")
	ctx := context.Background()
	redisKey := monthKey(key.Customer.ID, time.Now().UTC())
	t.Cleanup(func() { rdb.Del(context.Background(), redisKey) })

	for i := int64(0); i < freeCap; i++ {
		ok, _, _, _, err := tr.Reserve(ctx, key.Customer.ID, freeCap)
		if err != nil || !ok {
			t.Fatalf("pre-fill reserve %d: ok=%v err=%v", i, ok, err)
		}
	}

	called := false
	handler := Middleware(tr, plans, emitter)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	}))

	req := httptest.NewRequest(http.MethodPost, "/", nil).WithContext(auth.WithTestKey(ctx, key))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if called {
		t.Error("inner handler must not be called when quota is exceeded")
	}
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429 despite Emit failing against a closed pool", rec.Code)
	}
}
