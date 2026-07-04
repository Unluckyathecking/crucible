package channelsig

import (
	"strconv"
	"testing"
	"time"
)

// goldenVectors are independently computed (Python hmac/hashlib) HMAC-SHA256 digests
// over "timestamp.body". Sign must reproduce these exactly — this is the byte-identical
// contract the pre-existing webhookout.Sign and proxy/client.go signers already satisfy.
var goldenVectors = []struct {
	name   string
	secret []byte
	ts     string
	body   []byte
	want   string
}{
	{
		name:   "webhookout-style vector",
		secret: []byte("supersecretkey12345678901234567890"),
		ts:     "1700000000",
		body:   []byte(`{"event":"test"}`),
		want:   "22df4f6859e5d6a84b02ddf444f20593608f2b2d03971d5618085ac46faf10e4",
	},
	{
		name:   "proxy-style vector",
		secret: []byte("another-secret-key"),
		ts:     "1700000001",
		body:   []byte(`{"event":"other","n":2}`),
		want:   "6875b41b3e2f64d23ba2a28973223b96d9a52033b73ae718aeb0b9032259b04b",
	},
	{
		name:   "empty body",
		secret: []byte("k"),
		ts:     "0",
		body:   []byte(""),
		want:   "6b4a4b8b3c40f1e8f53a3d36682e5f99f7ad2ac1df1c93dfe336f329167641e7",
	},
	{
		name:   "long body, hex secret bytes",
		secret: []byte("0123456789abcdef0123456789abcdef"),
		ts:     "9999999999",
		body:   []byte("the quick brown fox jumps over the lazy dog"),
		want:   "7e430e52bbf2fa2cba3bda28c84ab052502d8f294bd3e57f51c5329b657fd3b6",
	},
}

func TestSign_GoldenVectors(t *testing.T) {
	for _, v := range goldenVectors {
		t.Run(v.name, func(t *testing.T) {
			got := Sign(v.secret, v.ts, v.body)
			if got != v.want {
				t.Fatalf("Sign(%q, %q, %q) = %s, want %s", v.secret, v.ts, v.body, got, v.want)
			}
		})
	}
}

func TestSign_Deterministic(t *testing.T) {
	secret := []byte("supersecretkey12345678901234567890")
	ts := "1700000000"
	body := []byte(`{"event":"test"}`)

	sig := Sign(secret, ts, body)
	if sig == "" {
		t.Fatal("Sign returned empty string")
	}
	if len(sig) != 64 {
		t.Fatalf("expected 64-char hex digest (SHA-256), got %d", len(sig))
	}
	if Sign(secret, ts, body) != sig {
		t.Fatal("Sign is not deterministic")
	}
	if Sign(secret, ts, []byte(`{"event":"other"}`)) == sig {
		t.Fatal("different bodies produced the same signature")
	}
	if Sign(secret, "1700000001", body) == sig {
		t.Fatal("different timestamps produced the same signature")
	}
}

func TestHeader_Format(t *testing.T) {
	got := Header("1700000000", "deadbeef")
	want := "t=1700000000,v1=deadbeef"
	if got != want {
		t.Fatalf("Header() = %q, want %q", got, want)
	}
}

func TestVerify_Valid(t *testing.T) {
	secret := []byte("supersecretkey12345678901234567890")
	body := []byte(`{"event":"test"}`)
	now := time.Unix(1700000000, 0)
	ts := strconv.FormatInt(now.Unix(), 10)
	header := Header(ts, Sign(secret, ts, body))

	if err := Verify(secret, header, body, now, 5*time.Minute); err != nil {
		t.Fatalf("Verify() = %v, want nil", err)
	}
}

func TestVerify_MissingHeader(t *testing.T) {
	err := Verify([]byte("secret"), "", []byte("body"), time.Now(), 5*time.Minute)
	if err != ErrMissingHeader {
		t.Fatalf("Verify() = %v, want %v", err, ErrMissingHeader)
	}
}

func TestVerify_MalformedHeader(t *testing.T) {
	cases := []string{"garbage", "t=123", "v1=abcd", "t=,v1=", ",,,"}
	for _, header := range cases {
		t.Run(header, func(t *testing.T) {
			err := Verify([]byte("secret"), header, []byte("body"), time.Now(), 5*time.Minute)
			if err != ErrMalformedHeader {
				t.Fatalf("Verify(%q) = %v, want %v", header, err, ErrMalformedHeader)
			}
		})
	}
}

func TestVerify_InvalidTimestamp(t *testing.T) {
	err := Verify([]byte("secret"), "t=not-a-number,v1=deadbeef", []byte("body"), time.Now(), 5*time.Minute)
	if err != ErrInvalidTimestamp {
		t.Fatalf("Verify() = %v, want %v", err, ErrInvalidTimestamp)
	}
}

