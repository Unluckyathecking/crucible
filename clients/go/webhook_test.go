package crucible_test

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
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
	mac.Write([]byte(timestamp))
	mac.Write([]byte("."))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

func TestVerifyWebhook_valid(t *testing.T) {
	secret := make([]byte, 32)
	for i := range secret {
		secret[i] = byte(i + 1)
	}
	secretHex := hex.EncodeToString(secret)
	body := []byte(`{"event":"delivery.succeeded","data":{"id":1}}`)
	ts := fmt.Sprintf("%d", time.Now().Unix())
	sig := testSign(secret, ts, body)
	header := "t=" + ts + ",v1=" + sig

	if err := crucible.VerifyWebhook(secretHex, header, body, 5*time.Minute); err != nil {
		t.Fatalf("VerifyWebhook: %v", err)
	}
}

func TestVerifyWebhook_tamperedBody(t *testing.T) {
	secret := make([]byte, 32)
	secretHex := hex.EncodeToString(secret)
	body := []byte(`{"event":"original"}`)
	ts := fmt.Sprintf("%d", time.Now().Unix())
	sig := testSign(secret, ts, body)
	header := "t=" + ts + ",v1=" + sig

	err := crucible.VerifyWebhook(secretHex, header, []byte(`{"event":"tampered"}`), 5*time.Minute)
	if err == nil {
		t.Fatal("expected error for tampered body, got nil")
	}
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
	ts := fmt.Sprintf("%d", time.Now().Unix())
	sig := testSign(wrongSecret, ts, body)
	header := "t=" + ts + ",v1=" + sig

	if err := crucible.VerifyWebhook(secretHex, header, body, 5*time.Minute); err == nil {
		t.Fatal("expected error for wrong secret, got nil")
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

	if err := crucible.VerifyWebhook(secretHex, header, body, 5*time.Minute); err == nil {
		t.Fatal("expected error for expired timestamp, got nil")
	}
}

func TestVerifyWebhook_multipleV1Candidates_secondValid(t *testing.T) {
	secret := make([]byte, 32)
	for i := range secret {
		secret[i] = 0x42
	}
	secretHex := hex.EncodeToString(secret)
	body := []byte(`{"event":"multi"}`)
	ts := fmt.Sprintf("%d", time.Now().Unix())
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
	ts := fmt.Sprintf("%d", time.Now().Unix())
	validSig := testSign(secret, ts, body)

	parts := []string{"t=" + ts}
	for i := 0; i < 8; i++ {
		parts = append(parts, "v1="+strings.Repeat("b", 64))
	}
	parts = append(parts, "v1="+validSig)
	header := strings.Join(parts, ",")

	if err := crucible.VerifyWebhook(secretHex, header, body, 5*time.Minute); err == nil {
		t.Fatal("expected error when valid sig is beyond maxSigCandidates, got nil")
	}
}

func TestVerifyWebhook_missingHeader(t *testing.T) {
	if err := crucible.VerifyWebhook("aabb", "", []byte("body"), 5*time.Minute); err == nil {
		t.Fatal("expected error for missing header, got nil")
	}
}
