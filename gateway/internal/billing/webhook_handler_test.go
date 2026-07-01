package billing

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/pashagolub/pgxmock/v5"
)

// newStripeHeader builds a Stripe-Signature header value (t=<ts>,v1=<hmac-sha256>)
// for the given secret, body, and unix timestamp. Self-contained so this file
// does not depend on helpers defined in sibling test files.
func newStripeHeader(secret string, body []byte, ts int64) string {
	mac := hmac.New(sha256.New, []byte(secret))
	if _, err := mac.Write([]byte(fmt.Sprintf("%d.%s", ts, body))); err != nil {
		panic(fmt.Sprintf("hmac.Write: %v", err))
	}
	return fmt.Sprintf("t=%d,v1=%s", ts, hex.EncodeToString(mac.Sum(nil)))
}

// TestHandleSubscriptionDeleted is the regression guard for the "wrong-status downgrade"
// bug: only a "canceled" event must fire the plan_id → "free" DB update.
func TestHandleSubscriptionDeleted(t *testing.T) {
	tests := []struct {
		name     string
		status   string
		customer string
		wantExec bool
	}{
		{"canceled downgrades to free", "canceled", "cus_cancel_001", true},
		{"past_due does not downgrade", "past_due", "cus_pastdue_001", false},
		{"active does not downgrade", "active", "cus_active_001", false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			mock, err := pgxmock.NewPool()
			if err != nil {
				t.Fatalf("pgxmock: %v", err)
			}
			defer mock.Close()

			if tc.wantExec {
				// Regex includes the hardcoded literal to validate the business rule,
				// not just "some UPDATE ran".
				mock.ExpectExec(`plan_id = 'free'`).
					WithArgs(tc.customer).
					WillReturnResult(pgxmock.NewResult("UPDATE", 1))
			}

			h := &Webhook{db: mock, now: time.Now}
			obj, _ := json.Marshal(map[string]string{
				"customer": tc.customer,
				"status":   tc.status,
			})
			event := &stripeEvent{
				ID:   "evt_del_" + tc.status,
				Type: "customer.subscription.deleted",
				Data: stripeEventData{Object: json.RawMessage(obj)},
			}

			if _, err := h.handleSubscriptionDeleted(context.Background(), event); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Errorf("mock expectations not met: %v", err)
			}
		})
	}
}

// TestHandleSubscriptionUpsert covers the customer.subscription.created /
// customer.subscription.updated path: the handler resolves the plan from the
// Stripe price ID and updates the customer row.
func TestHandleSubscriptionUpsert(t *testing.T) {
	tests := []struct {
		name    string
		event   string
		customer string
		priceID string
		planID  string
	}{
		{"subscription.created maps price to plan", "customer.subscription.created", "cus_upsert_001", "price_pro_monthly", "pro"},
		{"subscription.updated applies new plan", "customer.subscription.updated", "cus_upsert_002", "price_basic_annual", "basic"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			mock, err := pgxmock.NewPool()
			if err != nil {
				t.Fatalf("pgxmock: %v", err)
			}
			defer mock.Close()

			mock.ExpectQuery(`SELECT id FROM plans WHERE stripe_price_id`).
				WithArgs(tc.priceID).
				WillReturnRows(mock.NewRows([]string{"id"}).AddRow(tc.planID))

			mock.ExpectExec(`UPDATE customers SET plan_id`).
				WithArgs(tc.planID, tc.customer).
				WillReturnResult(pgxmock.NewResult("UPDATE", 1))

			h := &Webhook{db: mock, now: time.Now}
			obj := json.RawMessage(fmt.Sprintf(
				`{"customer":%q,"items":{"data":[{"price":{"id":%q}}]}}`,
				tc.customer, tc.priceID,
			))
			event := &stripeEvent{
				ID:   "evt_upsert_" + tc.customer,
				Type: tc.event,
				Data: stripeEventData{Object: obj},
			}

			if _, err := h.handleSubscriptionUpsert(context.Background(), event); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Errorf("mock expectations not met: %v", err)
			}
		})
	}
}

