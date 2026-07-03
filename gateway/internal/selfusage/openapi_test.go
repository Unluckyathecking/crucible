package selfusage_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Unluckyathecking/crucible/gateway/internal/openapi"
)

// TestOpenAPI_UsagePathDocumented asserts GET /v1/usage is present in the
// actual served /openapi.json (openapi.Handler's output) with a 200 response
// and API-key security. /v1/usage is deliberately absent from
// openapi.Build()'s own return value — it's framework infra, not a
// per-product invoke route, and server.TestV1RoutesDriftGuard asserts every
// /v1/* path Build() produces (other than /v1/billing/*) is POST-only. The
// endpoint is layered onto the document inside Handler instead, so it's
// documented for API consumers without perturbing that invoke-route guarantee.
func TestOpenAPI_UsagePathDocumented(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/openapi.json", nil)
	rec := httptest.NewRecorder()
	openapi.Handler(nil)(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}

	var doc struct {
		Paths map[string]struct {
			Get *struct {
				Security  []map[string][]string `json:"security"`
				Responses map[string]struct {
					Description string `json:"description"`
				} `json:"responses"`
			} `json:"get"`
			Post json.RawMessage `json:"post"`
		} `json:"paths"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&doc); err != nil {
		t.Fatalf("decode /openapi.json: %v", err)
	}

	item, ok := doc.Paths["/v1/usage"]
	if !ok {
		t.Fatal("served /openapi.json missing /v1/usage path")
	}
	if item.Get == nil {
		t.Fatal("/v1/usage has no GET operation documented")
	}
	if item.Post != nil {
		t.Error("/v1/usage should not document a POST operation")
	}
	if _, ok := item.Get.Responses["200"]; !ok {
		t.Error("/v1/usage GET missing a 200 response schema")
	}
	if len(item.Get.Security) == 0 {
		t.Error("/v1/usage GET should require API key security like other authenticated endpoints")
	}
}
