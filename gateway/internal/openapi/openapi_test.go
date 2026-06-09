package openapi_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/Unluckyathecking/crucible/gateway/internal/openapi"
)

// testRoutes is a fixed set for unit-testing the OpenAPI builder.
// Production sync is enforced by TestV1RoutesDriftGuard in gateway/internal/server/routes_test.go.
var testRoutes = []openapi.RouteDescriptor{
	{Path: "/echo", Operation: "echo", Summary: "Invoke echo worker operation (authenticated)"},
}

func TestBuild_Version(t *testing.T) {
	doc := openapi.Build(testRoutes)
	if doc.OpenAPI != "3.1.0" {
		t.Errorf("openapi = %q; want 3.1.0", doc.OpenAPI)
	}
}

func TestBuild_RequiredPaths(t *testing.T) {
	doc := openapi.Build(testRoutes)
	for _, path := range []string{"/healthz", "/readyz", "/metrics", "/v1/echo"} {
		if _, ok := doc.Paths[path]; !ok {
			t.Errorf("missing required path %q", path)
		}
	}
}

func TestBuild_SecurityScheme(t *testing.T) {
	doc := openapi.Build(testRoutes)
	scheme, ok := doc.Components.SecuritySchemes["ApiKeyAuth"]
	if !ok {
		t.Fatal("components.securitySchemes missing ApiKeyAuth")
	}
	if scheme.Type != "apiKey" {
		t.Errorf("ApiKeyAuth type = %q; want apiKey", scheme.Type)
	}
	if scheme.In != "header" {
		t.Errorf("ApiKeyAuth in = %q; want header", scheme.In)
	}
	if scheme.Name != "X-API-Key" {
		t.Errorf("ApiKeyAuth name = %q; want X-API-Key", scheme.Name)
	}
	if scheme.Description == "" {
		t.Error("ApiKeyAuth description is empty")
	}
}

func TestBuild_ErrorComponentDeclaredOnce(t *testing.T) {
	doc := openapi.Build(testRoutes)
	if _, ok := doc.Components.Schemas["Error"]; !ok {
		t.Fatal("components.schemas missing Error")
	}
}

func TestBuild_ErrorResponsesUseRef(t *testing.T) {
	const wantRef = "#/components/schemas/Error"
	doc := openapi.Build(testRoutes)
	echo, ok := doc.Paths["/v1/echo"]
	if !ok || echo.Post == nil {
		t.Fatal("missing POST /v1/echo")
	}
	// Verify 200 has an object schema (not an error ref).
	if resp200, ok := echo.Post.Responses["200"]; !ok {
		t.Fatal("missing 200 response")
	} else if media, ok := resp200.Content["application/json"]; !ok || media.Schema == nil {
		t.Fatal("200 response missing application/json schema")
	} else if media.Schema.Ref != "" {
		t.Errorf("200 response should not be a $ref, got %q", media.Schema.Ref)
	} else if media.Schema.Type != "object" {
		t.Errorf("200 response type = %q; want object", media.Schema.Type)
	}

	// Verify each error code is present and uses the Error $ref (no inline duplication).
	// 409 = idempotency conflict, 422 = idempotency key reused with different body.
	for _, code := range []string{"400", "401", "409", "422", "429", "500", "502"} {
		resp, ok := echo.Post.Responses[code]
		if !ok {
			t.Fatalf("POST /v1/echo missing response %s", code)
		}
		media, ok := resp.Content["application/json"]
		if !ok {
			t.Fatalf("response %s missing application/json content", code)
		}
		if media.Schema == nil || media.Schema.Ref != wantRef {
			t.Errorf("response %s: want schema.$ref=%q, got %v", code, wantRef, media.Schema)
		}
	}
}

func TestBuild_InvokeRouteSecured(t *testing.T) {
	doc := openapi.Build(testRoutes)
	echo := doc.Paths["/v1/echo"]
	if echo.Post == nil {
		t.Fatal("missing POST /v1/echo")
	}
	if len(echo.Post.Security) == 0 {
		t.Fatal("POST /v1/echo has no security requirements")
	}
	if _, ok := echo.Post.Security[0]["ApiKeyAuth"]; !ok {
		t.Error("POST /v1/echo security does not reference ApiKeyAuth")
	}
}

func TestBuild_UnauthenticatedRoutesHaveNoSecurity(t *testing.T) {
	doc := openapi.Build(testRoutes)
	unauthenticated := []struct {
		path   string
		method string
	}{
		{"/healthz", "get"},
		{"/readyz", "get"},
		{"/metrics", "get"},
		{"/webhooks/stripe", "post"},
	}
	for _, tc := range unauthenticated {
		item, ok := doc.Paths[tc.path]
		if !ok {
			t.Errorf("missing path %q", tc.path)
			continue
		}
		var op *openapi.Operation
		switch tc.method {
		case "get":
			op = item.Get
		case "post":
			op = item.Post
		}
		if op == nil {
			t.Errorf("missing %s %s operation", tc.method, tc.path)
			continue
		}
		if len(op.Security) != 0 {
			t.Errorf("%s %s should have no security requirements, got %v", tc.method, tc.path, op.Security)
		}
	}
}

