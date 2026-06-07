package billing

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/pashagolub/pgxmock/v5"
)

func TestCreateCheckoutSession(t *testing.T) {
	const (
		secret     = "sk_test_checkout"
		customerID = "550e8400-e29b-41d4-a716-446655440000"
		planID     = "pro"
		priceID    = "price_pro_monthly"
		wantURL    = "https://checkout.stripe.com/pay/cs_test_abc123"
	)

	var capturedForm url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if err := r.ParseForm(); err != nil {
			t.Fatalf("parse form: %v", err)
		}
		capturedForm = r.Form
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(map[string]string{"url": wantURL}); err != nil {
			t.Fatalf("encode response: %v", err)
		}
	}))
	defer srv.Close()

	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock: %v", err)
	}
	defer mock.Close()

	mock.ExpectQuery(`SELECT stripe_price_id FROM plans WHERE id`).
		WithArgs(planID).
		WillReturnRows(mock.NewRows([]string{"stripe_price_id"}).AddRow(priceID))

	c := &CheckoutClient{
		secretKey:  secret,
		successURL: "https://example.com/success",
		cancelURL:  "https://example.com/cancel",
		returnURL:  "https://example.com/billing",
		http:       srv.Client(),
		db:         mock,
		baseURL:    srv.URL,
	}
	got, err := c.CreateCheckoutSession(context.Background(), customerID, planID)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != wantURL {
		t.Errorf("url = %q, want %q", got, wantURL)
	}

	assertField(t, capturedForm, "mode", "subscription")
	assertField(t, capturedForm, "client_reference_id", customerID)
	assertField(t, capturedForm, "line_items[0][price]", priceID)
	assertField(t, capturedForm, "line_items[0][quantity]", "1")
	assertField(t, capturedForm, "success_url", "https://example.com/success")
	assertField(t, capturedForm, "cancel_url", "https://example.com/cancel")

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("mock expectations: %v", err)
	}
}

func TestCreateCheckoutSession_NonOK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		if err := json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]string{"message": "invalid price"},
		}); err != nil {
			t.Fatalf("encode response: %v", err)
		}
	}))
	defer srv.Close()

	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock: %v", err)
	}
	defer mock.Close()

	mock.ExpectQuery(`SELECT stripe_price_id FROM plans WHERE id`).
		WithArgs("pro").
		WillReturnRows(mock.NewRows([]string{"stripe_price_id"}).AddRow("price_pro"))

	c := &CheckoutClient{
		secretKey: "sk_test",
		http:      srv.Client(),
		db:        mock,
		baseURL:   srv.URL,
	}
	_, err = c.CreateCheckoutSession(context.Background(), "uuid", "pro")
	if err == nil {
		t.Error("expected error for non-2xx response")
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("mock expectations not met: %v", err)
	}
}

func TestCreatePortalSession(t *testing.T) {
	const (
		stripeCustomerID = "cus_test123"
		wantURL          = "https://billing.stripe.com/session/test_abc"
	)

	var capturedForm url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Fatalf("parse form: %v", err)
		}
		capturedForm = r.Form
		if err := json.NewEncoder(w).Encode(map[string]string{"url": wantURL}); err != nil {
			t.Fatalf("encode response: %v", err)
		}
	}))
	defer srv.Close()

	c := &CheckoutClient{
		secretKey: "sk_test",
		returnURL: "https://example.com/billing",
		http:      srv.Client(),
		baseURL:   srv.URL,
	}

	got, err := c.CreatePortalSession(context.Background(), stripeCustomerID)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != wantURL {
		t.Errorf("url = %q, want %q", got, wantURL)
	}

	assertField(t, capturedForm, "customer", stripeCustomerID)
	assertField(t, capturedForm, "return_url", "https://example.com/billing")
}

func TestCreatePortalSession_EmptyCustomerID(t *testing.T) {
	// Canary server: if CreatePortalSession makes any HTTP call before validating,
	// the test will catch it. The empty-ID guard should fire before any network I/O.
	var called bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := &CheckoutClient{http: srv.Client(), baseURL: srv.URL}
	_, err := c.CreatePortalSession(context.Background(), "")
	if err == nil {
		t.Error("expected error for empty stripeCustomerID")
	}
	if called {
		t.Error("CreatePortalSession must not make HTTP calls when stripeCustomerID is empty")
	}
}

func assertField(t *testing.T, form url.Values, key, want string) {
	t.Helper()
	if got := form.Get(key); got != want {
		t.Errorf("form[%q] = %q, want %q", key, got, want)
	}
}
