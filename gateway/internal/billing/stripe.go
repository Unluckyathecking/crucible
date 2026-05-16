// Package billing wraps Stripe's HTTP API for the gateway. We talk to Stripe over plain
// HTTP/form-encoded rather than the stripe-go SDK — fewer dep-version moving parts, and
// the surface we use (meter_events) is small.
package billing

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const stripeAPIBase = "https://api.stripe.com/v1"

type StripeClient struct {
	secretKey string
	meterName string
	http      *http.Client
}

func NewStripeClient(secretKey, meterName string) *StripeClient {
	return &StripeClient{
		secretKey: secretKey,
		meterName: meterName,
		http:      &http.Client{Timeout: 10 * time.Second},
	}
}

// EmitMeterEvent POSTs a usage event to Stripe Billing's meter_events endpoint.
// idempotencyKey ensures retries don't double-count.
func (s *StripeClient) EmitMeterEvent(ctx context.Context, stripeCustomerID string, units uint64, idempotencyKey string) error {
	form := url.Values{}
	form.Set("event_name", s.meterName)
	form.Set("payload[stripe_customer_id]", stripeCustomerID)
	form.Set("payload[value]", fmt.Sprintf("%d", units))
	form.Set("identifier", idempotencyKey)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		stripeAPIBase+"/billing/meter_events", strings.NewReader(form.Encode()))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+s.secretKey)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Idempotency-Key", idempotencyKey)

	resp, err := s.http.Do(req)
	if err != nil {
		return fmt.Errorf("stripe call: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	return fmt.Errorf("stripe meter_event returned status %d", resp.StatusCode)
}
