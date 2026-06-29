package conformance

// TestFixtureDrivenConformance loads workers/conformance/fixture.json and asserts
// each case against an in-process Go SDK server. This is the fixture-driven
// companion to Harness: it proves the Go SDK satisfies the language-neutral
// contract spec. The fixture is the single source of truth; adding a case here
// means adding it to the JSON — no per-SDK-only test cases.
//
// Run alone with:
//
//	go test -race ./conformance/... -run TestFixtureDrivenConformance

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	crucible "github.com/Unluckyathecking/crucible/workers/sdk-go"
)

// fixtureDivergence describes a known SDK-specific deviation from the canonical
// expected value for a case (e.g. TS returning 404 instead of 405).
type fixtureDivergence struct {
	Status int    `json:"status"`
	Note   string `json:"note"`
}

// fixtureCase is one entry in workers/conformance/fixture.json.
type fixtureCase struct {
	ID          string `json:"id"`
	Description string `json:"description"`
	Kind        string `json:"kind"`
	// KnownDivergences maps SDK name ("go", "rust", "ts") to divergence details.
	// A runner that finds its own key skips or adjusts the assertion accordingly.
	KnownDivergences map[string]fixtureDivergence `json:"known_divergences"`
}

type fixtureFile struct {
	Version string        `json:"version"`
	Cases   []fixtureCase `json:"cases"`
}

// loadSharedFixture reads workers/conformance/fixture.json relative to the Go test's
// working directory. The Go test runner sets CWD to the package directory
// (workers/sdk-go/conformance/), so the fixture is at ../../conformance/fixture.json.
func loadSharedFixture(t *testing.T) fixtureFile {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "conformance", "fixture.json"))
	if err != nil {
		t.Fatalf("load shared fixture: %v", err)
	}
	var f fixtureFile
	if err := json.Unmarshal(data, &f); err != nil {
		t.Fatalf("parse shared fixture: %v", err)
	}
	return f
}

// TestFixtureDrivenConformance loads the shared fixture and runs each case against
// an in-process Go SDK server. Known Go divergences (none currently) are skipped.
func TestFixtureDrivenConformance(t *testing.T) {
	f := loadSharedFixture(t)
	if len(f.Cases) == 0 {
		t.Fatal("shared fixture loaded zero cases; check workers/conformance/fixture.json")
	}

	echoMux, err := crucible.Handler(func(_ context.Context, _ crucible.Request) (crucible.Response, error) {
		return crucible.Response{Payload: map[string]string{"ok": "true"}, BillableUnits: 1}, nil
	})
	if err != nil {
		t.Fatalf("crucible.Handler(echo): %v", err)
	}
	echoSrv := httptest.NewServer(echoMux)
	t.Cleanup(echoSrv.Close)

	for _, tc := range f.Cases {
		tc := tc
		if div, ok := tc.KnownDivergences["go"]; ok {
			t.Logf("SKIP %s: known Go divergence: %s", tc.ID, div.Note)
			continue
		}
		t.Run(tc.ID, func(t *testing.T) {
			runGoFixtureCase(t, tc, echoSrv)
		})
	}
}

// runGoFixtureCase dispatches a single fixture case to the appropriate assertion helper.
// Each kind maps 1:1 to a helper already proven by the Harness tests.
func runGoFixtureCase(t *testing.T, tc fixtureCase, echoSrv *httptest.Server) {
	t.Helper()
	client := harnessClient()
	defer client.CloseIdleConnections()

	switch tc.Kind {
	case "healthz_body":
		assertHealthz(t, echoSrv, client)

	case "method_not_allowed":
		assertInvokeMethodNotAllowed(t, echoSrv, client)

	case "billable_units_floor":
		assertBillableUnitsNormalization(t, client)

	case "apierror_envelope":
		assertHandlerStructuredError(t, client)

	case "empty_body_bad_request":
		// Empty body is not valid JSON; the SDK must return a BAD_REQUEST error envelope.
		checkErrorEnvelopeAt(t, echoSrv, client, []byte{}, "BAD_REQUEST")

	default:
		t.Fatalf("unknown fixture case kind %q (id=%s): update runGoFixtureCase", tc.Kind, tc.ID)
	}
}

