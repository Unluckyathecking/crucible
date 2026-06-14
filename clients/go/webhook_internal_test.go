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
// The header is constructed with 1 t= + 15 v1= = 16 total parts, which is
// exactly at the maxHeaderParts boundary so the header is accepted as valid.
// Of the 15 v1= values, only the first 8 (maxSigCandidates) should appear in
// the returned sigs slice.
func TestParseSignatureHeader_enforcesMaxSigCandidates(t *testing.T) {
	const sigHex = sha256.Size * 2 // 64
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
		t.Fatalf("parseSignatureHeader: got %d sig candidates, want exactly %d (maxSigCandidates); "+
			"excess candidates were not dropped at parse time", len(sigs), maxSigCandidates)
	}
}
