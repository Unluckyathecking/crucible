package openapi_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/Unluckyathecking/crucible/gateway/internal/openapi"
)

func TestBuild_Version(t *testing.T) {
	doc := openapi.Build()
	if doc.OpenAPI != "3.1.0" {
		t.Errorf("openapi = %q; want 3.1.0", doc.OpenAPI)
	}
}

func TestBuild_RequiredPaths(t *testing.T) {
	doc := openapi.Build()
	for _, path := range []string{"/healthz", "/v1/echo"} {
		if _, ok := doc.Paths[path]; !ok {
			t.Errorf("missing required path %q", path)
		}
	}
}

func TestBuild_SecurityScheme(t *testing.T) {
	doc := openapi.Build()
	scheme, ok := doc.Components.SecuritySchemes["ApiKeyAuth"]
	if !ok {
		t.Fatal("components.securitySchemes missing ApiKeyAuth")
	}
	// Gateway uses Authorization: Bearer <token>; correct OpenAPI representation is http/bearer.
	if scheme.Type != "http" {
		t.Errorf("ApiKeyAuth type = %q; want http", scheme.Type)
	}
	if scheme.Scheme != "bearer" {
		t.Errorf("ApiKeyAuth scheme = %q; want bearer", scheme.Scheme)
	}
}

func TestBuild_ErrorComponentDeclaredOnce(t *testing.T) {
	doc := openapi.Build()
	if _, ok := doc.Components.Schemas["Error"]; !ok {
		t.Fatal("components.schemas missing Error")
	}
}

func TestBuild_ErrorResponsesUseRef(t *testing.T) {
	const wantRef = "#/components/schemas/Error"
	doc := openapi.Build()
	echo, ok := doc.Paths["/v1/echo"]
	if !ok || echo.Post == nil {
		t.Fatal("missing POST /v1/echo")
	}
	// Explicitly verify each expected error status code is present and uses $ref.
	for _, code := range []string{"400", "401", "429", "502"} {
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
	doc := openapi.Build()
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

func TestHandler_Response(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/openapi.json", nil)
	w := httptest.NewRecorder()

	openapi.Handler()(w, req)

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
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for range goroutines {
		go func() {
			defer wg.Done()
			req := httptest.NewRequest(http.MethodGet, "/openapi.json", nil)
			w := httptest.NewRecorder()
			openapi.Handler()(w, req)
			if w.Code != http.StatusOK {
				t.Errorf("concurrent call: status = %d; want 200", w.Code)
			}
		}()
	}
	wg.Wait()
}