// TestHandleSubscriptionUpsert_WithCacheInvalidation verifies that when a cache deleter
// is configured, the handler invalidates Redis auth entries after upgrading the plan.
func TestHandleSubscriptionUpsert_WithCacheInvalidation(t *testing.T) {
	const (
		customer   = "cus_cache_upsert"
		customerID = "550e8400-e29b-41d4-a716-000000000020"
		priceID    = "price_pro"
		planID     = "pro"
		prefix     = "cru_live_cache123456789"
	)

	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock: %v", err)
	}
	defer mock.Close()

	mock.ExpectQuery(`SELECT id FROM plans WHERE stripe_price_id`).
		WithArgs(priceID).
		WillReturnRows(mock.NewRows([]string{"id"}).AddRow(planID))

	mock.ExpectExec(`UPDATE customers SET plan_id`).
		WithArgs(planID, customer).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))

	// When cache != nil, handler fetches customer UUID for invalidation.
	mock.ExpectQuery(`SELECT id FROM customers WHERE stripe_customer_id`).
		WithArgs(customer).
		WillReturnRows(mock.NewRows([]string{"id"}).AddRow(customerID))

	// invalidateCustomerCache queries prefixes.
	mock.ExpectQuery(`SELECT prefix FROM api_keys WHERE customer_id`).
		WithArgs(customerID, 1000).
		WillReturnRows(mock.NewRows([]string{"prefix"}).AddRow(prefix))

	spy := &spyCacheDeleter{}
	h := &Webhook{db: mock, now: time.Now, cache: spy}

	obj := json.RawMessage(fmt.Sprintf(`{"customer":%q,"items":{"data":[{"price":{"id":%q}}]}}`, customer, priceID))
	event := &stripeEvent{
		ID:   "evt_cache_upsert_001",
		Type: "customer.subscription.created",
		Data: stripeEventData{Object: obj},
	}

	if _, err := h.handleSubscriptionUpsert(context.Background(), event); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Assert cache invalidation was attempted.
	if len(spy.calls) == 0 {
		t.Fatal("expected cache invalidation to be attempted after plan upsert")
	}
	wantKey := "auth:" + prefix
	found := false
	for _, k := range spy.calls[0] {
		if k == wantKey {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected DEL %q, got %v", wantKey, spy.calls[0])
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("mock expectations not met: %v", err)
	}
}

// TestHandleSubscriptionDeleted_WithCacheInvalidation verifies that when a cache deleter
// is configured, the downgrade handler invalidates Redis auth entries.
func TestHandleSubscriptionDeleted_WithCacheInvalidation(t *testing.T) {
	const (
		customer   = "cus_cache_del"
		customerID = "550e8400-e29b-41d4-a716-000000000021"
		prefix     = "cru_live_cache_del12345"
	)

	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock: %v", err)
	}
	defer mock.Close()

	mock.ExpectExec(`plan_id = 'free'`).
		WithArgs(customer).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))

	mock.ExpectQuery(`SELECT id FROM customers WHERE stripe_customer_id`).
		WithArgs(customer).
		WillReturnRows(mock.NewRows([]string{"id"}).AddRow(customerID))

	mock.ExpectQuery(`SELECT prefix FROM api_keys WHERE customer_id`).
		WithArgs(customerID, 1000).
		WillReturnRows(mock.NewRows([]string{"prefix"}).AddRow(prefix))

	spy := &spyCacheDeleter{}
	h := &Webhook{db: mock, now: time.Now, cache: spy}

	obj := json.RawMessage(fmt.Sprintf(`{"customer":%q,"status":"canceled"}`, customer))
	event := &stripeEvent{
		ID:   "evt_cache_del_001",
		Type: "customer.subscription.deleted",
		Data: stripeEventData{Object: obj},
	}

	if _, err := h.handleSubscriptionDeleted(context.Background(), event); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(spy.calls) == 0 {
		t.Fatal("expected cache invalidation after subscription deleted")
	}
	wantKey := "auth:" + prefix
	found := false
	for _, k := range spy.calls[0] {
		if k == wantKey {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected DEL %q, got %v", wantKey, spy.calls[0])
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("mock expectations not met: %v", err)
	}
}

