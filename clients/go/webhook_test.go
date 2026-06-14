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

// sha256HexLen is the expected hex-encoded length of a SHA-256 digest (32 bytes × 2).
// Using sha256.Size*2 instead of the magic number 64 keeps tests resilient to algorithm changes.
const sha256HexLen = sha256.Size * 2

// testSign replicates gateway/internal/webhookout.Sign locally so tests build
// the positive vector without importing the gateway package tree.
// Three separate mac.Write calls mirror the production signing algorithm exactly.
func testSign(secret []byte, timestamp string, body []byte) string {
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write([]byte(timestamp))
	_, _ = mac.Write([]byte("."))
	_, _ = mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

// nowTS returns the current Unix timestamp as a decimal string.
func nowTS() string { return fmt.Sprintf("%d", time.Now().Unix()) }

// assertWebhookError asserts err is a *crucible.WebhookError. Use when the
// error message does not need further inspection.
func assertWebhookError(t *testing.T, err error) {
	t.Helper()
	var wErr *crucible.WebhookError
	if !errors.As(err, &wErr) {
		t.Fatalf("expected *crucible.WebhookError, got %T: %v", err, err)
	}
}

// mustBeWebhookError is like assertWebhookError but returns the typed error for
// message inspection. Use only when the test verifies specific error text.
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

	if err := crucible.VerifyWebhook(secretHex, header, body, crucible.DefaultTolerance); err != nil {
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
	assertWebhookError(t, err)
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
	wErr := mustBeWebhookError(t, err)
	if !strings.Contains(wErr.Error(), "no matching v1 signature") {
		t.Fatalf("expected 'no matching v1 signature', got: %v", wErr)
	}
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
	assertWebhookError(t, err)
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
	invalidSig := strings.Repeat("a", sha256HexLen)
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
		parts = append(parts, "v1="+strings.Repeat("b", sha256HexLen))
	}
	parts = append(parts, "v1="+validSig)
	header := strings.Join(parts, ",")

	err := crucible.VerifyWebhook(secretHex, header, body, 5*time.Minute)
	if err == nil {
		t.Fatal("expected error when valid sig is beyond maxSigCandidates, got nil")
	}
	wErr := mustBeWebhookError(t, err)
	// The 9th candidate (validSig) must have been dropped; the error should be
	// "no matching v1 signature", not "malformed header" or anything else.
	if !strings.Contains(wErr.Error(), "no matching v1 signature") {
		t.Fatalf("expected 'no matching v1 signature', got: %v", wErr)
	}
}

func TestVerifyWebhook_missingHeader(t *testing.T) {
	err := crucible.VerifyWebhook("aabb", "", []byte("body"), 5*time.Minute)
	if err == nil {
		t.Fatal("expected error for missing header, got nil")
	}
	assertWebhookError(t, err)
}

func TestVerifyWebhook_invalidSecretHex(t *testing.T) {
	body := []byte(`{"event":"test"}`)
	ts := nowTS()

	for _, badSecret := range []string{"", "zz", "abc"} { // non-hex or odd-length
		err := crucible.VerifyWebhook(badSecret, "t="+ts+",v1="+strings.Repeat("a", sha256HexLen), body, 5*time.Minute)
		if err == nil {
			t.Fatalf("expected error for invalid secretHex %q, got nil", badSecret)
		}
		assertWebhookError(t, err)
	}
}

