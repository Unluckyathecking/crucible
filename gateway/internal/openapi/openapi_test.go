package openapi_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
	if scheme.Type != "apiKey" {
		t.Errorf("ApiKeyAuth type = %q; want apiKey", scheme.Type)
	}
	if scheme.In != "header" {
		t.Errorf("ApiKeyAuth in = %q; want header", scheme.In)
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
	for code, resp := range echo.Post.Responses {
		if code == "200" {
			continue
		}
		for mime, media := range resp.Content {
			if media.Schema == nil || media.Schema.Ref != wantRef {
				t.Errorf("POST /v1/echo response %s %s: want schema.$ref=%q, got %v",
					code, mime, wantRef, media.Schema)
			}
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
	if ct := res.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q; want application/json", ct)
	}

	var raw map[string]json.RawMessage
	if err := json.NewDecoder(res.Body).Decode(&raw); err != nil {
		t.Fatalf("decode: %v", err)
	}

	var version string
	if err := json.Unmarshal(raw["openapi"], &version); err != nil || version != "3.1.0" {
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
