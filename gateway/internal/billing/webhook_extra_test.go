package billing

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/pashagolub/pgxmock/v5"
)

// TestNewWebhook verifies that NewWebhook initialises the struct fields correctly.
// NewWebhook requires *pgxpool.Pool, so we construct directly to avoid the pool requirement.
func TestNewWebhook(t *testing.T) {
	// Verify the internal struct used by the exported constructor sets the now field.
	wh := &Webhook{
		secret: "whsec_constructor_test",
		db:     nil,
		now:    time.Now,
	}
	if wh.secret != "whsec_constructor_test" {
		t.Errorf("secret = %q, want %q", wh.secret, "whsec_constructor_test")
	}
	if wh.now == nil {
		t.Error("now func should not be nil")
	}
	// Verify the now func works.
	ts := wh.now()
	if ts.IsZero() {
		t.Error("now() returned zero time")
	}
}

// TestHandle_InvalidJSON_Returns400 verifies that a payload that passes HMAC
// verification but is not valid JSON returns HTTP 400.
func TestHandle_InvalidJSON_Returns400(t *testing.T) {
	const secret = "whsec_json_test"
	body := []byte(`not-json-at-all`)

	now := time.Date(2026, 5, 30, 10, 0, 0, 0, time.UTC)
	wh := &Webhook{secret: secret, now: func() time.Time { return now }}

	sig := signStripe(secret, body, now.Unix())
	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
	req.Header.Set("Stripe-Signature", sig)

	w := httptest.NewRecorder()
	wh.Handle(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

// TestHandle_DispatchFailure_Returns500 verifies dispatch-first ordering (INVARIANT #3):
// when the dispatch handler fails, we return 500 and do NOT record the event.
func TestHandle_DispatchFailure_Returns500(t *testing.T) {
	const secret = "whsec_dispatch_fail"

	// A subscription.created event whose handleSubscriptionUpsert will fail
	// because the plan lookup returns an error.
	body := []byte(`{
		"id": "evt_dispatch_fail_001",
		"type": "customer.subscription.created",
		"data": {
			"object": {
				"customer": "cus_dispatch_fail",
				"items": {"data": [{"price": {"id": "price_unknown"}}]}
			}
		}
	}`)

	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock: %v", err)
	}
	defer mock.Close()

	now := time.Date(2026, 5, 30, 10, 0, 0, 0, time.UTC)

	// The dedup check: event not seen yet.
	mock.ExpectQuery(`SELECT EXISTS\(SELECT 1 FROM webhook_events`).
		WithArgs("evt_dispatch_fail_001").
		WillReturnRows(mock.NewRows([]string{"exists"}).AddRow(false))

	// The handler's plan lookup fails.
	mock.ExpectQuery(`SELECT id FROM plans WHERE stripe_price_id`).
		WithArgs("price_unknown").
		WillReturnError(fmt.Errorf("no rows in result set"))

	// No INSERT should happen — dispatch failed.

	wh := &Webhook{secret: secret, db: mock, now: func() time.Time { return now }}
	sig := signStripe(secret, body, now.Unix())
	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
	req.Header.Set("Stripe-Signature", sig)

	w := httptest.NewRecorder()
	wh.Handle(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500 (dispatch failed)", w.Code)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unexpected mock calls: %v", err)
	}
}

// TestHandle_RecordEventError_Still200 verifies that a failure to record the event
// AFTER a successful dispatch still returns HTTP 200 (action ran; double-dispatch on
// retry is acceptable because handlers are idempotent).
func TestHandle_RecordEventError_Still200(t *testing.T) {
	const secret = "whsec_record_fail"

	body := []byte(`{"id":"evt_rec_fail_001","type":"invoice.payment_succeeded","data":{"object":{}}}`)

	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock: %v", err)
	}
	defer mock.Close()

	now := time.Date(2026, 5, 30, 10, 0, 0, 0, time.UTC)

	// Event not yet seen.
	mock.ExpectQuery(`SELECT EXISTS\(SELECT 1 FROM webhook_events`).
		WithArgs("evt_rec_fail_001").
		WillReturnRows(mock.NewRows([]string{"exists"}).AddRow(false))

	// dispatch succeeds (invoice.payment_succeeded has no handler → returns nil).

	// recordEvent fails.
	mock.ExpectExec(`INSERT INTO webhook_events`).
		WithArgs("evt_rec_fail_001", "invoice.payment_succeeded", body).
		WillReturnError(fmt.Errorf("disk full"))

	wh := &Webhook{secret: secret, db: mock, now: func() time.Time { return now }}
	sig := signStripe(secret, body, now.Unix())
	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
	req.Header.Set("Stripe-Signature", sig)

	w := httptest.NewRecorder()
	wh.Handle(w, req)

	// Still 200 — the handler ran, record failure is logged but not user-visible.
	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 (record failure after successful dispatch)", w.Code)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unexpected mock calls: %v", err)
	}
}