func TestVerify_StaleTimestamp(t *testing.T) {
	secret := []byte("secret")
	body := []byte("body")
	now := time.Unix(1700000000, 0)
	window := 5 * time.Minute

	staleTs := strconv.FormatInt(now.Add(-window-time.Second).Unix(), 10)
	header := Header(staleTs, Sign(secret, staleTs, body))
	if err := Verify(secret, header, body, now, window); err != ErrStaleTimestamp {
		t.Fatalf("stale past: Verify() = %v, want %v", err, ErrStaleTimestamp)
	}
}

func TestVerify_FutureSkew(t *testing.T) {
	secret := []byte("secret")
	body := []byte("body")
	now := time.Unix(1700000000, 0)
	window := 5 * time.Minute

	futureTs := strconv.FormatInt(now.Add(window+time.Second).Unix(), 10)
	header := Header(futureTs, Sign(secret, futureTs, body))
	if err := Verify(secret, header, body, now, window); err != ErrStaleTimestamp {
		t.Fatalf("future skew: Verify() = %v, want %v", err, ErrStaleTimestamp)
	}
}

func TestVerify_WithinWindowBoundary(t *testing.T) {
	secret := []byte("secret")
	body := []byte("body")
	now := time.Unix(1700000000, 0)
	window := 5 * time.Minute

	edgeTs := strconv.FormatInt(now.Add(-window).Unix(), 10)
	header := Header(edgeTs, Sign(secret, edgeTs, body))
	if err := Verify(secret, header, body, now, window); err != nil {
		t.Fatalf("boundary timestamp should be accepted: %v", err)
	}
}

func TestVerify_InvalidSignatureHex(t *testing.T) {
	err := Verify([]byte("secret"), "t=1700000000,v1=not-hex", []byte("body"), time.Unix(1700000000, 0), 5*time.Minute)
	if err != ErrInvalidSignature {
		t.Fatalf("Verify() = %v, want %v", err, ErrInvalidSignature)
	}
}

func TestVerify_WrongLengthSignature(t *testing.T) {
	err := Verify([]byte("secret"), "t=1700000000,v1=deadbeef", []byte("body"), time.Unix(1700000000, 0), 5*time.Minute)
	if err != ErrInvalidSignature {
		t.Fatalf("Verify() = %v, want %v", err, ErrInvalidSignature)
	}
}

func TestVerify_SignatureMismatch(t *testing.T) {
	secret := []byte("secret")
	now := time.Unix(1700000000, 0)
	ts := strconv.FormatInt(now.Unix(), 10)
	header := Header(ts, Sign(secret, ts, []byte("original body")))

	if err := Verify(secret, header, []byte("tampered body"), now, 5*time.Minute); err != ErrSignatureMismatch {
		t.Fatalf("Verify() = %v, want %v", err, ErrSignatureMismatch)
	}
}

func TestVerify_WrongSecret(t *testing.T) {
	body := []byte("body")
	now := time.Unix(1700000000, 0)
	ts := strconv.FormatInt(now.Unix(), 10)
	header := Header(ts, Sign([]byte("right-secret"), ts, body))

	if err := Verify([]byte("wrong-secret"), header, body, now, 5*time.Minute); err != ErrSignatureMismatch {
		t.Fatalf("Verify() = %v, want %v", err, ErrSignatureMismatch)
	}
}

// TestVerify_CrossLanguageParity documents that Verify accepts exactly the wire format
// produced by the Go worker channel signer (proxy/client.go doOnce) and the outbound
// webhook signer (webhookout.Sign): "t=<unix>,v1=<hex>" built from Sign's own output.
// Any SDK (Go/Rust/TS) that reproduces HMAC-SHA256(secret, ts+"."+body) in this format
// interoperates with Verify by construction; a tampered body is rejected.
func TestVerify_CrossLanguageParity(t *testing.T) {
	secret := []byte("cross-sdk-shared-secret")
	body := []byte(`{"request_id":"req_123","operation":"do_thing"}`)
	now := time.Unix(1700000000, 0)
	ts := strconv.FormatInt(now.Unix(), 10)

	// Simulates a signature produced by an independent SDK signer that implements
	// the same scheme byte-for-byte (Sign is that scheme's canonical Go implementation).
	sdkProducedSig := Sign(secret, ts, body)
	header := Header(ts, sdkProducedSig)

	if err := Verify(secret, header, body, now, 5*time.Minute); err != nil {
		t.Fatalf("Verify() rejected a valid cross-SDK signature: %v", err)
	}

	tamperedBody := []byte(`{"request_id":"req_123","operation":"do_evil"}`)
	if err := Verify(secret, header, tamperedBody, now, 5*time.Minute); err != ErrSignatureMismatch {
		t.Fatalf("Verify() on tampered body = %v, want %v", err, ErrSignatureMismatch)
	}
}