// TestHandleSubscriptionDeleted_CacheLookupError verifies that when the customer lookup
// after a successful UPDATE fails with a non-ErrNoRows DB error, the handler returns nil
// (no webhook retry) rather than propagating the error. Cache staleness is preferred
// over causing Stripe to retry the event.
func TestHandleSubscriptionDeleted_CacheLookupError(t *testing.T) {
	const customer = "cus_lookup_err_001"

	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock: %v", err)
	}
	defer mock.Close()

	mock.ExpectExec(`plan_id = 'free'`).
		WithArgs(customer).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))

	mock.ExpectQuery(`SELECT id FROM customers WHERE stripe_customer_id`).
		WithArgs(customer).
		WillReturnError(fmt.Errorf("connection lost"))

	spy := &spyCacheDeleter{}
	h := &Webhook{db: mock, now: time.Now, cache: spy}

	obj := json.RawMessage(fmt.Sprintf(`{"customer":%q,"status":"canceled"}`, customer))
	event := &stripeEvent{
		ID:   "evt_lookup_err_001",
		Type: "customer.subscription.deleted",
		Data: stripeEventData{Object: obj},
	}

	if _, err := h.handleSubscriptionDeleted(context.Background(), event); err != nil {
		t.Fatalf("expected nil (best-effort cache), got: %v", err)
	}
	if len(spy.calls) != 0 {
		t.Errorf("expected no cache invalidation when lookup fails, got %d calls", len(spy.calls))
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("mock expectations not met: %v", err)
	}
}

// TestHandle_InvoicePaymentSucceeded_DispatchIgnored verifies that invoice.payment_succeeded
// events are acknowledged (HTTP 200) and recorded for dedup, but do NOT touch subscription
// state — this event type has no handler in the dispatch switch.
func TestHandle_InvoicePaymentSucceeded_DispatchIgnored(t *testing.T) {
	const secret = "whsec_inv_test"
	body := []byte(`{"id":"evt_inv_001","type":"invoice.payment_succeeded","data":{"object":{}}}`)

	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock: %v", err)
	}
	defer mock.Close()

	now := time.Date(2026, 5, 28, 12, 0, 0, 0, time.UTC)

	mock.ExpectQuery(`SELECT EXISTS\(SELECT 1 FROM webhook_events`).
		WithArgs("evt_inv_001").
		WillReturnRows(mock.NewRows([]string{"exists"}).AddRow(false))

	mock.ExpectExec(`INSERT INTO webhook_events`).
		WithArgs("evt_inv_001", "invoice.payment_succeeded", body).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))

	wh := &Webhook{secret: secret, db: mock, now: func() time.Time { return now }}
	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
	req.Header.Set("Stripe-Signature", newStripeHeader(secret, body, now.Unix()))

	w := httptest.NewRecorder()
	wh.Handle(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", w.Code)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unexpected mock calls: %v", err)
	}
}

// TestHandle_HMACMismatch_NoDB verifies that an invalid Stripe-Signature header returns
// HTTP 400 with zero DB interaction — no state is mutated for unauthenticated callers.
func TestHandle_HMACMismatch_NoDB(t *testing.T) {
	const secret = "whsec_real_secret"
	body := []byte(`{"id":"evt_hmac_001","type":"customer.subscription.deleted","data":{"object":{}}}`)

	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock: %v", err)
	}
	defer mock.Close()

	now := time.Date(2026, 5, 28, 12, 0, 0, 0, time.UTC)
	wh := &Webhook{secret: secret, db: mock, now: func() time.Time { return now }}

	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
	req.Header.Set("Stripe-Signature", newStripeHeader("whsec_wrong_secret", body, now.Unix()))

	w := httptest.NewRecorder()
	wh.Handle(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
	// Verify the 400 body indicates a signature problem, not a business logic error.
	var respBody map[string]any
	if err := json.NewDecoder(w.Body).Decode(&respBody); err == nil {
		if _, ok := respBody["error"]; !ok {
			t.Errorf("response body missing error field: %v", respBody)
		}
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unexpected mock calls: %v", err)
	}
}

// spyCacheDeleter records Del calls so tests can assert cache invalidation was attempted.
type spyCacheDeleter struct {
	calls [][]string
}

func (s *spyCacheDeleter) Del(_ context.Context, keys ...string) error {
	s.calls = append(s.calls, append([]string(nil), keys...))
	return nil
}