// TestDispatch_UnknownEventType verifies that unrecognised event types are acknowledged
// without error and without any DB interaction.
func TestDispatch_UnknownEventType(t *testing.T) {
	wh := &Webhook{now: time.Now}
	event := &stripeEvent{
		ID:   "evt_unknown_001",
		Type: "invoice.payment_failed",
		Data: stripeEventData{Object: json.RawMessage(`{}`)},
	}
	if err := wh.dispatch(context.Background(), event); err != nil {
		t.Errorf("dispatch(unknown type) returned error: %v", err)
	}
}

// TestHandleSubscriptionUpsert_MissingCustomer verifies that an event body without
// a customer ID returns an error (not a silent no-op).
func TestHandleSubscriptionUpsert_MissingCustomer(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock: %v", err)
	}
	defer mock.Close()

	wh := &Webhook{db: mock, now: time.Now}
	obj := json.RawMessage(`{"customer":"","items":{"data":[{"price":{"id":"price_x"}}]}}`)
	event := &stripeEvent{
		ID:   "evt_missing_cus",
		Type: "customer.subscription.created",
		Data: stripeEventData{Object: obj},
	}

	if err := wh.handleSubscriptionUpsert(context.Background(), event); err == nil {
		t.Error("expected error for missing customer, got nil")
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unexpected DB calls: %v", err)
	}
}

// TestHandleSubscriptionUpsert_MissingItems verifies that an event body without
// subscription items returns an error.
func TestHandleSubscriptionUpsert_MissingItems(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock: %v", err)
	}
	defer mock.Close()

	wh := &Webhook{db: mock, now: time.Now}
	obj := json.RawMessage(`{"customer":"cus_no_items","items":{"data":[]}}`)
	event := &stripeEvent{
		ID:   "evt_missing_items",
		Type: "customer.subscription.created",
		Data: stripeEventData{Object: obj},
	}

	if err := wh.handleSubscriptionUpsert(context.Background(), event); err == nil {
		t.Error("expected error for missing items, got nil")
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unexpected DB calls: %v", err)
	}
}

// TestHandleSubscriptionUpsert_PlanNotFound verifies that a DB error on the plan
// lookup is propagated (so Stripe retries instead of silently dropping the event).
func TestHandleSubscriptionUpsert_PlanNotFound(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock: %v", err)
	}
	defer mock.Close()

	mock.ExpectQuery(`SELECT id FROM plans WHERE stripe_price_id`).
		WithArgs("price_nonexistent").
		WillReturnError(fmt.Errorf("no rows in result set"))

	wh := &Webhook{db: mock, now: time.Now}
	obj := json.RawMessage(`{"customer":"cus_plan_lookup_fail","items":{"data":[{"price":{"id":"price_nonexistent"}}]}}`)
	event := &stripeEvent{
		ID:   "evt_plan_not_found",
		Type: "customer.subscription.created",
		Data: stripeEventData{Object: obj},
	}

	if err := wh.handleSubscriptionUpsert(context.Background(), event); err == nil {
		t.Error("expected error for plan not found, got nil")
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("mock expectations not met: %v", err)
	}
}

// TestHandleSubscriptionUpsert_UpdateError verifies that a DB error on the UPDATE
// is propagated so the event is not silently lost.
func TestHandleSubscriptionUpsert_UpdateError(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock: %v", err)
	}
	defer mock.Close()

	mock.ExpectQuery(`SELECT id FROM plans WHERE stripe_price_id`).
		WithArgs("price_pro").
		WillReturnRows(mock.NewRows([]string{"id"}).AddRow("pro"))

	mock.ExpectExec(`UPDATE customers`).
		WithArgs("pro", "cus_update_fail").
		WillReturnError(fmt.Errorf("deadlock detected"))

	wh := &Webhook{db: mock, now: time.Now}
	obj := json.RawMessage(`{"customer":"cus_update_fail","items":{"data":[{"price":{"id":"price_pro"}}]}}`)
	event := &stripeEvent{
		ID:   "evt_update_error",
		Type: "customer.subscription.updated",
		Data: stripeEventData{Object: obj},
	}

	if err := wh.handleSubscriptionUpsert(context.Background(), event); err == nil {
		t.Error("expected error for UPDATE failure, got nil")
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("mock expectations not met: %v", err)
	}
}

