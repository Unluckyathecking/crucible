package billing

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// mockTransport captures the request and returns a configurable response for
// testing Stripe HTTP calls without hitting the real Stripe API.
type mockTransport struct {
	statusCode int
	body       string
	lastReq    *http.Request
	lastBody   []byte
}

func (m *mockTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	m.lastReq = req
	if req.Body != nil {
		m.lastBody, _ = io.ReadAll(req.Body)
		// Restore body so http.Client can read it if needed.
		req.Body = io.NopCloser(strings.NewReader(string(m.lastBody)))
	}
	resp := &http.Response{
		StatusCode: m.statusCode,
		Body:       io.NopCloser(strings.NewReader(m.body)),
		Header:     make(http.Header),
		Proto:      "HTTP/1.1",
		ProtoMajor: 1,
		ProtoMinor: 1,
	}
	return resp, nil
}

func TestEmitMeterEvent_PayloadAndIdempotency(t *testing.T) {
	tests := []struct {
		name             string
		secretKey        string
		meterName        string
		stripeCustomerID string
		units            uint64
		idempotencyKey   string
		statusCode       int
		wantErr          bool
		errContains      string
	}{
		{
			name:             "success with correct payload",
			secretKey:        "sk_test_abc123",
			meterName:        "requests",
			stripeCustomerID: "cus_xyz",
			units:            42,
			idempotencyKey:   "idem_2026_001",
			statusCode:       200,
			wantErr:          false,
		},
		{
			name:             "server error 500",
			secretKey:        "sk_test_abc123",
			meterName:        "requests",
			stripeCustomerID: "cus_err",
			units:            1,
			idempotencyKey:   "idem_err",
			statusCode:       500,
			wantErr:          true,
			errContains:      "status 500",
		},
		{
			name:             "server error 429 rate limited",
			secretKey:        "sk_test_abc123",
			meterName:        "requests",
			stripeCustomerID: "cus_rl",
			units:            1,
			idempotencyKey:   "idem_rl",
			statusCode:       429,
			wantErr:          true,
			errContains:      "status 429",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			mt := &mockTransport{statusCode: tc.statusCode}

			client := &StripeClient{
				secretKey: tc.secretKey,
				meterName: tc.meterName,
				http:      &http.Client{Transport: mt},
			}

			err := client.EmitMeterEvent(context.Background(),
				tc.stripeCustomerID, tc.units, tc.idempotencyKey)

			if tc.wantErr && err == nil {
				t.Fatal("expected error but got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tc.wantErr && tc.errContains != "" && err != nil {
				if !strings.Contains(err.Error(), tc.errContains) {
					t.Errorf("error %q does not contain %q", err.Error(), tc.errContains)
				}
			}

			// Verify the HTTP request properties.
			if mt.lastReq == nil {
				t.Fatal("expected an HTTP request to be made")
			}

			// Method should be POST.
			if mt.lastReq.Method != http.MethodPost {
				t.Errorf("method = %s, want %s", mt.lastReq.Method, http.MethodPost)
			}

			// Path should be /v1/billing/meter_events.
			if mt.lastReq.URL.Path != "/v1/billing/meter_events" {
				t.Errorf("path = %s, want /v1/billing/meter_events", mt.lastReq.URL.Path)
			}

			// Authorization header.
			wantAuth := "Bearer " + tc.secretKey
			if got := mt.lastReq.Header.Get("Authorization"); got != wantAuth {
				t.Errorf("Authorization = %q, want %q", got, wantAuth)
			}

			// Content-Type.
			if ct := mt.lastReq.Header.Get("Content-Type"); ct != "application/x-www-form-urlencoded" {
				t.Errorf("Content-Type = %q, want application/x-www-form-urlencoded", ct)
			}

			// Idempotency-Key header.
			if ik := mt.lastReq.Header.Get("Idempotency-Key"); ik != tc.idempotencyKey {
				t.Errorf("Idempotency-Key = %q, want %q", ik, tc.idempotencyKey)
			}

			// Verify form-encoded body contents.
			if len(mt.lastBody) == 0 {
				t.Fatal("expected non-empty request body")
			}
			form, err := url.ParseQuery(string(mt.lastBody))
			if err != nil {
				t.Fatalf("failed to parse form body: %v", err)
			}

			if got := form.Get("event_name"); got != tc.meterName {
				t.Errorf("event_name = %q, want %q", got, tc.meterName)
			}
			if got := form.Get("payload[stripe_customer_id]"); got != tc.stripeCustomerID {
				t.Errorf("payload[stripe_customer_id] = %q, want %q", got, tc.stripeCustomerID)
			}
			wantValue := fmt.Sprintf("%d", tc.units)
			if got := form.Get("payload[value]"); got != wantValue {
				t.Errorf("payload[value] = %q, want %q", got, wantValue)
			}
			if got := form.Get("identifier"); got != tc.idempotencyKey {
				t.Errorf("identifier = %q, want %q", got, tc.idempotencyKey)
			}
		})
	}
}


