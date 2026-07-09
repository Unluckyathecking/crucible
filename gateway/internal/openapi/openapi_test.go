package openapi_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/Unluckyathecking/crucible/gateway/internal/events"
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

	// Lock idempotency response descriptions so a 409↔422 description swap is caught.
	if resp, ok := echo.Post.Responses["409"]; !ok {
		t.Fatal("POST /v1/echo missing 409 response")
	} else if resp.Description != "Idempotency conflict — concurrent request with same key" {
		t.Errorf("409 description = %q; want idempotency-conflict description", resp.Description)
	}
	if resp, ok := echo.Post.Responses["422"]; !ok {
		t.Fatal("POST /v1/echo missing 422 response")
	} else if resp.Description != "Idempotency key reused with a different request body" {
		t.Errorf("422 description = %q; want idempotency-reuse description", resp.Description)
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
	// Verify Handler copies the slice at construction time so that caller
	// mutations between Handler() and the first request cannot affect the document.
	routes := []openapi.RouteDescriptor{
		{Path: "/echo", Operation: "echo", Summary: "Echo"},
	}

	handler := openapi.Handler(routes)

	// Mutate BEFORE the first request: the copy made in Handler() must isolate
	// the built document from the original slice.
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
		t.Error("Handler lost original /v1/echo — defensive copy not working")
	}
}

// TestBuild_RejectsUnderscoreInPath verifies that validateRouteDescriptor rejects
// paths containing underscores. OperationIDFromPath uses _ as its escape character
// (replacing / and -), so a literal _ in the path would produce ambiguous operationIds
// (e.g., /a_b and /a-b both map to invoke_a_b), breaking SDK codegen.
func TestBuild_RejectsUnderscoreInPath(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("Build with underscore path did not panic")
		}
		if msg := fmt.Sprintf("%v", r); !strings.Contains(msg, "underscore") {
			t.Fatalf("panic message does not mention underscore: %v", r)
		}
	}()
	openapi.Build([]openapi.RouteDescriptor{
		{Path: "/my_path", Operation: "my_path", Summary: "Underscore path"},
	})
}

// TestBuild_RejectsEmptyPath verifies that validateRouteDescriptor panics when
// Path is the empty string.
func TestBuild_RejectsEmptyPath(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("Build with empty path did not panic")
		}
		if msg := fmt.Sprintf("%v", r); !strings.Contains(msg, "must start with /") {
			t.Fatalf("panic message does not mention 'must start with /': %v", r)
		}
	}()
	openapi.Build([]openapi.RouteDescriptor{
		{Path: "", Operation: "empty", Summary: "Empty path"},
	})
}

// TestSchemaMarshalJSON_BoolFalse verifies that Schema{BoolFalse:true} marshals as
// JSON false, and that a BoolFalse nested inside a parent schema also marshals
// correctly as false. This exercises the custom MarshalJSON / schemaAlias approach
// which relies on value-receiver promotion to avoid infinite recursion.
func TestSchemaMarshalJSON_BoolFalse(t *testing.T) {
	s := openapi.Schema{BoolFalse: true}
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("marshal Schema{BoolFalse:true}: %v", err)
	}
	if string(b) != "false" {
		t.Errorf("BoolFalse schema marshaled to %q; want \"false\"", b)
	}

	// Nested BoolFalse inside additionalProperties must also marshal as false.
	parent := openapi.Schema{
		Type: "object",
		Properties: map[string]*openapi.Schema{
			"name": {Type: "string"},
		},
		AdditionalProperties: &openapi.Schema{BoolFalse: true},
	}
	pb, err := json.Marshal(parent)
	if err != nil {
		t.Fatalf("marshal parent schema: %v", err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(pb, &raw); err != nil {
		t.Fatalf("unmarshal parent schema: %v", err)
	}
	ap, ok := raw["additionalProperties"]
	if !ok {
		t.Fatal("marshaled parent schema missing additionalProperties field")
	}
	if string(ap) != "false" {
		t.Errorf("nested BoolFalse marshaled to %s; want false", ap)
	}
}

// TestBuild_OperationIDUniqueness verifies that Build panics when two paths would
// produce the same operationId after normalization. With the current scheme (/ → __, - → _),
// a double-hyphen segment and a slash produce the same escape: /a--b and /a/b both
// yield invoke_a__b.
func TestBuild_OperationIDUniqueness(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("Build with duplicate operationId paths did not panic")
		}
		if msg := fmt.Sprintf("%v", r); !strings.Contains(msg, "operationId") {
			t.Fatalf("panic message does not mention operationId: %v", r)
		}
	}()
	openapi.Build([]openapi.RouteDescriptor{
		{Path: "/a--b", Operation: "a--b", Summary: "First"},
		{Path: "/a/b", Operation: "a/b", Summary: "Second"},
	})
}

