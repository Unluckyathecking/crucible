package crucible_test

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	crucible "github.com/Unluckyathecking/crucible/clients/go"
)

// testSign replicates gateway/internal/webhookout.Sign locally so tests build
// the positive vector without importing the gateway package tree.
func testSign(secret []byte, timestamp string, body []byte) string {
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write([]byte(timestamp))
	_, _ = mac.Write([]byte("."))
	_, _ = mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

// nowTS returns the current Unix timestamp as a decimal string.
func nowTS() string { return fmt.Sprintf("%d", time.Now().Unix()) }

func mustBeWebhookError(t *testing.T, err error) *crucible.WebhookError {
	t.Helper()
	var wErr *crucible.WebhookError
	if !errors.As(err, &wErr) {
		t.Fatalf("expected *crucible.WebhookError, got %T: %v", err, err)
	}
	return wErr
}

func TestVerifyWebhook_valid(t *testing.T) {
	secret := make([]byte, 32)
	for i := range secret {
		secret[i] = byte(i + 1)
	}
	secretHex := hex.EncodeToString(secret)
	body := []byte(`{"event":"delivery.succeeded","data":{"id":1}}`)
	ts := nowTS()
	sig := testSign(secret, ts, body)
	header := "t=" + ts + ",v1=" + sig

	if err := crucible.VerifyWebhook(secretHex, header, body, 5*time.Minute); err != nil {
		t.Fatalf("VerifyWebhook: %v", err)
	}
}

func TestVerifyWebhook_defaultTolerance(t *testing.T) {
	secret := make([]byte, 32)
	secretHex := hex.EncodeToString(secret)
	body := []byte(`{"event":"test"}`)
	ts := nowTS()
	sig := testSign(secret, ts, body)
	header := "t=" + ts + ",v1=" + sig

	// tolerance=0 should use DefaultTolerance (5 min) and accept a current timestamp
	if err := crucible.VerifyWebhook(secretHex, header, body, 0); err != nil {
		t.Fatalf("VerifyWebhook with tolerance=0: %v", err)
	}
}

func TestVerifyWebhook_tamperedBody(t *testing.T) {
	secret := make([]byte, 32)
	secretHex := hex.EncodeToString(secret)
	body := []byte(`{"event":"original"}`)
	ts := nowTS()
	sig := testSign(secret, ts, body)
	header := "t=" + ts + ",v1=" + sig

	err := crucible.VerifyWebhook(secretHex, header, []byte(`{"event":"tampered"}`), 5*time.Minute)
	if err == nil {
		t.Fatal("expected error for tampered body, got nil")
	}
	mustBeWebhookError(t, err)
}

func TestVerifyWebhook_wrongSecret(t *testing.T) {
	correctSecret := make([]byte, 32)
	for i := range correctSecret {
		correctSecret[i] = 0xAA
	}
	wrongSecret := make([]byte, 32)
	for i := range wrongSecret {
		wrongSecret[i] = 0xBB
	}
	secretHex := hex.EncodeToString(correctSecret)
	body := []byte(`{"event":"test"}`)
	ts := nowTS()
	sig := testSign(wrongSecret, ts, body)
	header := "t=" + ts + ",v1=" + sig

	err := crucible.VerifyWebhook(secretHex, header, body, 5*time.Minute)
	if err == nil {
		t.Fatal("expected error for wrong secret, got nil")
	}
	mustBeWebhookError(t, err)
}

func TestVerifyWebhook_futureTimestamp(t *testing.T) {
	secret := make([]byte, 32)
	secretHex := hex.EncodeToString(secret)
	body := []byte(`{"event":"test"}`)
	future := time.Now().Add(10 * time.Minute)
	ts := fmt.Sprintf("%d", future.Unix())
	sig := testSign(secret, ts, body)
	header := "t=" + ts + ",v1=" + sig

	err := crucible.VerifyWebhook(secretHex, header, body, 5*time.Minute)
	if err == nil {
		t.Fatal("expected error for future timestamp, got nil")
	}
	wErr := mustBeWebhookError(t, err)
	if !strings.Contains(wErr.Error(), "future") {
		t.Fatalf("expected error to mention 'future', got: %v", wErr)
	}
}

func TestVerifyWebhook_expiredTimestamp(t *testing.T) {
	secret := make([]byte, 32)
	secretHex := hex.EncodeToString(secret)
	body := []byte(`{"event":"test"}`)
	old := time.Now().Add(-10 * time.Minute)
	ts := fmt.Sprintf("%d", old.Unix())
	sig := testSign(secret, ts, body)
	header := "t=" + ts + ",v1=" + sig

	err := crucible.VerifyWebhook(secretHex, header, body, 5*time.Minute)
	if err == nil {
		t.Fatal("expected error for expired timestamp, got nil")
	}
	mustBeWebhookError(t, err)
}

func TestVerifyWebhook_multipleV1Candidates_secondValid(t *testing.T) {
	secret := make([]byte, 32)
	for i := range secret {
		secret[i] = 0x42
	}
	secretHex := hex.EncodeToString(secret)
	body := []byte(`{"event":"multi"}`)
	ts := nowTS()
	validSig := testSign(secret, ts, body)
	invalidSig := strings.Repeat("a", 64)
	header := "t=" + ts + ",v1=" + invalidSig + ",v1=" + validSig

	if err := crucible.VerifyWebhook(secretHex, header, body, 5*time.Minute); err != nil {
		t.Fatalf("VerifyWebhook with multiple candidates: %v", err)
	}
}

func TestVerifyWebhook_boundedCandidates(t *testing.T) {
	secret := make([]byte, 32)
	secretHex := hex.EncodeToString(secret)
	body := []byte(`{"event":"test"}`)
	ts := nowTS()
	validSig := testSign(secret, ts, body)

	parts := []string{"t=" + ts}
	for i := 0; i < 8; i++ {
		parts = append(parts, "v1="+strings.Repeat("b", 64))
	}
	parts = append(parts, "v1="+validSig)
	header := strings.Join(parts, ",")

	err := crucible.VerifyWebhook(secretHex, header, body, 5*time.Minute)
	if err == nil {
		t.Fatal("expected error when valid sig is beyond maxSigCandidates, got nil")
	}
	mustBeWebhookError(t, err)
}

func TestVerifyWebhook_missingHeader(t *testing.T) {
	err := crucible.VerifyWebhook("aabb", "", []byte("body"), 5*time.Minute)
	if err == nil {
		t.Fatal("expected error for missing header, got nil")
	}
	mustBeWebhookError(t, err)
}

func TestVerifyWebhook_invalidSecretHex(t *testing.T) {
	body := []byte(`{"event":"test"}`)
	ts := nowTS()

	for _, badSecret := range []string{"", "zz", "abc"} { // non-hex or odd-length
		err := crucible.VerifyWebhook(badSecret, "t="+ts+",v1="+strings.Repeat("a", 64), body, 5*time.Minute)
		if err == nil {
			t.Fatalf("expected error for invalid secretHex %q, got nil", badSecret)
		}
		mustBeWebhookError(t, err)
	}
}

func TestVerifyWebhook_negativeTolerance(t *testing.T) {
	secret := make([]byte, 32)
	secretHex := hex.EncodeToString(secret)
	body := []byte(`{"event":"test"}`)
	ts := nowTS()
	sig := testSign(secret, ts, body)
	header := "t=" + ts + ",v1=" + sig

	err := crucible.VerifyWebhook(secretHex, header, body, -1*time.Minute)
	if err == nil {
		t.Fatal("expected error for negative tolerance, got nil")
	}
	mustBeWebhookError(t, err)
}

func TestVerifyWebhook_emptyBody(t *testing.T) {
	secret := make([]byte, 32)
	for i := range secret {
		secret[i] = 0x11
	}
	secretHex := hex.EncodeToString(secret)
	body := []byte{}
	ts := nowTS()
	sig := testSign(secret, ts, body)
	header := "t=" + ts + ",v1=" + sig

	if err := crucible.VerifyWebhook(secretHex, header, body, 5*time.Minute); err != nil {
		t.Fatalf("VerifyWebhook with empty body: %v", err)
	}
}

func TestVerifyWebhook_malformedHeader_noTimestamp(t *testing.T) {
	secret := make([]byte, 32)
	secretHex := hex.EncodeToString(secret)
	body := []byte(`{"event":"test"}`)
	header := "v1=" + strings.Repeat("a", 64)

	err := crucible.VerifyWebhook(secretHex, header, body, 5*time.Minute)
	if err == nil {
		t.Fatal("expected error for header missing t=, got nil")
	}
	mustBeWebhookError(t, err)
}

func TestVerifyWebhook_malformedHeader_noSignature(t *testing.T) {
	secret := make([]byte, 32)
	secretHex := hex.EncodeToString(secret)
	body := []byte(`{"event":"test"}`)
	header := "t=" + nowTS()

	err := crucible.VerifyWebhook(secretHex, header, body, 5*time.Minute)
	if err == nil {
		t.Fatal("expected error for header missing v1=, got nil")
	}
	mustBeWebhookError(t, err)
}

func TestVerifyWebhook_malformedTimestamp(t *testing.T) {
	secret := make([]byte, 32)
	secretHex := hex.EncodeToString(secret)
	body := []byte(`{"event":"test"}`)

	for _, badTS := range []string{"abc", "1.5", "0x10", ""} {
		sig := strings.Repeat("a", 64)
		header := "t=" + badTS + ",v1=" + sig
		err := crucible.VerifyWebhook(secretHex, header, body, 5*time.Minute)
		if err == nil {
			t.Fatalf("expected error for malformed timestamp %q, got nil", badTS)
		}
		mustBeWebhookError(t, err)
	}
}
