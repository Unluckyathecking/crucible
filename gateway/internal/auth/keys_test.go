package auth

import (
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGenerate(t *testing.T) {
	full1, prefix1, err := Generate("cru_")
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if !strings.HasPrefix(full1, "cru_live_") {
		t.Errorf("full key %q missing expected prefix", full1)
	}
	if len(prefix1) != PrefixLen {
		t.Errorf("prefix length = %d, want %d", len(prefix1), PrefixLen)
	}
	if !strings.HasPrefix(full1, prefix1) {
		t.Errorf("prefix %q is not a prefix of full %q", prefix1, full1)
	}

	full2, _, _ := Generate("cru_")
	if full1 == full2 {
		t.Error("two calls produced the same key — entropy broken")
	}
}

func TestHashVerify(t *testing.T) {
	salt := "thirty-two-bytes-of-salt-padding-aaaa"
	key := "cru_live_AAAA"

	h1 := Hash(salt, key)
	h2 := Hash(salt, key)
	if !VerifyHash(h1, h2) {
		t.Error("Hash is not deterministic")
	}

	hOther := Hash(salt, "different_key")
	if VerifyHash(h1, hOther) {
		t.Error("VerifyHash matched different inputs")
	}

	hSalted := Hash("other-salt-padding-bytes-aaaaaaaaaaa", key)
	if VerifyHash(h1, hSalted) {
		t.Error("VerifyHash matched across different salts — salt not affecting output")
	}
}

func TestPrefixLen(t *testing.T) {
	cases := []string{"cru_", "vat_", "phn_", "x_"}
	for _, p := range cases {
		t.Run(p, func(t *testing.T) {
			full, prefix, err := Generate(p)
			if err != nil {
				t.Fatalf("Generate: %v", err)
			}
			if len(prefix) != PrefixLen {
				t.Errorf("prefix=%q len=%d want=%d", prefix, len(prefix), PrefixLen)
			}
			if !strings.HasPrefix(full, p+"live_") {
				t.Errorf("full=%q missing prefix %q", full, p+"live_")
			}
		})
	}
}

type HashTestCase struct {
	Salt         string `json:"salt"`
	Key          string `json:"key"`
	ExpectedHash string `json:"expectedHash"`
}

func TestHash_CrossValidation(t *testing.T) {
	// Read the shared testdata file from the dashboard directory
	// This ensures the Go and TS implementations stay byte-identical.
	path := filepath.Join("..", "..", "..", "dashboard", "testdata", "keys.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("failed to read cross-language testdata: %v", err)
	}

	var testCases []HashTestCase
	if err := json.Unmarshal(data, &testCases); err != nil {
		t.Fatalf("failed to parse testdata: %v", err)
	}

	for i, tc := range testCases {
		hashBytes := Hash(tc.Salt, tc.Key)
		hashHex := hex.EncodeToString(hashBytes)

		if hashHex != tc.ExpectedHash {
			t.Errorf("test case %d failed:\nsalt: %q\nkey: %q\nexpected: %s\ngot:      %s",
				i, tc.Salt, tc.Key, tc.ExpectedHash, hashHex)
		}
	}
}
