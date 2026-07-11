// Command spec-dump prints the gateway's served OpenAPI document as
// canonical, indented JSON on stdout. It is the single generator behind
// clients/openapi.json: .github/workflows/client-sdk-drift.yml runs this
// command and fails the build if the committed snapshot differs from its
// output, so the client SDKs can never silently drift from what the gateway
// actually serves at GET /openapi.json.
//
// It calls the exact same production code path routes.go uses to mount
// /openapi.json (openapi.Handler over server.V1Routes) and only re-indents
// the result — it never adds, removes, or otherwise edits paths. That is a
// deliberate constraint, not an oversight: an earlier version of this file
// additively documented GET /v1/webhooks/deliveries (a real, shipped route
// not yet folded into openapi.Handler's layered path set) so the SDK could
// cover it. That made this command's output diverge from what GET
// /openapi.json actually returns, which defeats the entire point of the
// drift guard — a snapshot that's "accurate plus one invented path" is not
// accurate. Folding /v1/webhooks/deliveries into openapi.Handler belongs in
// gateway/internal/openapi (out of this change's file scope); until then,
// the SDKs simply don't cover that route, same as the served document
// doesn't document it.
package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"

	"github.com/Unluckyathecking/crucible/gateway/internal/openapi"
	"github.com/Unluckyathecking/crucible/gateway/internal/server"
)

func main() {
	if err := run(os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "spec-dump:", err)
		os.Exit(1)
	}
}

func run(w *os.File) error {
	body, err := servedDocument()
	if err != nil {
		return err
	}

	out, err := reindent(body)
	if err != nil {
		return err
	}

	_, err = w.Write(out)
	return err
}

// servedDocument renders the same JSON bytes routes.go's
// `r.Get("/openapi.json", openapi.Handler(routes))` serves in production.
func servedDocument() ([]byte, error) {
	handler := openapi.Handler(server.AnnotatedRoutes())
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/openapi.json", nil)
	handler(rec, req)

	if rec.Code != http.StatusOK {
		return nil, fmt.Errorf("openapi.Handler returned status %d", rec.Code)
	}
	return rec.Body.Bytes(), nil
}

// rawDocument mirrors openapi.Document's field order with json.RawMessage
// fields, so re-marshaling preserves the original top-level key order
// without needing to round-trip every nested schema type (some, like
// Schema's BoolFalse, have custom marshal-only behavior not safe to
// unmarshal generically).
type rawDocument struct {
	OpenAPI    json.RawMessage `json:"openapi"`
	Info       json.RawMessage `json:"info"`
	Servers    json.RawMessage `json:"servers,omitempty"`
	Paths      json.RawMessage `json:"paths"`
	Webhooks   json.RawMessage `json:"webhooks,omitempty"`
	Components json.RawMessage `json:"components"`
}

// reindent re-marshals the served document's raw bytes as 2-space-indented
// JSON terminated with a trailing newline (openapi.Handler serves it
// minified). It decodes only the fixed top-level shape above — every nested
// value passes through as json.RawMessage, untouched — so this is purely a
// formatting step, never a content change.
func reindent(served []byte) ([]byte, error) {
	var doc rawDocument
	if err := json.Unmarshal(served, &doc); err != nil {
		return nil, fmt.Errorf("decode served document: %w", err)
	}

	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal document: %w", err)
	}
	return append(out, '\n'), nil
}
