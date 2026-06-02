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
	mac.Write([]byte(fmt.Sprintf("%d.%s", ts, body)))
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

			if err := h.handleSubscriptionDeleted(context.Background(), event); err != nil {
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
		name     string
		event    string
		customer string
		priceID  string
		planID   string
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

			mock.ExpectExec(`UPDATE customers`).
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

			if err := h.handleSubscriptionUpsert(context.Background(), event); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Errorf("mock expectations not met: %v", err)
			}
		})
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
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unexpected mock calls: %v", err)
	}
}