// TestHandleCheckoutSessionCompleted verifies that the handler writes stripe_customer_id
// onto the matching customers row and attempts Redis cache invalidation.
func TestHandleCheckoutSessionCompleted(t *testing.T) {
	const (
		customerID       = "550e8400-e29b-41d4-a716-446655440001"
		stripeCustomerID = "cus_checkout_001"
		prefix           = "cru_live_abc123456789012"
	)

	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock: %v", err)
	}
	defer mock.Close()

	mock.ExpectExec(`UPDATE customers SET stripe_customer_id`).
		WithArgs(stripeCustomerID, customerID).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))

	mock.ExpectQuery(`SELECT prefix FROM api_keys WHERE customer_id`).
		WithArgs(customerID, 1000).
		WillReturnRows(mock.NewRows([]string{"prefix"}).AddRow(prefix))

	spy := &spyCacheDeleter{}
	h := &Webhook{db: mock, now: time.Now, cache: spy}

	obj := json.RawMessage(fmt.Sprintf(`{"client_reference_id":%q,"customer":%q}`, customerID, stripeCustomerID))
	event := &stripeEvent{
		ID:   "evt_checkout_001",
		Type: "checkout.session.completed",
		Data: stripeEventData{Object: obj},
	}

	if err := h.handleCheckoutSessionCompleted(context.Background(), event); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Assert the cache invalidation was attempted for the customer's key prefix.
	if len(spy.calls) == 0 {
		t.Fatal("expected cache invalidation to be attempted")
	}
	wantKey := "auth:" + prefix
	found := false
	for _, k := range spy.calls[0] {
		if k == wantKey {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected DEL %q in cache invalidation keys, got %v", wantKey, spy.calls[0])
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("mock expectations not met: %v", err)
	}
}

// TestHandleCheckoutSessionCompleted_ForgedSignature verifies that a forged
// checkout.session.completed event (bad HMAC) is rejected with HTTP 400 and
// the existing 5-minute replay window is unchanged.
func TestHandleCheckoutSessionCompleted_ForgedSignature(t *testing.T) {
	const secret = "whsec_checkout_secret"
	body := []byte(`{"id":"evt_forge_001","type":"checkout.session.completed","data":{"object":{"client_reference_id":"uuid","customer":"cus_xxx"}}}`)

	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock: %v", err)
	}
	defer mock.Close()

	now := time.Date(2026, 6, 7, 12, 0, 0, 0, time.UTC)
	wh := &Webhook{secret: secret, db: mock, now: func() time.Time { return now }}

	req := httptest.NewRequest(http.MethodPost, "/webhooks/stripe", bytes.NewReader(body))
	req.Header.Set("Stripe-Signature", newStripeHeader("whsec_wrong_secret", body, now.Unix()))

	w := httptest.NewRecorder()
	wh.Handle(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for forged checkout.session.completed", w.Code)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unexpected mock calls: %v", err)
	}
}

// TestHandleCheckoutSessionCompleted_ReplayWindowRejected confirms the 5-minute
// replay window is enforced for checkout.session.completed (same as all event types).
func TestHandleCheckoutSessionCompleted_ReplayWindowRejected(t *testing.T) {
	const secret = "whsec_replay_secret"
	body := []byte(`{"id":"evt_replay_001","type":"checkout.session.completed","data":{"object":{}}}`)

	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock: %v", err)
	}
	defer mock.Close()

	now := time.Date(2026, 6, 7, 12, 0, 0, 0, time.UTC)
	// Sign with a timestamp 10 minutes in the past — beyond the 5-minute window.
	oldTs := now.Add(-10 * time.Minute).Unix()
	wh := &Webhook{secret: secret, db: mock, now: func() time.Time { return now }}

	req := httptest.NewRequest(http.MethodPost, "/webhooks/stripe", bytes.NewReader(body))
	req.Header.Set("Stripe-Signature", newStripeHeader(secret, body, oldTs))

	w := httptest.NewRecorder()
	wh.Handle(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for expired checkout.session.completed", w.Code)
	}
}

