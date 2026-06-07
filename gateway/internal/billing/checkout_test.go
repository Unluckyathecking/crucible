package billing

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/pashagolub/pgxmock/v5"
)

func TestCreateCheckoutSession(t *testing.T) {
	const (
		secret     = "sk_test_checkout"
		customerID = "550e8400-e29b-41d4-a716-446655440000"
		planID  = "pro"
		priceID = "price_pro"
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
	assertField(t, capturedForm, "customer_creation", "always")
	assertField(t, capturedForm, "subscription_data[metadata][crucible_customer_id]", customerID)
	assertField(t, capturedForm, "success_url", "https://example.com/success")
	assertField(t, capturedForm, "cancel_url", "https://example.com/cancel")

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("mock expectations: %v", err)
	}
}

func TestCreateCheckoutSession_PlanNotFound(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock: %v", err)
	}
	defer mock.Close()

	mock.ExpectQuery(`SELECT stripe_price_id FROM plans WHERE id`).
		WithArgs("unknown").
		WillReturnError(pgx.ErrNoRows)

	c := &CheckoutClient{db: mock}
	_, err = c.CreateCheckoutSession(context.Background(), "uuid", "unknown")
	if err == nil {
		t.Fatal("expected error for unknown plan")
	}
	if !errors.Is(err, ErrPlanNotFound) {
		t.Errorf("expected ErrPlanNotFound, got: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("mock expectations: %v", err)
	}
}

func TestCreateCheckoutSession_InvalidPriceIDFormat(t *testing.T) {
	// DB returns a value that doesn't match stripePriceIDRE (e.g. contains underscores
	// in the suffix or uses a forbidden prefix). The client must reject it before
	// making any Stripe API call.
	var called bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock: %v", err)
	}
	defer mock.Close()

	mock.ExpectQuery(`SELECT stripe_price_id FROM plans WHERE id`).
		WithArgs("pro").
		WillReturnRows(mock.NewRows([]string{"stripe_price_id"}).AddRow("invalid_price_id"))

	c := &CheckoutClient{
		http:    srv.Client(),
		db:      mock,
		baseURL: srv.URL,
	}
	_, err = c.CreateCheckoutSession(context.Background(), "uuid", "pro")
	if err == nil {
		t.Fatal("expected error for invalid price ID format")
	}
	if !strings.Contains(err.Error(), "invalid stripe_price_id format") {
		t.Errorf("expected invalid stripe_price_id format error, got: %v", err)
	}
	if called {
		t.Error("CreateCheckoutSession must not call Stripe when price ID format is invalid")
	}
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
		t.Fatal("expected error for non-2xx response")
	}
	if !strings.Contains(err.Error(), "stripe session error") {
		t.Errorf("expected stripe session error, got: %v", err)
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

func TestLookupStripeCustomerID(t *testing.T) {
	const customerID = "550e8400-e29b-41d4-a716-446655440000"

	t.Run("has stripe customer", func(t *testing.T) {
		mock, err := pgxmock.NewPool()
		if err != nil {
			t.Fatalf("pgxmock: %v", err)
		}
		defer mock.Close()
		want := "cus_test123"
		mock.ExpectQuery(`SELECT stripe_customer_id FROM customers WHERE id`).
			WithArgs(customerID).
			WillReturnRows(mock.NewRows([]string{"stripe_customer_id"}).AddRow(&want))
		c := &CheckoutClient{db: mock}
		got, err := c.LookupStripeCustomerID(context.Background(), customerID)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Errorf("mock expectations: %v", err)
		}
	})

	t.Run("null stripe customer id", func(t *testing.T) {
		mock, err := pgxmock.NewPool()
		if err != nil {
			t.Fatalf("pgxmock: %v", err)
		}
		defer mock.Close()
		mock.ExpectQuery(`SELECT stripe_customer_id FROM customers WHERE id`).
			WithArgs(customerID).
			WillReturnRows(mock.NewRows([]string{"stripe_customer_id"}).AddRow(nil))
		c := &CheckoutClient{db: mock}
		got, err := c.LookupStripeCustomerID(context.Background(), customerID)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != "" {
			t.Errorf("got %q, want empty string", got)
		}
	})

	t.Run("customer not found", func(t *testing.T) {
		mock, err := pgxmock.NewPool()
		if err != nil {
			t.Fatalf("pgxmock: %v", err)
		}
		defer mock.Close()
		mock.ExpectQuery(`SELECT stripe_customer_id FROM customers WHERE id`).
			WithArgs(customerID).
			WillReturnError(pgx.ErrNoRows)
		c := &CheckoutClient{db: mock}
		got, err := c.LookupStripeCustomerID(context.Background(), customerID)
		if err != nil {
			t.Fatalf("pgx.ErrNoRows should return nil error, got: %v", err)
		}
		if got != "" {
			t.Errorf("got %q, want empty string for missing customer", got)
		}
	})

	t.Run("db error", func(t *testing.T) {
		mock, err := pgxmock.NewPool()
		if err != nil {
			t.Fatalf("pgxmock: %v", err)
		}
		defer mock.Close()
		mock.ExpectQuery(`SELECT stripe_customer_id FROM customers WHERE id`).
			WithArgs(customerID).
			WillReturnError(fmt.Errorf("connection lost"))
		c := &CheckoutClient{db: mock}
		_, err = c.LookupStripeCustomerID(context.Background(), customerID)
		if err == nil {
			t.Fatal("expected error for db failure")
		}
		if !strings.Contains(err.Error(), "lookup stripe customer") {
			t.Errorf("expected wrapped error, got: %v", err)
		}
	})
}

func assertField(t *testing.T, form url.Values, key, want string) {
	t.Helper()
	if got := form.Get(key); got != want {
		t.Errorf("form[%q] = %q, want %q", key, got, want)
	}
}
