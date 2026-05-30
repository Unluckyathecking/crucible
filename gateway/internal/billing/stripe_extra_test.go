package billing

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"
)

// TestNewStripeClient verifies that NewStripeClient initialises the struct correctly.
func TestNewStripeClient(t *testing.T) {
	c := NewStripeClient("sk_test_abc", "api_requests")
	if c.secretKey != "sk_test_abc" {
		t.Errorf("secretKey = %q, want %q", c.secretKey, "sk_test_abc")
	}
	if c.meterName != "api_requests" {
		t.Errorf("meterName = %q, want %q", c.meterName, "api_requests")
	}
	if c.http == nil {
		t.Error("http client should not be nil")
	}
}

// TestEmitMeterEvent_HTTPTestServer exercises EmitMeterEvent against an
// httptest.Server, asserting:
//   - the Idempotency-Key header equals the idempotencyKey argument
//   - the payload[value] form field equals the units argument
//   - success returns nil
//   - 4xx/5xx returns an error containing the status code
func TestEmitMeterEvent_HTTPTestServer(t *testing.T) {
	tests := []struct {
		name           string
		units          uint64
		idempotencyKey string
		respondStatus  int
		wantErr        bool
		errContains    string
	}{
		{
			name:           "200 success",
			units:          42,
			idempotencyKey: "batch_aaa_001",
			respondStatus:  http.StatusOK,
			wantErr:        false,
		},
		{
			name:           "201 created is also success",
			units:          7,
			idempotencyKey: "batch_aaa_002",
			respondStatus:  http.StatusCreated,
			wantErr:        false,
		},
		{
			name:           "400 bad request returns error",
			units:          1,
			idempotencyKey: "batch_bad_001",
			respondStatus:  http.StatusBadRequest,
			wantErr:        true,
			errContains:    "400",
		},
		{
			name:           "500 server error returns error",
			units:          1,
			idempotencyKey: "batch_err_001",
			respondStatus:  http.StatusInternalServerError,
			wantErr:        true,
			errContains:    "500",
		},
		{
			name:           "429 rate-limit returns error",
			units:          1,
			idempotencyKey: "batch_rl_001",
			respondStatus:  http.StatusTooManyRequests,
			wantErr:        true,
			errContains:    "429",
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			var capturedReq *http.Request
			var capturedBody []byte

			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				capturedReq = r
				if err := r.ParseForm(); err == nil {
					capturedBody = []byte(r.Form.Encode())
				}
				w.WriteHeader(tc.respondStatus)
			}))
			defer srv.Close()

			client := &StripeClient{
				secretKey: "sk_test_httptest",
				meterName: "api_calls",
				http:      &http.Client{Timeout: 5 * time.Second},
			}
			// Override base URL to point at the test server.
			origBase := stripeAPIBase
			_ = origBase // keep linter happy
			// We construct the request directly using a custom transport that rewrites the host.
			client.http.Transport = &rewriteHostTransport{
				base:    srv.URL,
				wrapped: http.DefaultTransport,
			}

			err := client.EmitMeterEvent(context.Background(), "cus_httptest", tc.units, tc.idempotencyKey)

			if tc.wantErr {
				if err == nil {
					t.Fatal("expected error but got nil")
				}
				if tc.errContains != "" {
					if got := err.Error(); !containsString(got, tc.errContains) {
						t.Errorf("error %q does not contain %q", got, tc.errContains)
					}
				}
			} else {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
			}

			if capturedReq == nil {
				t.Fatal("test server received no request")
			}

			// Assert idempotency key header.
			if got := capturedReq.Header.Get("Idempotency-Key"); got != tc.idempotencyKey {
				t.Errorf("Idempotency-Key = %q, want %q", got, tc.idempotencyKey)
			}

			// Assert Authorization header.
			if got := capturedReq.Header.Get("Authorization"); got != "Bearer sk_test_httptest" {
				t.Errorf("Authorization = %q, want %q", got, "Bearer sk_test_httptest")
			}

			// Parse and assert form body.
			form, parseErr := url.ParseQuery(string(capturedBody))
			if parseErr != nil {
				t.Fatalf("failed to parse form body: %v", parseErr)
			}
			wantValue := fmt.Sprintf("%d", tc.units)
			if got := form.Get("payload[value]"); got != wantValue {
				t.Errorf("payload[value] = %q, want %q", got, wantValue)
			}
			if got := form.Get("identifier"); got != tc.idempotencyKey {
				t.Errorf("identifier form field = %q, want %q", got, tc.idempotencyKey)
			}
			if got := form.Get("event_name"); got != "api_calls" {
				t.Errorf("event_name = %q, want %q", got, "api_calls")
			}
		})
	}
}

// TestEmitMeterEvent_IdempotencyKey_IsStable verifies that calling EmitMeterEvent
// twice with the same idempotencyKey sends the same Idempotency-Key header both times.
// This is the Stripe retry safety guarantee (invariant #4 from CLAUDE.md).
func TestEmitMeterEvent_IdempotencyKey_IsStable(t *testing.T) {
	const idemKey = "batch_uuid_stable_001"
	const units = uint64(99)

	var received []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received = append(received, r.Header.Get("Idempotency-Key"))
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	client := &StripeClient{
		secretKey: "sk_test_stable",
		meterName: "requests",
		http: &http.Client{
			Transport: &rewriteHostTransport{base: srv.URL, wrapped: http.DefaultTransport},
		},
	}

	for i := 0; i < 3; i++ {
		if err := client.EmitMeterEvent(context.Background(), "cus_stable", units, idemKey); err != nil {
			t.Fatalf("call %d: unexpected error: %v", i, err)
		}
	}

	if len(received) != 3 {
		t.Fatalf("expected 3 requests, got %d", len(received))
	}
	for i, k := range received {
		if k != idemKey {
			t.Errorf("call %d: Idempotency-Key = %q, want %q", i, k, idemKey)
		}
	}
}

// rewriteHostTransport rewrites the request URL host to point at a test server
// while leaving the path intact, enabling StripeClient to be tested without
// changing the stripeAPIBase constant.
type rewriteHostTransport struct {
	base    string // e.g. "http://127.0.0.1:PORT"
	wrapped http.RoundTripper
}

func (r *rewriteHostTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	cloned := req.Clone(req.Context())
	parsed, err := url.Parse(r.base)
	if err != nil {
		return nil, err
	}
	cloned.URL.Scheme = parsed.Scheme
	cloned.URL.Host = parsed.Host
	return r.wrapped.RoundTrip(cloned)
}

func containsString(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(sub) == 0 ||
		func() bool {
			for i := 0; i <= len(s)-len(sub); i++ {
				if s[i:i+len(sub)] == sub {
					return true
				}
			}
			return false
		}())
}