// TestHandleCustomerCreated verifies that the customer.created handler writes
// stripe_customer_id onto the matching row (matched by email) and invalidates cache.
func TestHandleCustomerCreated(t *testing.T) {
	const (
		customerID       = "550e8400-e29b-41d4-a716-446655440002"
		stripeCustomerID = "cus_created_001"
		email            = "test@example.com"
		prefix           = "cru_live_def456789012345"
	)

	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock: %v", err)
	}
	defer mock.Close()

	// QueryRow to fetch our customer UUID by email (case-insensitive LOWER comparison).
	mock.ExpectQuery(`SELECT id FROM customers WHERE LOWER`).
		WithArgs(email).
		WillReturnRows(mock.NewRows([]string{"id"}).AddRow(customerID))

	// Exec to write stripe_customer_id using the UUID (not email) as the WHERE key.
	mock.ExpectExec(`UPDATE customers SET stripe_customer_id`).
		WithArgs(stripeCustomerID, customerID).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))

	// Query for cache invalidation prefixes.
	mock.ExpectQuery(`SELECT prefix FROM api_keys WHERE customer_id`).
		WithArgs(customerID, 1000).
		WillReturnRows(mock.NewRows([]string{"prefix"}).AddRow(prefix))

	spy := &spyCacheDeleter{}
	h := &Webhook{db: mock, now: time.Now, cache: spy}

	obj := json.RawMessage(fmt.Sprintf(`{"id":%q,"email":%q}`, stripeCustomerID, email))
	event := &stripeEvent{
		ID:   "evt_cust_created_001",
		Type: "customer.created",
		Data: stripeEventData{Object: obj},
	}

	if err := h.handleCustomerCreated(context.Background(), event); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(spy.calls) == 0 {
		t.Fatal("expected cache invalidation to be attempted")
	}
	wantKey := "auth:" + prefix
	found := false
	for _, k := range spy.calls[0] {
		if k == wantKey {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected DEL %q in cache invalidation keys, got %v", wantKey, spy.calls[0])
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("mock expectations not met: %v", err)
	}
}

// TestInvalidateCustomerCache_Batching verifies that more than batchSize (100) prefixes
// are flushed in multiple Redis DEL calls rather than a single unbounded command.
func TestInvalidateCustomerCache_Batching(t *testing.T) {
	const (
		customerID = "550e8400-e29b-41d4-a716-446655440099"
		nPrefixes  = 101 // just over batchSize=100
	)

	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock: %v", err)
	}
	defer mock.Close()

	rows := mock.NewRows([]string{"prefix"})
	for i := range nPrefixes {
		rows.AddRow(fmt.Sprintf("prefix%03d", i))
	}
	mock.ExpectQuery(`SELECT prefix FROM api_keys WHERE customer_id`).
		WithArgs(customerID, 1000).
		WillReturnRows(rows)

	spy := &spyCacheDeleter{}
	h := &Webhook{db: mock, cache: spy, now: time.Now}
	h.invalidateCustomerCache(context.Background(), customerID)

	if len(spy.calls) != 2 {
		t.Fatalf("expected 2 DEL batches for %d prefixes, got %d", nPrefixes, len(spy.calls))
	}
	if len(spy.calls[0]) != 100 {
		t.Errorf("first batch: got %d keys, want 100", len(spy.calls[0]))
	}
	if len(spy.calls[1]) != 1 {
		t.Errorf("second batch: got %d keys, want 1", len(spy.calls[1]))
	}
	// Spot-check key format.
	wantKey := "auth:prefix000"
	if spy.calls[0][0] != wantKey {
		t.Errorf("first key = %q, want %q", spy.calls[0][0], wantKey)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("mock expectations: %v", err)
	}
}

// TestHandleCheckoutSessionCompleted_NoCacheDeleter verifies that the handler still
// succeeds (no panic) when no CacheDeleter is configured (nil h.cache).
func TestHandleCheckoutSessionCompleted_NoCacheDeleter(t *testing.T) {
	const (
		customerID       = "550e8400-e29b-41d4-a716-446655440003"
		stripeCustomerID = "cus_nocache_001"
	)

	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock: %v", err)
	}
	defer mock.Close()

	mock.ExpectExec(`UPDATE customers SET stripe_customer_id`).
		WithArgs(stripeCustomerID, customerID).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))

	// No cache deleter → invalidateCustomerCache returns early; no Query expected.
	h := &Webhook{db: mock, now: time.Now, cache: nil}

	obj := json.RawMessage(fmt.Sprintf(`{"client_reference_id":%q,"customer":%q}`, customerID, stripeCustomerID))
	event := &stripeEvent{
		ID:   "evt_checkout_nocache",
		Type: "checkout.session.completed",
		Data: stripeEventData{Object: obj},
	}

	if err := h.handleCheckoutSessionCompleted(context.Background(), event); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("mock expectations not met: %v", err)
	}
}
