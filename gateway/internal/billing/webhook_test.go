package billing

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"testing"
	"time"
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