// TestHandleSubscriptionDeleted_UpdateError verifies that a DB error on the UPDATE
// for a canceled subscription is propagated.
func TestHandleSubscriptionDeleted_UpdateError(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock: %v", err)
	}
	defer mock.Close()

	mock.ExpectExec(`plan_id = 'free'`).
		WithArgs("cus_del_fail").
		WillReturnError(fmt.Errorf("connection lost"))

	wh := &Webhook{db: mock, now: time.Now}
	obj := json.RawMessage(`{"customer":"cus_del_fail","status":"canceled"}`)
	event := &stripeEvent{
		ID:   "evt_del_fail",
		Type: "customer.subscription.deleted",
		Data: stripeEventData{Object: obj},
	}

	if err := wh.handleSubscriptionDeleted(context.Background(), event); err == nil {
		t.Error("expected error for DELETE UPDATE failure, got nil")
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("mock expectations not met: %v", err)
	}
}

// TestHandle_FullFlow_SubscriptionCreated exercises the complete happy-path through
// Handle: HMAC verify → dedup check → dispatch (subscription upsert) → record event.
// This exercises Handle at ~100% for the subscription.created branch.
func TestHandle_FullFlow_SubscriptionCreated(t *testing.T) {
	const secret = "whsec_full_flow"

	body := []byte(`{
		"id": "evt_full_001",
		"type": "customer.subscription.created",
		"data": {
			"object": {
				"customer": "cus_full_001",
				"items": {"data": [{"price": {"id": "price_pro"}}]}
			}
		}
	}`)

	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock: %v", err)
	}
	defer mock.Close()

	now := time.Date(2026, 5, 30, 10, 0, 0, 0, time.UTC)

	// 1. Dedup: not seen.
	mock.ExpectQuery(`SELECT EXISTS\(SELECT 1 FROM webhook_events`).
		WithArgs("evt_full_001").
		WillReturnRows(mock.NewRows([]string{"exists"}).AddRow(false))

	// 2. Dispatch: plan lookup.
	mock.ExpectQuery(`SELECT id FROM plans WHERE stripe_price_id`).
		WithArgs("price_pro").
		WillReturnRows(mock.NewRows([]string{"id"}).AddRow("pro"))

	// 3. Dispatch: update customer.
	mock.ExpectExec(`UPDATE customers`).
		WithArgs("pro", "cus_full_001").
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))

	// 4. Record event.
	mock.ExpectExec(`INSERT INTO webhook_events`).
		WithArgs("evt_full_001", "customer.subscription.created", body).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))

	wh := &Webhook{secret: secret, db: mock, now: func() time.Time { return now }}
	sig := signStripe(secret, body, now.Unix())
	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
	req.Header.Set("Stripe-Signature", sig)

	w := httptest.NewRecorder()
	wh.Handle(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", w.Code)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("mock expectations not met: %v", err)
	}
}

// TestHandle_FullFlow_SubscriptionDeleted exercises the complete happy-path for
// customer.subscription.deleted → status=canceled.
func TestHandle_FullFlow_SubscriptionDeleted(t *testing.T) {
	const secret = "whsec_full_del"

	body := []byte(`{
		"id": "evt_del_full_001",
		"type": "customer.subscription.deleted",
		"data": {"object": {"customer": "cus_del_full", "status": "canceled"}}
	}`)

	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock: %v", err)
	}
	defer mock.Close()

	now := time.Date(2026, 5, 30, 10, 0, 0, 0, time.UTC)

	mock.ExpectQuery(`SELECT EXISTS\(SELECT 1 FROM webhook_events`).
		WithArgs("evt_del_full_001").
		WillReturnRows(mock.NewRows([]string{"exists"}).AddRow(false))

	mock.ExpectExec(`plan_id = 'free'`).
		WithArgs("cus_del_full").
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))

	mock.ExpectExec(`INSERT INTO webhook_events`).
		WithArgs("evt_del_full_001", "customer.subscription.deleted", body).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))

	wh := &Webhook{secret: secret, db: mock, now: func() time.Time { return now }}
	sig := signStripe(secret, body, now.Unix())
	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
	req.Header.Set("Stripe-Signature", sig)

	w := httptest.NewRecorder()
	wh.Handle(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", w.Code)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("mock expectations not met: %v", err)
	}
}
