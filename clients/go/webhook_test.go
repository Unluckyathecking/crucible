package crucible_test

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	crucible "github.com/Unluckyathecking/crucible/clients/go"
)

// sha256HexLen is the expected hex-encoded length of a SHA-256 digest (32 bytes × 2).
// Using sha256.Size*2 instead of the magic number 64 keeps tests resilient to algorithm changes.
const sha256HexLen = sha256.Size * 2

// testSign replicates gateway/internal/webhookout.Sign — MUST be kept in sync.
// Any change to the gateway signing algorithm requires updating this helper and
// the known-good reference vector in TestVerifyWebhook_knownGoodVector.
//
// Design note: testSign is intentionally algorithm-equivalent to the production
// VerifyWebhook path (same Write order, same separator). This is not a tautological
// test helper — algorithmic correctness is independently verified by
// TestVerifyWebhook_knownGoodVector, which compares against a pre-computed hardcoded
// HMAC produced offline. If VerifyWebhook's algorithm ever drifts from the gateway
// signer, the known-good vector test catches it regardless of testSign.
//
// Nil body: testSign mirrors VerifyWebhook's explicit nil→[]byte{} normalisation
// so both paths remain visibly aligned. TestVerifyWebhook_nilBody verifies the
// round-trip: a nil-body signature produced by testSign verifies successfully.
func testSign(secret []byte, timestamp string, body []byte) string {
	// Explicit nil guard keeps testSign aligned with VerifyWebhook — both normalise
	// nil to []byte{} before HMAC so test signatures match the verifier input exactly.
	if body == nil {
		body = []byte{}
	}
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write([]byte(timestamp))
	_, _ = mac.Write([]byte("."))
	_, _ = mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

// nowTS returns a Unix timestamp 2 minutes in the past. The 2-minute margin
// absorbs goroutine descheduling and CI load spikes without approaching the
// 5-minute tolerance boundary used by the tests.
func nowTS() string { return strconv.FormatInt(time.Now().Add(-2*time.Minute).Unix(), 10) }

// assertWebhookError asserts err is a non-nil *crucible.WebhookError. Use when
// the error message does not need further inspection.
func assertWebhookError(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	mustBeWebhookError(t, err)
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

func TestVerifyWebhook_knownGoodVector(t *testing.T) {
	// Pre-computed reference vector — independent of testSign. Guards against
	// algorithmic drift between this SDK and the gateway signer.
	//
	// Inputs:
	//   secret:    0x00 × 32 bytes
	//   timestamp: "1700000000"   (2023-11-14T22:13:20 UTC)
	//   body:      {"event":"test"}
	// Expected HMAC-SHA256("1700000000.{\"event\":\"test\"}", key=secret):
	//   247d0f12bc3bef311cdb44ced37a1192ba82e78ffe8edd22fbf2205a414e94f5
	//
	// The digest is lowercase hex. Go's encoding/hex package always produces
	// lowercase output (a–f), so this constant has no platform dependency.
	// The verifier decodes the v1= hex bytes and compares at the byte level,
	// so case in the incoming header is irrelevant for comparison purposes.
	secretHex := strings.Repeat("00", 32)
	body := []byte(`{"event":"test"}`)
	header := "t=1700000000,v1=247d0f12bc3bef311cdb44ced37a1192ba82e78ffe8edd22fbf2205a414e94f5"

	// Compute the actual age of the 2023 reference vector plus a 1-hour buffer.
	// If the system clock predates the reference (e.g. a VM with wrong time), the
	// verifier rejects the timestamp as "in the future" regardless of tolerance —
	// skip rather than clamping, which would produce a misleading "future" failure.
	vectorAge := time.Since(time.Unix(1700000000, 0))
	if vectorAge < 0 {
		t.Skip("system clock predates reference vector; skipping time-dependent test")
	}
	vectorTolerance := vectorAge + time.Hour
	if err := crucible.VerifyWebhook(secretHex, header, body, vectorTolerance); err != nil {
		t.Fatalf("known-good reference vector rejected: %v", err)
	}

	// The same vector must be rejected with DefaultTolerance: the 2023 timestamp is
	// far in the past, so replay protection must fire. Catches regressions that
	// accidentally disable timestamp validation.
	err := crucible.VerifyWebhook(secretHex, header, body, crucible.DefaultTolerance)
	if err == nil {
		t.Fatal("expected 2023 timestamp to be rejected with DefaultTolerance, but got nil")
	}
	wErr := mustBeWebhookError(t, err)
	if !strings.Contains(wErr.Message(), "too old") {
		t.Fatalf("expected 'too old' error for stale 2023 vector, got: %v", wErr)
	}
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

func TestVerifyWebhook_defaultTolerance_futureTimestamp(t *testing.T) {
	secret := make([]byte, 32)
	secretHex := hex.EncodeToString(secret)
	body := []byte(`{"event":"test"}`)
	future := time.Now().Add(10 * time.Minute)
	ts := strconv.FormatInt(future.Unix(), 10)
	sig := testSign(secret, ts, body)
	header := "t=" + ts + ",v1=" + sig

	// tolerance=0 maps to DefaultTolerance (5 min), but future-timestamp rejection
	// happens before the age check — a future ts must be rejected regardless of the
	// sentinel expansion. Verifies the sentinel path does not mask future rejection.
	err := crucible.VerifyWebhook(secretHex, header, body, 0)
	if err == nil {
		t.Fatal("expected error for future timestamp with tolerance=0, got nil")
	}
	wErr := mustBeWebhookError(t, err)
	if !strings.Contains(wErr.Message(), "future") {
		t.Fatalf("expected 'future' error with tolerance=0, got: %v", wErr)
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
	wErr := mustBeWebhookError(t, err)
	if !strings.Contains(wErr.Message(), "no matching v1 signature") {
		t.Fatalf("expected 'no matching v1 signature', got: %v", wErr)
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
	ts := nowTS()
	sig := testSign(wrongSecret, ts, body)
	header := "t=" + ts + ",v1=" + sig

	err := crucible.VerifyWebhook(secretHex, header, body, 5*time.Minute)
	if err == nil {
		t.Fatal("expected error for wrong secret, got nil")
	}
	wErr := mustBeWebhookError(t, err)
	if !strings.Contains(wErr.Message(), "no matching v1 signature") {
		t.Fatalf("expected 'no matching v1 signature', got: %v", wErr)
	}
}

func TestVerifyWebhook_futureTimestamp(t *testing.T) {
	secret := make([]byte, 32)
	secretHex := hex.EncodeToString(secret)
	body := []byte(`{"event":"test"}`)
	future := time.Now().Add(10 * time.Minute)
	ts := strconv.FormatInt(future.Unix(), 10)
	sig := testSign(secret, ts, body)
	header := "t=" + ts + ",v1=" + sig

	err := crucible.VerifyWebhook(secretHex, header, body, 5*time.Minute)
	if err == nil {
		t.Fatal("expected error for future timestamp, got nil")
	}
	wErr := mustBeWebhookError(t, err)
	if !strings.Contains(wErr.Message(), "future") {
		t.Fatalf("expected error to mention 'future', got: %v", wErr)
	}
}

func TestVerifyWebhook_expiredTimestamp(t *testing.T) {
	secret := make([]byte, 32)
	secretHex := hex.EncodeToString(secret)
	body := []byte(`{"event":"test"}`)
	old := time.Now().Add(-10 * time.Minute)
	ts := strconv.FormatInt(old.Unix(), 10)
	sig := testSign(secret, ts, body)
	header := "t=" + ts + ",v1=" + sig

	err := crucible.VerifyWebhook(secretHex, header, body, 5*time.Minute)
	if err == nil {
		t.Fatal("expected error for expired timestamp, got nil")
	}
	wErr := mustBeWebhookError(t, err)
	if !strings.Contains(wErr.Message(), "too old") {
		t.Fatalf("expected 'too old' in error, got: %v", wErr)
	}
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
	if !strings.Contains(wErr.Message(), "no matching v1 signature") {
		t.Fatalf("expected 'no matching v1 signature', got: %v", wErr)
	}
}

func TestVerifyWebhook_missingHeader(t *testing.T) {
	err := crucible.VerifyWebhook("aabb", "", []byte("body"), 5*time.Minute)
	if err == nil {
		t.Fatal("expected error for missing header, got nil")
	}
	wErr := mustBeWebhookError(t, err)
	if !strings.Contains(wErr.Message(), "missing") {
		t.Fatalf("expected 'missing' in error for empty header, got: %v", wErr)
	}
}

func TestVerifyWebhook_invalidSecretHex(t *testing.T) {
	body := []byte(`{"event":"test"}`)
	ts := nowTS()

	cases := []struct {
		secret  string
		wantMsg string
	}{
		{"", "must be non-empty"},  // empty string
		{"zz", "non-hex"},          // even length but non-hex
		{"abc", "even-length"},     // odd length
	}
	for _, tc := range cases {
		err := crucible.VerifyWebhook(tc.secret, "t="+ts+",v1="+strings.Repeat("a", sha256HexLen), body, 5*time.Minute)
		if err == nil {
			t.Fatalf("expected error for invalid secretHex %q, got nil", tc.secret)
		}
		wErr := mustBeWebhookError(t, err)
		if !strings.Contains(wErr.Message(), tc.wantMsg) {
			t.Fatalf("invalid secretHex %q: expected %q in error, got: %v", tc.secret, tc.wantMsg, wErr)
		}
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
	wErr := mustBeWebhookError(t, err)
	if !strings.Contains(wErr.Message(), "negative tolerance") {
		t.Fatalf("expected 'negative tolerance' in error, got: %v", wErr)
	}
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
	wErr := mustBeWebhookError(t, err)
	if !strings.Contains(wErr.Message(), "malformed") {
		t.Fatalf("expected 'malformed' for header missing t=, got: %v", wErr)
	}
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
	wErr := mustBeWebhookError(t, err)
	if !strings.Contains(wErr.Message(), "malformed") {
		t.Fatalf("expected 'malformed' for header missing v1=, got: %v", wErr)
	}
}

func TestVerifyWebhook_malformedTimestamp(t *testing.T) {
	secret := make([]byte, 32)
	secretHex := hex.EncodeToString(secret)
	body := []byte(`{"event":"test"}`)

	// empty timestamp causes "malformed header" (empty t= value, caught by final check);
	// non-empty non-decimal values fail ParseInt with "bad timestamp";
	// leading zeros are rejected for cross-language consistency with TypeScript.
	tsCases := []struct {
		badTS   string
		wantMsg string
	}{
		{"abc", "bad timestamp"},
		{"1.5", "bad timestamp"},
		{"0x10", "bad timestamp"},
		{"", "malformed"},
		{"0123456789", "bad timestamp"}, // leading zero — rejected for cross-language consistency
		{"+1234567890", "bad timestamp"}, // + prefix — ParseInt accepts it, but first-char digit guard rejects for cross-language parity
	}
	for _, tc := range tsCases {
		sig := strings.Repeat("a", sha256HexLen)
		header := "t=" + tc.badTS + ",v1=" + sig
		err := crucible.VerifyWebhook(secretHex, header, body, 5*time.Minute)
		if err == nil {
			t.Fatalf("expected error for malformed timestamp %q, got nil", tc.badTS)
		}
		wErr := mustBeWebhookError(t, err)
		if !strings.Contains(wErr.Message(), tc.wantMsg) {
			t.Fatalf("malformed timestamp %q: expected %q in error, got: %v", tc.badTS, tc.wantMsg, wErr)
		}
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
	wErr := mustBeWebhookError(t, err)
	if !strings.Contains(wErr.Message(), "too old") {
		t.Fatalf("expected 'too old' in error, got: %v", wErr)
	}
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
	if !strings.Contains(wErr.Message(), "malformed") {
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

func TestVerifyWebhook_v1TooLong(t *testing.T) {
	secret := make([]byte, 32)
	secretHex := hex.EncodeToString(secret)
	body := []byte(`{"event":"test"}`)
	ts := nowTS()
	validSig := testSign(secret, ts, body)
	// 66 chars: valid 64-char sig + "00" — must be rejected by the len guard in VerifyWebhook.
	tooLongSig := validSig + "00"
	header := "t=" + ts + ",v1=" + tooLongSig

	err := crucible.VerifyWebhook(secretHex, header, body, 5*time.Minute)
	if err == nil {
		t.Fatal("expected error for too-long v1 sig, got nil")
	}
	wErr := mustBeWebhookError(t, err)
	if !strings.Contains(wErr.Message(), "no matching v1 signature") {
		t.Fatalf("expected 'no matching v1 signature', got: %v", wErr)
	}
}

func TestVerifyWebhook_duplicateTimestamp(t *testing.T) {
	secret := make([]byte, 32)
	secretHex := hex.EncodeToString(secret)
	body := []byte(`{"event":"test"}`)
	ts := nowTS()
	sig := testSign(secret, ts, body)

	cases := []struct {
		name   string
		header string
	}{
		// Real timestamp first, attacker timestamp second — first-wins rejects the dup.
		{"real_first", "t=" + ts + ",t=999,v1=" + sig},
		// Attacker timestamp first, real timestamp second — first-wins uses the attacker
		// timestamp (999), then the real t= is rejected as a duplicate. HMAC verification
		// then fails because the signed message used the real timestamp, not "999".
		// Both orderings must be rejected to prevent any form of timestamp substitution.
		{"attacker_first", "t=999,t=" + ts + ",v1=" + sig},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := crucible.VerifyWebhook(secretHex, tc.header, body, 5*time.Minute)
			if err == nil {
				t.Fatalf("expected error for duplicate t= key (%s), got nil", tc.name)
			}
			wErr := mustBeWebhookError(t, err)
			if !strings.Contains(wErr.Message(), "malformed") {
				t.Fatalf("expected 'malformed' in error for duplicate t= (%s), got: %v", tc.name, wErr)
			}
		})
	}
}

func TestVerifyWebhook_timestampBoundaries(t *testing.T) {
	secret := make([]byte, 32)
	secretHex := hex.EncodeToString(secret)
	body := []byte(`{"event":"test"}`)

	cases := []struct {
		ts      string
		wantMsg string
	}{
		// 16 digits: exceeds the 15-char length guard → "bad timestamp"
		{"1000000000000000", "bad timestamp"},
		// Negative/plus prefix: first-char digit guard fires before ParseInt,
		// matching TypeScript's /^\d{1,15}$/ rejection. Both SDKs emit "bad timestamp".
		{"-1", "bad timestamp"},
		{"+1234567890", "bad timestamp"},
	}
	for _, tc := range cases {
		sig := strings.Repeat("a", sha256HexLen)
		header := "t=" + tc.ts + ",v1=" + sig
		err := crucible.VerifyWebhook(secretHex, header, body, 5*time.Minute)
		if err == nil {
			t.Fatalf("expected error for timestamp %q, got nil", tc.ts)
		}
		wErr := mustBeWebhookError(t, err)
		if !strings.Contains(wErr.Message(), tc.wantMsg) {
			t.Fatalf("timestamp %q: expected %q in error, got: %v", tc.ts, tc.wantMsg, wErr)
		}
	}
}

func TestVerifyWebhook_emptyV1Value(t *testing.T) {
	secret := make([]byte, 32)
	secretHex := hex.EncodeToString(secret)
	body := []byte(`{"event":"test"}`)
	ts := nowTS()
	// v1= with no value is rejected at parse time as malformed — the parser
	// never produces an empty-string candidate for the verifier to discard.
	header := "t=" + ts + ",v1="
	err := crucible.VerifyWebhook(secretHex, header, body, 5*time.Minute)
	if err == nil {
		t.Fatal("expected error for empty v1= value, got nil")
	}
	wErr := mustBeWebhookError(t, err)
	if !strings.Contains(wErr.Message(), "malformed") {
		t.Fatalf("expected 'malformed' for empty v1= value, got: %v", wErr)
	}
}

func TestVerifyWebhook_emptyKey(t *testing.T) {
	secret := make([]byte, 32)
	secretHex := hex.EncodeToString(secret)
	body := []byte(`{"event":"test"}`)
	ts := nowTS()
	sig := testSign(secret, ts, body)
	// "=value" has empty key — matches TypeScript's idx<=0 rejection.
	header := "t=" + ts + ",v1=" + sig + ",=extra"
	err := crucible.VerifyWebhook(secretHex, header, body, 5*time.Minute)
	if err == nil {
		t.Fatal("expected error for empty-key part, got nil")
	}
	wErr := mustBeWebhookError(t, err)
	if !strings.Contains(wErr.Message(), "malformed") {
		t.Fatalf("expected 'malformed' for empty-key part, got: %v", wErr)
	}
}

func TestVerifyWebhook_unknownKeyEmptyValue(t *testing.T) {
	secret := make([]byte, 32)
	secretHex := hex.EncodeToString(secret)
	body := []byte(`{"event":"test"}`)
	ts := nowTS()
	sig := testSign(secret, ts, body)
	// Unknown key with empty value (foo=) must be rejected as malformed,
	// consistent with t= and v1= which already reject empty values explicitly.
	header := "t=" + ts + ",v1=" + sig + ",foo="
	err := crucible.VerifyWebhook(secretHex, header, body, 5*time.Minute)
	if err == nil {
		t.Fatal("expected error for unknown key with empty value, got nil")
	}
	wErr := mustBeWebhookError(t, err)
	if !strings.Contains(wErr.Message(), "malformed") {
		t.Fatalf("expected 'malformed' for unknown key with empty value, got: %v", wErr)
	}
}

func TestVerifyWebhook_unknownKeyForwardCompat(t *testing.T) {
	secret := make([]byte, 32)
	secretHex := hex.EncodeToString(secret)
	body := []byte(`{"event":"test"}`)
	ts := nowTS()
	sig := testSign(secret, ts, body)
	// Unknown keys with non-empty values must be silently ignored (forward compatibility
	// with future gateway fields like v2=). Verification should still succeed.
	header := "t=" + ts + ",v1=" + sig + ",foo=bar"
	if err := crucible.VerifyWebhook(secretHex, header, body, 5*time.Minute); err != nil {
		t.Fatalf("VerifyWebhook with unknown key foo=bar: %v", err)
	}
}

func TestWebhookError_Message(t *testing.T) {
	// WebhookError.Error() must have the "crucible webhook:" prefix and embed the raw
	// message. WebhookError.Message() must return the raw message without prefix.
	// Verify for multiple error paths since msg is unexported (can't construct directly).
	cases := []struct {
		name    string
		fn      func() error
		wantMsg string
	}{
		{
			name:    "invalid secretHex",
			fn:      func() error { return crucible.VerifyWebhook("", "t=1,v1="+strings.Repeat("a", sha256HexLen), []byte{}, 5*time.Minute) },
			wantMsg: "secretHex",
		},
		{
			name:    "negative tolerance",
			fn:      func() error { return crucible.VerifyWebhook(strings.Repeat("aa", 32), "t=1,v1="+strings.Repeat("a", sha256HexLen), []byte{}, -time.Second) },
			wantMsg: "negative tolerance",
		},
		{
			name:    "malformed header",
			fn:      func() error { return crucible.VerifyWebhook(strings.Repeat("aa", 32), "notvalid", []byte{}, 5*time.Minute) },
			wantMsg: "malformed",
		},
		{
			name:    "timestamp too old",
			fn:      func() error { return crucible.VerifyWebhook(strings.Repeat("aa", 32), "t=1,v1="+strings.Repeat("a", sha256HexLen), []byte{}, 5*time.Minute) },
			wantMsg: "too old",
		},
		{
			name:    "future timestamp",
			fn:      func() error { return crucible.VerifyWebhook(strings.Repeat("aa", 32), "t=99999999999,v1="+strings.Repeat("a", sha256HexLen), []byte{}, 5*time.Minute) },
			wantMsg: "future",
		},
		{
			name: "no matching v1 signature",
			fn: func() error {
				secret := make([]byte, 32)
				return crucible.VerifyWebhook(hex.EncodeToString(secret), "t="+nowTS()+",v1="+strings.Repeat("a", sha256HexLen), []byte("body"), 5*time.Minute)
			},
			wantMsg: "no matching v1 signature",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.fn()
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			wErr := mustBeWebhookError(t, err)
			if !strings.Contains(wErr.Message(), tc.wantMsg) {
				t.Errorf("Message() should contain %q, got %q", tc.wantMsg, wErr.Message())
			}
			want := "crucible webhook: " + wErr.Message()
			if wErr.Error() != want {
				t.Errorf("Error() = %q, want %q", wErr.Error(), want)
			}
		})
	}
}

func TestVerifyWebhook_v1NonHexChars(t *testing.T) {
	secret := make([]byte, 32)
	secretHex := hex.EncodeToString(secret)
	body := []byte(`{"event":"test"}`)
	ts := nowTS()
	// 64-char string where first char is non-hex ('g'): correct length but
	// hex.DecodeString fails, so no candidate matches.
	nonHexSig := "g" + strings.Repeat("0", sha256HexLen-1)
	header := "t=" + ts + ",v1=" + nonHexSig

	err := crucible.VerifyWebhook(secretHex, header, body, 5*time.Minute)
	if err == nil {
		t.Fatal("expected error for non-hex v1 candidate, got nil")
	}
	wErr := mustBeWebhookError(t, err)
	if !strings.Contains(wErr.Message(), "no matching v1 signature") {
		t.Fatalf("expected 'no matching v1 signature', got: %v", wErr)
	}
}

func TestVerifyWebhook_maxSigCandidates_atBoundary(t *testing.T) {
	secret := make([]byte, 32)
	secretHex := hex.EncodeToString(secret)
	body := []byte(`{"event":"test"}`)
	ts := nowTS()
	validSig := testSign(secret, ts, body)

	// 7 non-matching candidates + 1 valid candidate = exactly maxSigCandidates (8).
	// The 8th candidate is at index 7 (len(sigs) < 8 is true), so it is accepted.
	parts := []string{"t=" + ts}
	for i := 0; i < 7; i++ {
		parts = append(parts, "v1="+strings.Repeat("b", sha256HexLen))
	}
	parts = append(parts, "v1="+validSig)
	header := strings.Join(parts, ",")

	if err := crucible.VerifyWebhook(secretHex, header, body, 5*time.Minute); err != nil {
		t.Fatalf("VerifyWebhook with 8 candidates at boundary: %v", err)
	}
}

func TestVerifyWebhook_maxValidTimestamp(t *testing.T) {
	secret := make([]byte, 32)
	secretHex := hex.EncodeToString(secret)
	body := []byte(`{"event":"test"}`)
	// 15-digit all-nines timestamp: maximum string length accepted by the length guard.
	// The corresponding Unix time is far in the future, so the age check rejects it.
	ts := "999999999999999"
	sig := testSign(secret, ts, body)
	header := "t=" + ts + ",v1=" + sig

	err := crucible.VerifyWebhook(secretHex, header, body, 5*time.Minute)
	if err == nil {
		t.Fatal("expected error for far-future max-length timestamp, got nil")
	}
	wErr := mustBeWebhookError(t, err)
	if !strings.Contains(wErr.Message(), "future") {
		t.Fatalf("expected 'future' in error for max-length timestamp, got: %v", wErr)
	}
}

func TestVerifyWebhook_timestampZero(t *testing.T) {
	secret := make([]byte, 32)
	secretHex := hex.EncodeToString(secret)
	body := []byte(`{"event":"test"}`)
	// Unix timestamp 0 = 1970-01-01 00:00:00 UTC — valid decimal, no leading zero
	// issue, but far outside any replay window → rejected as "too old".
	ts := "0"
	sig := testSign(secret, ts, body)
	header := "t=" + ts + ",v1=" + sig

	err := crucible.VerifyWebhook(secretHex, header, body, 5*time.Minute)
	if err == nil {
		t.Fatal("expected error for epoch timestamp, got nil")
	}
	wErr := mustBeWebhookError(t, err)
	if !strings.Contains(wErr.Message(), "too old") {
		t.Fatalf("expected 'too old' for epoch timestamp, got: %v", wErr)
	}
}

func TestVerifyWebhook_embeddedEqualInValue(t *testing.T) {
	secret := make([]byte, 32)
	secretHex := hex.EncodeToString(secret)
	body := []byte(`{"event":"test"}`)
	// "t=1=2" has an embedded '=' in the timestamp value. The parser uses SplitN(part, "=", 2)
	// and then checks strings.Contains(kv[1], "=") — this guard fires and rejects the header.
	// Mirrors the TypeScript test for cross-language parity.
	err := crucible.VerifyWebhook(secretHex, "t=1=2,v1="+strings.Repeat("a", sha256HexLen), body, 5*time.Minute)
	if err == nil {
		t.Fatal("expected error for embedded '=' in timestamp value, got nil")
	}
	wErr := mustBeWebhookError(t, err)
	if !strings.Contains(wErr.Message(), "malformed") {
		t.Fatalf("expected 'malformed' for embedded '=', got: %v", wErr)
	}
}

func TestVerifyWebhook_nilBody(t *testing.T) {
	// In Go, nil and []byte{} are distinct but both valid as hash.Write inputs.
	// mac.Write(nil) is a no-op (zero bytes written), same as mac.Write([]byte{}).
	// Verify that a nil body signed correctly round-trips through VerifyWebhook.
	secret := make([]byte, 32)
	for i := range secret {
		secret[i] = 0x33
	}
	secretHex := hex.EncodeToString(secret)
	ts := nowTS()
	sig := testSign(secret, ts, nil) // nil body → HMAC over timestamp + "."
	header := "t=" + ts + ",v1=" + sig

	if err := crucible.VerifyWebhook(secretHex, header, nil, 5*time.Minute); err != nil {
		t.Fatalf("VerifyWebhook with nil body: %v", err)
	}
}

func TestVerifyWebhook_v1TooShort(t *testing.T) {
	secret := make([]byte, 32)
	secretHex := hex.EncodeToString(secret)
	body := []byte(`{"event":"test"}`)
	ts := nowTS()
	// 32 hex chars (16 bytes) — half the expected SHA-256 length; rejected by the
	// len(sigHex) != sha256.Size*2 guard, so no candidate matches.
	shortSig := strings.Repeat("a", sha256HexLen/2)
	header := "t=" + ts + ",v1=" + shortSig

	err := crucible.VerifyWebhook(secretHex, header, body, 5*time.Minute)
	if err == nil {
		t.Fatal("expected error for short v1 candidate, got nil")
	}
	wErr := mustBeWebhookError(t, err)
	if !strings.Contains(wErr.Message(), "no matching v1 signature") {
		t.Fatalf("expected 'no matching v1 signature' for short v1, got: %v", wErr)
	}
}
