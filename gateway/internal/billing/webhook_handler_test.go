package billing

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/pashagolub/pgxmock/v5"
)

// TestHandleSubscriptionDeleted is the regression guard for the "wrong-status downgrade" bug:
// only a "canceled" subscription should trigger the plan_id → "free" update.
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
				mock.ExpectExec(`UPDATE customers`).
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
				Data: stripeEventData{Object: obj},
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

// TestHandle_InvoicePaymentSucceeded_NoSideEffects verifies that an
// invoice.payment_succeeded event is acknowledged (HTTP 200) without touching
// any subscription state — it is not in the dispatch switch and must be ignored.
func TestHandle_InvoicePaymentSucceeded_NoSideEffects(t *testing.T) {
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
	req.Header.Set("Stripe-Signature", signStripe(secret, body, now.Unix()))

	w := httptest.NewRecorder()
	wh.Handle(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", w.Code)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unexpected mock calls: %v", err)
	}
}

// TestHandle_HMACMismatch_NoDB verifies that a bad Stripe-Signature header results in
// a 400 response with zero DB interaction — no events recorded, no state mutated.
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
	req.Header.Set("Stripe-Signature", signStripe("whsec_wrong_secret", body, now.Unix()))

	w := httptest.NewRecorder()
	wh.Handle(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unexpected mock calls: %v", err)
	}
}
