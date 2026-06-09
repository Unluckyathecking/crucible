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

// testRoutes mirrors the current V1Routes used in production (server/routes_table.go).
// Tests use this to exercise openapi.Build() and openapi.Handler() end-to-end.
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
	for _, code := range []string{"400", "401", "429", "500", "502"} {
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

func TestBuild_NoHardcodedV1EchoPath(t *testing.T) {
	// Acceptance: no hard-coded Paths["/v1/echo"] literal remains in Build().
	// Verify that Build() with an empty route list produces no /v1/* paths at all.
	doc := openapi.Build([]openapi.RouteDescriptor{})
	for path := range doc.Paths {
		if strings.HasPrefix(path, "/v1/") {
			t.Errorf("Build() with empty routes produced unexpected /v1 path %q", path)
		}
	}
}

func TestBuild_InvokeRouteDerivedFromDescriptor(t *testing.T) {
	// Verify that Build() derives /v1/* paths from the descriptor, not hard-coded literals.
	routes := []openapi.RouteDescriptor{
		{Path: "/custom-op", Operation: "custom-op", Summary: "Custom operation"},
	}
	doc := openapi.Build(routes)
	item, ok := doc.Paths["/v1/custom-op"]
	if !ok {
		t.Fatal("Build() did not produce /v1/custom-op from descriptor")
	}
	if item.Post == nil {
		t.Fatal("/v1/custom-op has no POST operation")
	}
	if item.Post.OperationID != "invoke_custom-op" {
		t.Errorf("operationId = %q; want invoke_custom-op", item.Post.OperationID)
	}
	if item.Post.Summary != "Custom operation" {
		t.Errorf("summary = %q; want Custom operation", item.Post.Summary)
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
