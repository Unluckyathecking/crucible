package license

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"testing"
	"time"
)

// testKeys returns a fresh keypair; every test signs its own fixtures so we
// never depend on DefaultPublicKeyHex (whose private half is discarded).
func testKeys(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("keygen: %v", err)
	}
	return pub, priv
}

func mustSign(t *testing.T, in SignInput, priv ed25519.PrivateKey) string {
	t.Helper()
	key, err := Sign(in, priv)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	return key
}

// withClock pins now for the duration of fn.
func withClock(at time.Time, fn func()) {
	prev := now
	now = func() time.Time { return at }
	defer func() { now = prev }()
	fn()
}

func baseInput() SignInput {
	issued := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	return SignInput{
		ID:        "lic_test",
		Licensee:  "Acme",
		Email:     "ops@acme.com",
		Edition:   EditionPro,
		Seats:     5,
		IssuedAt:  issued,
		ExpiresAt: issued.AddDate(1, 0, 0),
	}
}

func TestParse(t *testing.T) {
	pub, priv := testKeys(t)
	_, otherPriv := testKeys(t)

	valid := mustSign(t, baseInput(), priv)

	tests := []struct {
		name    string
		key     string
		pub     ed25519.PublicKey
		clockAt time.Time
		wantErr bool
		grace   bool
	}{
		{
			name:    "valid",
			key:     valid,
			pub:     pub,
			clockAt: time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC),
		},
		{
			name:    "wrong signature key",
			key:     mustSign(t, baseInput(), otherPriv),
			pub:     pub,
			clockAt: time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC),
			wantErr: true,
		},
		{
			name:    "wrong prefix",
			key:     "cru0" + valid[4:],
			pub:     pub,
			clockAt: time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC),
			wantErr: true,
		},
		{
			name:    "malformed segments",
			key:     "cru1.onlytwo",
			pub:     pub,
			clockAt: time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC),
			wantErr: true,
		},
		{
			name:    "non-base64 payload",
			key:     "cru1.!!!notb64!!!.sig",
			pub:     pub,
			clockAt: time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC),
			wantErr: true,
		},
		{
			name:    "expired within grace",
			key:     valid,
			pub:     pub,
			clockAt: time.Date(2027, 1, 5, 0, 0, 0, 0, time.UTC), // 4 days past expiry
			grace:   true,
		},
		{
			name:    "expired past grace",
			key:     valid,
			pub:     pub,
			clockAt: time.Date(2027, 2, 1, 0, 0, 0, 0, time.UTC), // >14 days past
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			withClock(tc.clockAt, func() {
				lic, err := Parse(tc.key, tc.pub)
				if tc.wantErr {
					if err == nil {
						t.Fatalf("expected error, got license %+v", lic)
					}
					return
				}
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if lic.InGrace() != tc.grace {
					t.Fatalf("InGrace = %v, want %v", lic.InGrace(), tc.grace)
				}
			})
		})
	}
}

func TestParseTamperedPayload(t *testing.T) {
	pub, priv := testKeys(t)
	valid := mustSign(t, baseInput(), priv)

	// Decode the signed payload, mutate a field, re-encode WITHOUT re-signing —
	// the original signature must no longer verify.
	parts := splitKey(t, valid)
	raw, err := b64.DecodeString(parts[1])
	if err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	var p payload
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	p.Seats = 100000
	mutated, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	tampered := keyPrefix + "." + b64.EncodeToString(mutated) + "." + parts[2]

	withClock(time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC), func() {
		if _, err := Parse(tampered, pub); err == nil {
			t.Fatal("expected tampered payload to fail signature verification")
		}
	})
}

func splitKey(t *testing.T, key string) [3]string {
	t.Helper()
	var out [3]string
	i, start := 0, 0
	for pos := 0; pos < len(key) && i < 3; pos++ {
		if key[pos] == '.' {
			out[i] = key[start:pos]
			i++
			start = pos + 1
		}
	}
	out[2] = key[start:]
	return out
}

func TestEditionDefaultFeatures(t *testing.T) {
	pub, priv := testKeys(t)
	clock := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)

	tests := []struct {
		edition  string
		declared []string
		want     []string
	}{
		{EditionPro, nil, []string{FeatureOperatorTokens, FeatureAuditExport}},
		{EditionBusiness, nil, []string{FeatureSSO, FeatureOperatorTokens, FeatureAuditExport}},
		{EditionEnterprise, nil, []string{FeatureSSO, FeatureOperatorTokens, FeatureAuditExport}},
		{EditionPro, []string{FeatureSSO}, []string{FeatureSSO}},
	}
	for _, tc := range tests {
		t.Run(tc.edition, func(t *testing.T) {
			in := baseInput()
			in.Edition = tc.edition
			in.Features = tc.declared
			key := mustSign(t, in, priv)
			withClock(clock, func() {
				lic, err := Parse(key, pub)
				if err != nil {
					t.Fatalf("parse: %v", err)
				}
				if len(lic.Features) != len(tc.want) {
					t.Fatalf("features = %v, want %v", lic.Features, tc.want)
				}
				for i := range tc.want {
					if lic.Features[i] != tc.want[i] {
						t.Fatalf("features = %v, want %v", lic.Features, tc.want)
					}
				}
			})
		})
	}
}

func TestUnknownEditionRejected(t *testing.T) {
	_, priv := testKeys(t)
	if _, err := Sign(SignInput{Edition: "platinum", IssuedAt: time.Now(), ExpiresAt: time.Now()}, priv); err == nil {
		t.Fatal("expected Sign to reject unknown edition")
	}
}

func TestNilLicenseSafety(t *testing.T) {
	var l *License
	if l.Has(FeatureSSO) {
		t.Fatal("nil license must not have any feature")
	}
	if l.InGrace() {
		t.Fatal("nil license must not be in grace")
	}
}

func TestHas(t *testing.T) {
	l := &License{Features: []string{FeatureOperatorTokens, FeatureAuditExport}}
	if !l.Has(FeatureOperatorTokens) {
		t.Fatal("expected operator_tokens")
	}
	if l.Has(FeatureSSO) {
		t.Fatal("did not expect sso")
	}
}

func TestResolvePublicKey(t *testing.T) {
	// DefaultPublicKeyHex must decode to a valid 32-byte key.
	if _, err := ResolvePublicKey(""); err != nil {
		t.Fatalf("default public key invalid: %v", err)
	}
	if _, err := ResolvePublicKey("nothex"); err == nil {
		t.Fatal("expected error on non-hex override")
	}
	if _, err := ResolvePublicKey("abcd"); err == nil {
		t.Fatal("expected error on short key")
	}
	pub, _ := testKeys(t)
	if _, err := ResolvePublicKey(encodeHex(pub)); err != nil {
		t.Fatalf("valid override rejected: %v", err)
	}
}

func encodeHex(b []byte) string {
	const hexdigits = "0123456789abcdef"
	out := make([]byte, len(b)*2)
	for i, c := range b {
		out[i*2] = hexdigits[c>>4]
		out[i*2+1] = hexdigits[c&0x0f]
	}
	return string(out)
}
