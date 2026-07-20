package channelsig

import (
	"testing"
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
