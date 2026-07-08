// Command spec-dump prints the gateway's served OpenAPI document as
// canonical, indented JSON on stdout. It is the single generator behind
// clients/openapi.json: .github/workflows/client-sdk-drift.yml runs this
// command and fails the build if the committed snapshot differs from its
// output, so the client SDKs can never silently drift from what the gateway
// actually serves at GET /openapi.json.
//
// It calls the exact same production code path routes.go uses to mount
// /openapi.json (openapi.Handler over server.V1Routes), so the bulk of the
// document is byte-identical to what a running gateway returns.
//
// One addition: GET /v1/webhooks/deliveries (routes.go's
// webhookDeliveriesHandler) is a real, shipped framework endpoint that has
// not yet been folded into openapi.Handler's layered path set (unlike
// /v1/usage, /v1/keys*, /v1/webhooks/endpoints*, and /v1/errors, which are).
// Documenting it here — additively, without touching openapi.go — lets the
// SDK generator cover it now; the layering should move into
// gateway/internal/openapi alongside its siblings in a follow-up change.
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

	out, err := withDeliveriesPath(body)
	if err != nil {
		return err
	}

	_, err = w.Write(out)
	return err
}

// servedDocument renders the same JSON bytes routes.go's
// `r.Get("/openapi.json", openapi.Handler(routes))` serves in production.
func servedDocument() ([]byte, error) {
	routes := make([]openapi.RouteDescriptor, len(server.V1Routes))
	copy(routes, server.V1Routes)

	handler := openapi.Handler(routes)
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

// withDeliveriesPath splices the /v1/webhooks/deliveries PathItem into the
// served document's paths object (only if not already present — so this
// becomes a no-op the moment openapi.go documents the route itself) and
// returns the canonical indented JSON, terminated with a trailing newline.
func withDeliveriesPath(served []byte) ([]byte, error) {
	var doc rawDocument
	if err := json.Unmarshal(served, &doc); err != nil {
		return nil, fmt.Errorf("decode served document: %w", err)
	}

	var paths map[string]json.RawMessage
	if err := json.Unmarshal(doc.Paths, &paths); err != nil {
		return nil, fmt.Errorf("decode served document paths: %w", err)
	}

	const deliveriesPath = "/v1/webhooks/deliveries"
	if _, exists := paths[deliveriesPath]; !exists {
		item, err := json.Marshal(deliveriesPathItem())
		if err != nil {
			return nil, fmt.Errorf("marshal deliveries path item: %w", err)
		}
		paths[deliveriesPath] = item
	}

	newPaths, err := json.Marshal(paths)
	if err != nil {
		return nil, fmt.Errorf("marshal paths: %w", err)
	}
	doc.Paths = newPaths

	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal document: %w", err)
	}
	return append(out, '\n'), nil
}

// deliveriesPathItem documents GET /v1/webhooks/deliveries: the
// authenticated customer's paginated outbound webhook delivery attempts
// across all their registered endpoints (gateway/internal/server's
// webhookDeliveriesHandler, wire shape paging.Page[item] with fields id,
// event_id, endpoint_url, status, attempts, last_response_code, created_at).
func deliveriesPathItem() openapi.PathItem {
	itemProps := map[string]*openapi.Schema{
		"id":                 {Type: "string", Description: "Delivery attempt UUID"},
		"event_id":           {Type: "string", Description: "The webhook event UUID this delivery attempt is for"},
		"endpoint_url":       {Type: "string", Description: "The destination URL this attempt was delivered to"},
		"status":             {Type: "string", Description: "Delivery status, e.g. \"delivered\" or \"failed\""},
		"attempts":           {Type: "integer", Description: "Number of delivery attempts made so far"},
		"last_response_code": {Type: "integer", Description: "HTTP status returned by the endpoint on its last attempt; absent if none was ever reached"},
		"created_at":         {Type: "string", Description: "RFC3339 creation timestamp"},
	}
	responseSchema := &openapi.Schema{
		Type:        "object",
		Description: "Paginated envelope of the caller's outbound webhook delivery attempts",
		Properties: map[string]*openapi.Schema{
			"items": {Type: "array", Description: "Delivery attempts for this page, newest-first", Properties: itemProps},
			"total": {Type: "integer", Description: "Total delivery attempts across all pages"},
		},
		Required: []string{"items", "total"},
	}
	errSchema := &openapi.Schema{Ref: "#/components/schemas/Error"}

	return openapi.PathItem{
		Get: &openapi.Operation{
			OperationID: "list_webhook_deliveries",
			Summary:     "List the authenticated customer's outbound webhook delivery attempts across all registered endpoints",
			Tags:        []string{"webhooks"},
			Security:    []openapi.SecurityRequirement{{"ApiKeyAuth": {}}},
			Parameters: []openapi.Parameter{
				{Name: "page", In: "query", Description: "1-indexed page number; default 1", Schema: &openapi.Schema{Type: "integer"}},
				{Name: "per_page", In: "query", Description: "Page size; default 100, capped at 100", Schema: &openapi.Schema{Type: "integer"}},
			},
			Responses: map[string]openapi.Response{
				"200": {
					Description: "The caller's webhook delivery attempts, newest-first",
					Content:     map[string]openapi.MediaType{"application/json": {Schema: responseSchema}},
				},
				"401": {Description: "Unauthorized — missing or invalid API key", Content: map[string]openapi.MediaType{"application/json": {Schema: errSchema}}},
				"500": {Description: "Internal server error", Content: map[string]openapi.MediaType{"application/json": {Schema: errSchema}}},
			},
		},
	}
}