// TestOpenAPI_ErrorsPathDocumented asserts GET /v1/errors is present in the
// actual served /openapi.json (openapi.Handler's output) with a 200 response
// and API-key security. /v1/errors is deliberately absent from
// openapi.Build()'s own return value — it's framework infra, not a
// per-product invoke route, and server.TestV1RoutesDriftGuard asserts every
// /v1/* path Build() produces (other than /v1/billing/*) is POST-only. The
// endpoint is layered onto the document inside Handler instead, mirroring
// usagePathItem/keysPathItems, so it's documented for API consumers without
// perturbing that invoke-route guarantee.
func TestOpenAPI_ErrorsPathDocumented(t *testing.T) {
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

	item, ok := doc.Paths["/v1/errors"]
	if !ok {
		t.Fatal("served /openapi.json missing /v1/errors path")
	}
	if item.Get == nil {
		t.Fatal("/v1/errors has no GET operation documented")
	}
	if item.Post != nil {
		t.Error("/v1/errors should not document a POST operation")
	}
	if _, ok := item.Get.Responses["200"]; !ok {
		t.Error("/v1/errors GET missing a 200 response schema")
	}
	if len(item.Get.Security) == 0 {
		t.Error("/v1/errors GET should require API key security like other authenticated endpoints")
	}
}

