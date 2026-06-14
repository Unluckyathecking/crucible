// webhook_internal_test.go is an internal test (package crucible, not crucible_test)
// so it can call the unexported parseSignatureHeader directly to verify that the
// maxSigCandidates cap is enforced at parse time, not just at HMAC-comparison time.
package crucible

import (
	"crypto/sha256"
	"fmt"
	"strings"
	"testing"
)

// TestParseSignatureHeader_enforcesMaxSigCandidates verifies that
// parseSignatureHeader itself caps the number of v1= values it returns at
// maxSigCandidates (8), silently dropping any extras. This ensures the
// DoS defense lives in the parser, not only in the HMAC comparison loop.
//
// Three sub-cases are tested:
//  1. Header at the maxHeaderParts boundary (1 t= + 15 v1= = 16 total parts).
//  2. Header well below maxHeaderParts (1 t= + 10 v1= = 11 total parts),
//     verifying the cap works independently of the max-parts limit.
//  3. Header where a duplicate t= appears after the cap — verifies the loop
//     continues validating remaining parts even once the v1 cap is full.
func TestParseSignatureHeader_enforcesMaxSigCandidates(t *testing.T) {
	const sigHex = sha256.Size * 2 // 64

	t.Run("at_maxHeaderParts_boundary", func(t *testing.T) {
		// 1 t= + 15 v1= = 16 total parts — exactly at maxHeaderParts.
		parts := []string{"t=1234567890"}
		for i := 0; i < 15; i++ {
			parts = append(parts, fmt.Sprintf("v1=%s", strings.Repeat("a", sigHex)))
		}
		header := strings.Join(parts, ",")

		_, sigs, err := parseSignatureHeader(header)
		if err != nil {
			t.Fatalf("unexpected parse error: %v", err)
		}
		if len(sigs) != maxSigCandidates {
			t.Fatalf("got %d sig candidates, want %d (maxSigCandidates); "+
				"excess were not dropped at parse time", len(sigs), maxSigCandidates)
		}
	})

	t.Run("below_maxHeaderParts", func(t *testing.T) {
		// 1 t= + 10 v1= = 11 total parts — well below maxHeaderParts (16) but
		// still exceeds maxSigCandidates (8). Verifies the cap is enforced
		// independently of the max-parts limit.
		parts := []string{"t=1234567890"}
		for i := 0; i < 10; i++ {
			parts = append(parts, fmt.Sprintf("v1=%s", strings.Repeat("b", sigHex)))
		}
		header := strings.Join(parts, ",")

		_, sigs, err := parseSignatureHeader(header)
		if err != nil {
			t.Fatalf("unexpected parse error: %v", err)
		}
		if len(sigs) != maxSigCandidates {
			t.Fatalf("got %d sig candidates, want %d (maxSigCandidates); "+
				"excess were not dropped at parse time", len(sigs), maxSigCandidates)
		}
	})

	t.Run("continues_validating_after_cap", func(t *testing.T) {
		// 1 t= + maxSigCandidates v1= + 1 duplicate t= — total parts still ≤ maxHeaderParts.
		// After the v1 cap fills, the loop must keep running and catch the duplicate t=.
		parts := []string{"t=1234567890"}
		for i := 0; i < maxSigCandidates; i++ {
			parts = append(parts, fmt.Sprintf("v1=%s", strings.Repeat("a", sigHex)))
		}
		parts = append(parts, "t=9999999999") // duplicate t= after cap
		header := strings.Join(parts, ",")

		_, _, err := parseSignatureHeader(header)
		if err == nil {
			t.Fatal("expected malformed error for duplicate t= after cap, got nil")
		}
		if !strings.Contains(err.Message(), "malformed") {
			t.Fatalf("expected 'malformed' error message, got: %q", err.Message())
		}
	})
}
