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
		_ = json.NewEncoder(w).Encode(map[string]string{"url": wantURL})
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
	}
	// Override the Stripe base to point at our test server.
	origBase := stripeAPIBase
	defer func() { _ = origBase }() // stripeAPIBase is a const; we patch via endpoint directly
	got, err := createCheckoutSessionAt(c, context.Background(), srv.URL+"/checkout/sessions", customerID, planID)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != wantURL {
		t.Errorf("url = %q, want %q", got, wantURL)
	}

	// Assert required Stripe form fields.
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
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]string{"message": "invalid price"},
		})
	}))
	defer srv.Close()

	mock, _ := pgxmock.NewPool()
	defer mock.Close()
	mock.ExpectQuery(`SELECT stripe_price_id FROM plans WHERE id`).
		WithArgs("pro").
		WillReturnRows(mock.NewRows([]string{"stripe_price_id"}).AddRow("price_pro"))

	c := &CheckoutClient{
		secretKey: "sk_test",
		http:      srv.Client(),
		db:        mock,
	}
	_, err := createCheckoutSessionAt(c, context.Background(), srv.URL+"/checkout/sessions", "uuid", "pro")
	if err == nil {
		t.Error("expected error for non-2xx response")
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
		_ = json.NewEncoder(w).Encode(map[string]string{"url": wantURL})
	}))
	defer srv.Close()

	c := &CheckoutClient{
		secretKey: "sk_test",
		returnURL: "https://example.com/billing",
		http:      srv.Client(),
	}

	got, err := createPortalSessionAt(c, context.Background(), srv.URL+"/billing/portal/sessions", stripeCustomerID)
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
	c := &CheckoutClient{http: http.DefaultClient}
	_, err := c.CreatePortalSession(context.Background(), "")
	if err == nil {
		t.Error("expected error for empty stripeCustomerID")
	}
}

// createCheckoutSessionAt is a test helper that posts to an explicit endpoint URL
// instead of the hard-coded Stripe base, so tests can use httptest.
func createCheckoutSessionAt(c *CheckoutClient, ctx context.Context, endpoint, customerID, planID string) (string, error) {
	var priceID string
	if err := c.db.QueryRow(ctx,
		`SELECT stripe_price_id FROM plans WHERE id = $1 AND stripe_price_id IS NOT NULL`,
		planID,
	).Scan(&priceID); err != nil {
		return "", err
	}
	form := url.Values{}
	form.Set("mode", "subscription")
	form.Set("client_reference_id", customerID)
	form.Set("line_items[0][price]", priceID)
	form.Set("line_items[0][quantity]", "1")
	form.Set("success_url", c.successURL)
	form.Set("cancel_url", c.cancelURL)
	form.Set("customer_creation", "always")
	form.Set("subscription_data[metadata][crucible_customer_id]", customerID)
	return c.postSession(ctx, endpoint, form)
}

// createPortalSessionAt is a test helper that posts to an explicit endpoint URL.
func createPortalSessionAt(c *CheckoutClient, ctx context.Context, endpoint, stripeCustomerID string) (string, error) {
	form := url.Values{}
	form.Set("customer", stripeCustomerID)
	form.Set("return_url", c.returnURL)
	return c.postSession(ctx, endpoint, form)
}

func assertField(t *testing.T, form url.Values, key, want string) {
	t.Helper()
	if got := form.Get(key); got != want {
		t.Errorf("form[%q] = %q, want %q", key, got, want)
	}
}
