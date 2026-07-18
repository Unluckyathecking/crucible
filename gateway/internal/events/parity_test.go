// External test package so the parity check doesn't share the package-internal
// binary with events_test.go — keeping concerns separate.
package events_test

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/Unluckyathecking/crucible/gateway/internal/events"
)

// TestGoTSEventTypeParity reads dashboard/lib/db.ts and asserts order-insensitive
// set-equality between WEBHOOK_EVENT_TYPES and events.AllEventTypes. It fails
// whenever the two lists diverge so that Go additions that aren't mirrored into
// the TypeScript constant (or vice-versa) are caught before shipping.
//
// The "go test" working directory for a package is the package directory itself,
// so navigating three levels up from gateway/internal/events reaches the repo root.
func TestGoTSEventTypeParity(t *testing.T) {
	tsPath := filepath.Join("..", "..", "..", "dashboard", "lib", "db.ts")
	src, err := os.ReadFile(tsPath)
	if err != nil {
		t.Fatalf("read %s: %v — is this test run from inside gateway/?", tsPath, err)
	}

	// Extract the WEBHOOK_EVENT_TYPES array body (between [ and ]).
	blockRe := regexp.MustCompile(`(?s)export const WEBHOOK_EVENT_TYPES\s*=\s*\[([^\]]+)\]`)
	m := blockRe.FindSubmatch(src)
	if m == nil {
		t.Fatalf("WEBHOOK_EVENT_TYPES declaration not found in %s", tsPath)
	}

	entryRe := regexp.MustCompile(`"([^"]+)"`)
	tsSet := make(map[string]bool)
	for _, entry := range entryRe.FindAllSubmatch(m[1], -1) {
		tsSet[string(entry[1])] = true
	}

	goSet := make(map[string]bool, len(events.AllEventTypes))
	for _, et := range events.AllEventTypes {
		goSet[et] = true
	}

	for et := range goSet {
		if !tsSet[et] {
			t.Errorf("AllEventTypes has %q but WEBHOOK_EVENT_TYPES in db.ts does not", et)
		}
	}
	for et := range tsSet {
		if !goSet[et] {
			t.Errorf("WEBHOOK_EVENT_TYPES in db.ts has %q but AllEventTypes does not", et)
		}
	}
}
