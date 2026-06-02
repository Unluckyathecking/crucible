package billing

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/pashagolub/pgxmock/v5"
)

func signStripe(secret string, body []byte, ts int64) string {
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(fmt.Sprintf("%d.%s", ts, body)))
	return fmt.Sprintf("t=%d,v1=%s", ts, hex.EncodeToString(mac.Sum(nil)))
}

func TestVerifySignature(t *testing.T) {
	const secret = "whsec_test_secret"
	body := []byte(`{"id":"evt_x","type":"customer.subscription.created"}`)
	now := time.Date(2026, 5, 16, 12, 0, 0, 0, time.UTC)
	h := &Webhook{secret: secret, now: func() time.Time { return now }}

	tests := []struct {
		name    string
		header  string
		wantErr bool
	}{
		{"valid", signStripe(secret, body, now.Unix()), false},
		{"wrong secret", signStripe("whsec_other", body, now.Unix()), true},
		{"too old", signStripe(secret, body, now.Add(-10*time.Minute).Unix()), true},
		{"missing header", "", true},
		{"malformed", "garbage", true},
		{"no v1", fmt.Sprintf("t=%d", now.Unix()), true},
		{"invalid hex in v1", fmt.Sprintf("t=%d,v1=gggggg", now.Unix()), true},
		{"invalid hex skipped but valid found", fmt.Sprintf("t=%d,v1=gggggg,%s", now.Unix(), signStripe(secret, body, now.Unix())[13:]), false},
		{"too long v1", fmt.Sprintf("t=%d,v1=%066x", now.Unix(), 1), true},
		{"wrong length v1", fmt.Sprintf("t=%d,v1=%062x", now.Unix(), 1), true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := h.VerifySignature(tc.header, body)
			if (err != nil) != tc.wantErr {
				t.Errorf("err=%v wantErr=%v", err, tc.wantErr)
			}
		})
	}
}

// TestVerifySignature_BoundedCandidates verifies that verifySignature only ever
// considers a small, bounded number of v1= candidates so an attacker cannot force
// unbounded constant-time HMAC comparisons on the unauthenticated webhook endpoint.
// Chosen semantics: only the first 8 v1= values are parsed/compared; a valid
// signature positioned after the cap is treated as not present and rejected.
func TestVerifySignature_BoundedCandidates(t *testing.T) {
	const secret = "whsec_bound_test"
	const cap = 8
	body := []byte(`{"id":"evt_bound","type":"customer.subscription.created"}`)
	now := time.Date(2026, 5, 16, 12, 0, 0, 0, time.UTC)
	h := &Webhook{secret: secret, now: func() time.Time { return now }}

	ts := now.Unix()
	valid := signStripe(secret, body, ts)
	validSig := valid[len(fmt.Sprintf("t=%d,v1=", ts)):] // bare 64-hex signature

	bogus := make([]string, 0, 1000)
	for i := 0; i < 1000; i++ {
		bogus = append(bogus, fmt.Sprintf("v1=%064x", i))
	}

	t.Run("1000 invalid candidates rejected", func(t *testing.T) {
		header := fmt.Sprintf("t=%d,%s", ts, joinComma(bogus))
		if err := h.VerifySignature(header, body); err == nil {
			t.Error("expected rejection for header full of invalid signatures")
		}
	})

	t.Run("valid within cap verifies", func(t *testing.T) {
		// valid at position 4 (0-indexed), within the cap of 8
		parts := append(append([]string{}, bogus[:3]...), "v1="+validSig)
		header := fmt.Sprintf("t=%d,%s", ts, joinComma(parts))
		if err := h.VerifySignature(header, body); err != nil {
			t.Errorf("expected valid signature within cap to verify, got %v", err)
		}
	})

	t.Run("valid beyond cap rejected", func(t *testing.T) {
		// fill the first `cap` slots with bogus, put the valid sig after the cap
		parts := append(append([]string{}, bogus[:cap]...), "v1="+validSig)
		header := fmt.Sprintf("t=%d,%s", ts, joinComma(parts))
		if err := h.VerifySignature(header, body); err == nil {
			t.Error("expected valid signature positioned beyond the cap to be rejected")
		}
	})
}

func joinComma(parts []string) string {
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += ","
		}
		out += p
	}
	return out
}

func TestVerifySignature_TamperedBody(t *testing.T) {
	const secret = "whsec_test"
	body := []byte(`{"id":"evt_x"}`)
	now := time.Now()
	h := &Webhook{secret: secret, now: func() time.Time { return now }}

	header := signStripe(secret, body, now.Unix())
	tampered := []byte(`{"id":"evt_y"}`)
	if err := h.VerifySignature(header, tampered); err == nil {
		t.Error("expected error when body tampered")
	}
}

func TestWebhook_Handle_DedupReturns200(t *testing.T) {
	const secret = "whsec_dedup_test"

	body := []byte(`{"id":"evt_dup_001","type":"invoice.payment_succeeded","data":{"object":{}}}`)

	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("failed to create mock pool: %v", err)
	}
	defer mock.Close()

	mock.ExpectQuery(`SELECT EXISTS\(SELECT 1 FROM webhook_events`).
		WithArgs("evt_dup_001").
		WillReturnRows(mock.NewRows([]string{"exists"}).AddRow(true))

	now := time.Date(2026, 5, 16, 12, 0, 0, 0, time.UTC)
	wh := &Webhook{
		secret: secret,
		db:     mock,
		now:    func() time.Time { return now },
	}

	sig := signStripe(secret, body, now.Unix())
	req := httptest.NewRequest("POST", "/webhook", bytes.NewReader(body))
	req.Header.Set("Stripe-Signature", sig)

	w := httptest.NewRecorder()
	wh.Handle(w, req)

	if w.Code != 200 {
		t.Errorf("status = %d, want 200", w.Code)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unexpected mock calls: %v", err)
	}
}

func TestWebhook_Handle_EventSeenErrorReturns500(t *testing.T) {
	const secret = "whsec_err_test"

	body := []byte(`{"id":"evt_err_001","type":"customer.subscription.created","data":{"object":{}}}`)

	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("failed to create mock pool: %v", err)
	}
	defer mock.Close()

	mock.ExpectQuery(`SELECT EXISTS\(SELECT 1 FROM webhook_events`).
		WithArgs("evt_err_001").
		WillReturnError(fmt.Errorf("db connection lost"))

	now := time.Date(2026, 5, 16, 12, 0, 0, 0, time.UTC)
	wh := &Webhook{
		secret: secret,
		db:     mock,
		now:    func() time.Time { return now },
	}

	sig := signStripe(secret, body, now.Unix())
	req := httptest.NewRequest("POST", "/webhook", bytes.NewReader(body))
	req.Header.Set("Stripe-Signature", sig)

	w := httptest.NewRecorder()
	wh.Handle(w, req)

	if w.Code != 500 {
		t.Errorf("status = %d, want 500", w.Code)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unexpected mock calls: %v", err)
	}
}
