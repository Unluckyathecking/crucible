package billing

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// stripePriceIDRE validates the format of a Stripe price ID fetched from the DB
// before passing it to Stripe's API, matching the format Stripe assigns.
var stripePriceIDRE = regexp.MustCompile(`^price_[a-zA-Z0-9_]+$`)

// ErrPlanNotFound is returned by CreateCheckoutSession when the requested plan
// does not exist in the plans table or has no stripe_price_id configured.
// Callers can distinguish this client error from Stripe API failures using errors.Is.
var ErrPlanNotFound = errors.New("plan not found")

// CheckoutClient creates Stripe Checkout and Billing Portal sessions using the
// same plain net/http form-encoded idiom as the rest of this package.
type CheckoutClient struct {
	secretKey  string
	successURL string
	cancelURL  string
	returnURL  string
	http       *http.Client
	db         db
	baseURL    string // overridden in tests to point at an httptest server
}

// NewCheckoutClient constructs a CheckoutClient. successURL and cancelURL are
// the URLs Stripe redirects to after a successful or canceled checkout.
// returnURL is where the portal sends the customer when they click "Return".
// database is used to resolve plan_id → stripe_price_id.
func NewCheckoutClient(secretKey, successURL, cancelURL, returnURL string, database db) *CheckoutClient {
	return &CheckoutClient{
		secretKey:  secretKey,
		successURL: successURL,
		cancelURL:  cancelURL,
		returnURL:  returnURL,
		http:       &http.Client{Timeout: 15 * time.Second},
		db:         database,
		baseURL:    stripeAPIBase,
	}
}

// stripeSessionResponse captures the Stripe Checkout Session or Portal Session response.
type stripeSessionResponse struct {
	URL   string `json:"url"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// CreateCheckoutSession creates a Stripe Checkout session for a customer upgrading to
// the given plan. client_reference_id is set to customerID so the checkout.session.completed
// webhook can link the Stripe customer back to our customer row.
func (c *CheckoutClient) CreateCheckoutSession(ctx context.Context, customerID, planID string) (string, error) {
	var priceID string
	if err := c.db.QueryRow(ctx,
		`SELECT stripe_price_id FROM plans WHERE id = $1 AND stripe_price_id IS NOT NULL`,
		planID,
	).Scan(&priceID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", fmt.Errorf("%w: %q", ErrPlanNotFound, planID)
		}
		return "", fmt.Errorf("resolve plan %q: %w", planID, err)
	}
	if !stripePriceIDRE.MatchString(priceID) {
		return "", fmt.Errorf("invalid stripe_price_id format for plan %q: %q", planID, priceID)
	}
	form := url.Values{}
	form.Set("mode", "subscription")
	form.Set("client_reference_id", customerID)
	form.Set("line_items[0][price]", priceID)
	form.Set("line_items[0][quantity]", "1")
	form.Set("success_url", c.successURL)
	form.Set("cancel_url", c.cancelURL)
	// customer_creation=always ensures a Stripe customer is created so customer.created
	// fires and we can link stripe_customer_id even before checkout.session.completed.
	form.Set("customer_creation", "always")
	// Embed our UUID in subscription metadata so that customer.subscription.created/updated
	// webhooks can correlate back to our customer if client_reference_id is absent.
	form.Set("subscription_data[metadata][crucible_customer_id]", customerID)

	return c.postSession(ctx, c.baseURL+"/checkout/sessions", form)
}

// CreatePortalSession creates a Stripe Billing Portal session for an existing Stripe customer.
func (c *CheckoutClient) CreatePortalSession(ctx context.Context, stripeCustomerID string) (string, error) {
	if stripeCustomerID == "" {
		return "", errors.New("no stripe customer id: customer has not completed checkout")
	}
	form := url.Values{}
	form.Set("customer", stripeCustomerID)
	form.Set("return_url", c.returnURL)

	return c.postSession(ctx, c.baseURL+"/billing/portal/sessions", form)
}

// LookupStripeCustomerID fetches the stripe_customer_id for the given internal
// customer UUID. Returns "" if the customer has no stripe link yet (or does not exist).
func (c *CheckoutClient) LookupStripeCustomerID(ctx context.Context, customerID string) (string, error) {
	var stripeCustomerID *string
	if err := c.db.QueryRow(ctx,
		`SELECT stripe_customer_id FROM customers WHERE id = $1`,
		customerID,
	).Scan(&stripeCustomerID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", nil
		}
		return "", fmt.Errorf("lookup stripe customer: %w", err)
	}
	if stripeCustomerID == nil {
		return "", nil
	}
	return *stripeCustomerID, nil
}

func (c *CheckoutClient) postSession(ctx context.Context, endpoint string, form url.Values) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return "", fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.secretKey)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("stripe call: %w", err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}()

	var sr stripeSessionResponse
	if err := json.NewDecoder(resp.Body).Decode(&sr); err != nil {
		return "", fmt.Errorf("decode stripe response: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		msg := fmt.Sprintf("status %d", resp.StatusCode)
		if sr.Error != nil && sr.Error.Message != "" {
			msg += ": " + sr.Error.Message
		}
		return "", fmt.Errorf("stripe session error: %s", msg)
	}
	if sr.URL == "" {
		return "", fmt.Errorf("stripe %s returned empty url", endpoint)
	}
	return sr.URL, nil
}