func TestBuild_InvokeRouteDerived(t *testing.T) {
	// Verify Build() derives /v1/* paths from descriptors (no hard-coded literals).
	cases := []struct {
		name          string
		routes        []openapi.RouteDescriptor
		wantPaths     []string
		wantAbsent    []string
		wantOpIDs     map[string]string // path → operationId
	}{
		{
			name:       "empty routes produces no /v1 paths",
			routes:     []openapi.RouteDescriptor{},
			wantAbsent: []string{"/v1/echo"},
		},
		{
			name: "single route",
			routes: []openapi.RouteDescriptor{
				{Path: "/custom-op", Operation: "custom-op", Summary: "Custom operation"},
			},
			wantPaths:  []string{"/v1/custom-op"},
			wantAbsent: []string{"/v1/echo"},
			wantOpIDs:  map[string]string{"/v1/custom-op": "invoke_custom_op"},
		},
		{
			name: "hyphenated path like validate-vat",
			routes: []openapi.RouteDescriptor{
				{Path: "/validate-vat", Operation: "validate-vat", Summary: "Validate VAT number"},
			},
			wantPaths: []string{"/v1/validate-vat"},
			wantOpIDs: map[string]string{"/v1/validate-vat": "invoke_validate_vat"},
		},
		{
			name: "multiple routes",
			routes: []openapi.RouteDescriptor{
				{Path: "/echo", Operation: "echo", Summary: "Echo"},
				{Path: "/summarise", Operation: "summarise", Summary: "Summarise"},
			},
			wantPaths: []string{"/v1/echo", "/v1/summarise"},
			wantOpIDs: map[string]string{
				"/v1/echo":      "invoke_echo",
				"/v1/summarise": "invoke_summarise",
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			doc := openapi.Build(tc.routes)

			for _, p := range tc.wantPaths {
				item, ok := doc.Paths[p]
				if !ok {
					t.Errorf("missing expected path %q", p)
					continue
				}
				if item.Post == nil {
					t.Errorf("path %q has no POST operation", p)
				}
			}
			for _, p := range tc.wantAbsent {
				if _, ok := doc.Paths[p]; ok {
					t.Errorf("path %q should be absent", p)
				}
			}
			for path, wantID := range tc.wantOpIDs {
				item, ok := doc.Paths[path]
				if !ok || item.Post == nil {
					continue // already reported above
				}
				if item.Post.OperationID != wantID {
					t.Errorf("path %q: operationId = %q, want %q", path, item.Post.OperationID, wantID)
				}
			}
		})
	}
}

func TestHandler_Response(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/openapi.json", nil)
	w := httptest.NewRecorder()

	openapi.Handler(testRoutes)(w, req)

	res := w.Result()

	if res.StatusCode != http.StatusOK {
		t.Errorf("status = %d; want 200", res.StatusCode)
	}
	ct := res.Header.Get("Content-Type")
	if !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q; want application/json", ct)
	}

	var raw map[string]json.RawMessage
	if err := json.NewDecoder(res.Body).Decode(&raw); err != nil {
		t.Fatalf("decode: %v", err)
	}

	rawOpenAPI, ok := raw["openapi"]
	if !ok {
		t.Fatal("missing openapi field in document")
	}
	var version string
	if err := json.Unmarshal(rawOpenAPI, &version); err != nil {
		t.Fatalf("unmarshal openapi: %v", err)
	}
	if version != "3.1.0" {
		t.Errorf("openapi = %q; want 3.1.0", version)
	}

	var paths map[string]json.RawMessage
	if err := json.Unmarshal(raw["paths"], &paths); err != nil {
		t.Fatalf("decode paths: %v", err)
	}
	if len(paths) == 0 {
		t.Error("paths is empty")
	}
	for _, want := range []string{"/healthz", "/v1/echo"} {
		if _, ok := paths[want]; !ok {
			t.Errorf("paths missing %q", want)
		}
	}
}

func TestHandler_ConcurrentAccess(t *testing.T) {
	const goroutines = 50
	var (
		wg       sync.WaitGroup
		failures atomic.Int32
	)
	handler := openapi.Handler(testRoutes)
	wg.Add(goroutines)
	for range goroutines {
		go func() {
			defer wg.Done()
			req := httptest.NewRequest(http.MethodGet, "/openapi.json", nil)
			w := httptest.NewRecorder()
			handler(w, req)
			if w.Code != http.StatusOK {
				failures.Add(1)
			}
		}()
	}
	wg.Wait()
	if n := failures.Load(); n > 0 {
		t.Errorf("%d concurrent calls returned non-200", n)
	}
}

func TestHandler_DefensiveCopy(t *testing.T) {
	// Verify Handler does not race if the caller mutates the slice after calling Handler.
	// Use a dedicated slice so this test is self-contained and never panics on index 0.
	routes := []openapi.RouteDescriptor{
		{Path: "/echo", Operation: "echo", Summary: "Echo"},
	}

	handler := openapi.Handler(routes)

	// Simulate a caller mutation after Handler() returns.
	routes[0] = openapi.RouteDescriptor{Path: "/mutated", Operation: "mutated", Summary: "Should not appear"}

	req := httptest.NewRequest(http.MethodGet, "/openapi.json", nil)
	w := httptest.NewRecorder()
	handler(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", w.Code)
	}

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(w.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode: %v", err)
	}
	var paths map[string]json.RawMessage
	if err := json.Unmarshal(raw["paths"], &paths); err != nil {
		t.Fatalf("decode paths: %v", err)
	}
	if _, ok := paths["/v1/mutated"]; ok {
		t.Error("Handler served mutated route — defensive copy not working")
	}
	if _, ok := paths["/v1/echo"]; !ok {
		t.Error("Handler lost original /v1/echo after caller mutation — defensive copy not working")
	}
}