// TestOpenAPI_ErrorsPathPreservesProductPOST asserts that if a product clone
// names a per-product invoke route "/errors" (POST /v1/errors, registered
// through the ordinary V1Routes mechanism), layering the self-service GET
// onto the document doesn't clobber that route's documentation.
func TestOpenAPI_ErrorsPathPreservesProductPOST(t *testing.T) {
	routes := []openapi.RouteDescriptor{
		{Path: "/errors", Operation: "errors", Summary: "Invoke errors worker operation (authenticated)"},
	}

	req := httptest.NewRequest(http.MethodGet, "/openapi.json", nil)
	rec := httptest.NewRecorder()
	openapi.Handler(routes)(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}

	var doc struct {
		Paths map[string]struct {
			Get  json.RawMessage `json:"get"`
			Post json.RawMessage `json:"post"`
		} `json:"paths"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&doc); err != nil {
		t.Fatalf("decode /openapi.json: %v", err)
	}

	item, ok := doc.Paths["/v1/errors"]
	if !ok {
		t.Fatal("served /openapi.json missing /v1/errors path")
	}
	if item.Get == nil {
		t.Error("/v1/errors lost its self-service GET operation when a product POST route shares the path")
	}
	if item.Post == nil {
		t.Error("/v1/errors lost its product POST operation — self-service GET overwrote it")
	}
}

// TestBuild_SampleRequestSurfacesAsExample verifies that a route's
// RouteDescriptor.SampleRequest is emitted verbatim as the request body's
// "example" value (MediaType.Example) for that route's operation.
func TestBuild_SampleRequestSurfacesAsExample(t *testing.T) {
	sample := json.RawMessage(`{"input":"hello"}`)
	routes := []openapi.RouteDescriptor{
		{
			Path:      "/custom-op",
			Operation: "custom-op",
			Summary:   "Custom operation",
			RequestSchema: &openapi.Schema{
				Type:       "object",
				Properties: map[string]*openapi.Schema{"input": {Type: "string"}},
				Required:   []string{"input"},
			},
			SampleRequest: sample,
		},
	}
	doc := openapi.Build(routes)
	item, ok := doc.Paths["/v1/custom-op"]
	if !ok || item.Post == nil {
		t.Fatal("missing POST /v1/custom-op")
	}
	media, ok := item.Post.RequestBody.Content["application/json"]
	if !ok {
		t.Fatal("missing application/json request body content")
	}
	if string(media.Example) != string(sample) {
		t.Errorf("example = %s, want %s", media.Example, sample)
	}

	// Round-trip through JSON to confirm the example marshals as a literal
	// JSON value (not a quoted string) inside the served document.
	b, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal doc: %v", err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatalf("unmarshal doc: %v", err)
	}
	var paths map[string]struct {
		Post struct {
			RequestBody struct {
				Content struct {
					ApplicationJSON struct {
						Example json.RawMessage `json:"example"`
					} `json:"application/json"`
				} `json:"content"`
			} `json:"requestBody"`
		} `json:"post"`
	}
	if err := json.Unmarshal(raw["paths"], &paths); err != nil {
		t.Fatalf("unmarshal paths: %v", err)
	}
	gotExample := paths["/v1/custom-op"].Post.RequestBody.Content.ApplicationJSON.Example
	if string(gotExample) != string(sample) {
		t.Errorf("served example = %s, want %s", gotExample, sample)
	}
}

// TestBuild_NilSampleRequestOmitsExample verifies that a route with no
// SampleRequest produces no "example" key at all (omitempty), rather than a
// literal JSON null.
func TestBuild_NilSampleRequestOmitsExample(t *testing.T) {
	doc := openapi.Build(testRoutes) // testRoutes' /echo declares no SampleRequest
	item, ok := doc.Paths["/v1/echo"]
	if !ok || item.Post == nil {
		t.Fatal("missing POST /v1/echo")
	}
	media, ok := item.Post.RequestBody.Content["application/json"]
	if !ok {
		t.Fatal("missing application/json request body content")
	}
	if media.Example != nil {
		t.Errorf("example = %s, want nil (omitted)", media.Example)
	}

	b, err := json.Marshal(item.Post.RequestBody.Content["application/json"])
	if err != nil {
		t.Fatalf("marshal media type: %v", err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatalf("unmarshal media type: %v", err)
	}
	if _, present := raw["example"]; present {
		t.Error("marshaled media type has an \"example\" key; want it omitted entirely")
	}
}

// TestBuild_WebhookEventCatalogueLocked asserts the document's `webhooks` section
// documents exactly the event types in events.AllEventTypes — no more, no fewer.
// Build() itself panics on drift (see buildWebhooks), so an added/removed/renamed
// event type that isn't mirrored into openapi's descriptor list fails this test
// (via the panic) instead of silently shipping an incomplete spec.
func TestBuild_WebhookEventCatalogueLocked(t *testing.T) {
	doc := openapi.Build(testRoutes)

	if len(doc.Webhooks) != len(events.AllEventTypes) {
		t.Fatalf("doc.Webhooks has %d entries, want %d (events.AllEventTypes)", len(doc.Webhooks), len(events.AllEventTypes))
	}
	for _, et := range events.AllEventTypes {
		op, ok := doc.Webhooks[et]
		if !ok {
			t.Errorf("doc.Webhooks missing event type %q", et)
			continue
		}
		if op.Post == nil {
			t.Errorf("doc.Webhooks[%q] has no POST operation", et)
			continue
		}
		if op.Post.Summary == "" {
			t.Errorf("doc.Webhooks[%q] has empty summary", et)
		}
		if op.Post.RequestBody == nil {
			t.Errorf("doc.Webhooks[%q] has no request body", et)
			continue
		}
		if _, ok := op.Post.RequestBody.Content["application/json"]; !ok {
			t.Errorf("doc.Webhooks[%q] request body has no application/json schema", et)
		}
	}
}