func TestVerifyWebhook_uppercaseSecretHex(t *testing.T) {
	secret := make([]byte, 32)
	for i := range secret {
		secret[i] = byte(i + 1)
	}
	secretHexLower := hex.EncodeToString(secret)
	secretHexUpper := strings.ToUpper(secretHexLower)
	body := []byte(`{"event":"test"}`)
	ts := nowTS()
	sig := testSign(secret, ts, body)
	header := "t=" + ts + ",v1=" + sig

	if err := crucible.VerifyWebhook(secretHexUpper, header, body, 5*time.Minute); err != nil {
		t.Fatalf("VerifyWebhook with uppercase secretHex: %v", err)
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
	assertWebhookError(t, err)
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
	header := "v1=" + strings.Repeat("a", sha256HexLen)

	err := crucible.VerifyWebhook(secretHex, header, body, 5*time.Minute)
	if err == nil {
		t.Fatal("expected error for header missing t=, got nil")
	}
	assertWebhookError(t, err)
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
	assertWebhookError(t, err)
}

func TestVerifyWebhook_malformedTimestamp(t *testing.T) {
	secret := make([]byte, 32)
	secretHex := hex.EncodeToString(secret)
	body := []byte(`{"event":"test"}`)

	for _, badTS := range []string{"abc", "1.5", "0x10", ""} {
		sig := strings.Repeat("a", sha256HexLen)
		header := "t=" + badTS + ",v1=" + sig
		err := crucible.VerifyWebhook(secretHex, header, body, 5*time.Minute)
		if err == nil {
			t.Fatalf("expected error for malformed timestamp %q, got nil", badTS)
		}
		assertWebhookError(t, err)
	}
}

func TestVerifyWebhook_ancientTimestamp(t *testing.T) {
	secret := make([]byte, 32)
	secretHex := hex.EncodeToString(secret)
	body := []byte(`{"event":"test"}`)
	ts := "946684800" // 2000-01-01 00:00:00 UTC — far beyond any tolerance window
	sig := testSign(secret, ts, body)
	header := "t=" + ts + ",v1=" + sig

	err := crucible.VerifyWebhook(secretHex, header, body, 5*time.Minute)
	if err == nil {
		t.Fatal("expected error for ancient timestamp, got nil")
	}
	assertWebhookError(t, err)
}

func TestVerifyWebhook_maxHeaderParts_exceeded(t *testing.T) {
	secret := make([]byte, 32)
	secretHex := hex.EncodeToString(secret)
	body := []byte(`{"event":"test"}`)
	ts := nowTS()
	sig := testSign(secret, ts, body)

	// 1 t= + 1 v1= + 15 unknown filler = 17 parts → exceeds maxHeaderParts
	parts := []string{"t=" + ts, "v1=" + sig}
	for i := 0; i < 15; i++ {
		parts = append(parts, fmt.Sprintf("x%d=y", i))
	}
	header := strings.Join(parts, ",")

	err := crucible.VerifyWebhook(secretHex, header, body, 5*time.Minute)
	if err == nil {
		t.Fatal("expected error for header exceeding maxHeaderParts, got nil")
	}
	wErr := mustBeWebhookError(t, err)
	if !strings.Contains(wErr.Error(), "malformed") {
		t.Fatalf("expected 'malformed' in error, got: %v", wErr)
	}
}

func TestVerifyWebhook_maxHeaderParts_atBoundary(t *testing.T) {
	secret := make([]byte, 32)
	secretHex := hex.EncodeToString(secret)
	body := []byte(`{"event":"test"}`)
	ts := nowTS()
	sig := testSign(secret, ts, body)

	// 1 t= + 1 v1= + 14 unknown filler = 16 parts → exactly maxHeaderParts
	parts := []string{"t=" + ts, "v1=" + sig}
	for i := 0; i < 14; i++ {
		parts = append(parts, fmt.Sprintf("x%d=y", i))
	}
	header := strings.Join(parts, ",")

	if err := crucible.VerifyWebhook(secretHex, header, body, 5*time.Minute); err != nil {
		t.Fatalf("VerifyWebhook with 16-part header (at boundary): %v", err)
	}
}

func TestVerifyWebhook_emptyV1Value(t *testing.T) {
	secret := make([]byte, 32)
	secretHex := hex.EncodeToString(secret)
	body := []byte(`{"event":"test"}`)
	ts := nowTS()
	// v1= with no value appends an empty string; it should not verify (too short
	// to be a valid HMAC hex), resulting in "no matching v1 signature".
	header := "t=" + ts + ",v1="
	err := crucible.VerifyWebhook(secretHex, header, body, 5*time.Minute)
	if err == nil {
		t.Fatal("expected error for empty v1= value, got nil")
	}
	assertWebhookError(t, err)
}
