package openapi_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"testing"

	"github.com/Unluckyathecking/crucible/gateway/internal/openapi"
)

// codegenParam and codegenOp are deliberately minimal, hand-rolled JSON
// targets — NOT openapi.Parameter/openapi.Operation — so these tests decode
// the served document's raw bytes without depending on openapi.Schema's
// marshal-only BoolFalse behavior (which is not safe to round-trip through
// json.Unmarshal). They only capture the fields these tests actually check.
type codegenParam struct {
	Name   string `json:"name"`
	In     string `json:"in"`
	Schema struct {
		Type string `json:"type"`
	} `json:"schema"`
}

type codegenOp struct {
	OperationID string         `json:"operationId"`
	Parameters  []codegenParam `json:"parameters"`
}

// pathParamRE matches a {name} path template segment.
var pathParamRE = regexp.MustCompile(`\{(\w*)\}`)

// codegenIdentRE is what scripts/gen-clients.sh's param_arg_name assumes:
// a path/query parameter name is one or more \w+ groups separated by "_",
// safe to turn into a Go/TS identifier by camel-casing on "_".
var codegenIdentRE = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9]*(_[A-Za-z][A-Za-z0-9]*)*$`)

// servedPaths renders the document exactly as routes.go's
// `r.Get("/openapi.json", openapi.Handler(routes))` serves it — the same
// production code path scripts/gen-clients.sh's input (via gateway/cmd/spec-dump)
// is built from — and decodes its "paths" object into method->op maps.
func servedPaths(t *testing.T) map[string]map[string]codegenOp {
	t.Helper()
	handler := openapi.Handler(testRoutes)
	rec := httptest.NewRecorder()
	handler(rec, httptest.NewRequest(http.MethodGet, "/openapi.json", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("openapi.Handler returned status %d", rec.Code)
	}

	var doc struct {
		Paths map[string]map[string]codegenOp `json:"paths"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("decode served document: %v", err)
	}
	return doc.Paths
}

// TestOpenAPI_PathParamNamesAreCodegenSafe guards an assumption
// scripts/gen-clients.sh depends on: every {name} path-template segment is a
// valid identifier (so it can become a Go/TS function argument), and every
// {name} in the literal path has a matching "in": "path" parameter declared
// on the operation (so gen-clients.sh's Op.path_params — parsed straight off
// the path string — always lines up with a real, typed parameter). A route
// that violates this compiles/runs fine at the gateway layer but would
// silently produce a broken or missing SDK method.
func TestOpenAPI_PathParamNamesAreCodegenSafe(t *testing.T) {
	for path, methods := range servedPaths(t) {
		segmentNames := map[string]bool{}
		for _, m := range pathParamRE.FindAllStringSubmatch(path, -1) {
			name := m[1]
			if !codegenIdentRE.MatchString(name) {
				t.Errorf("path %q: path-param name %q is not codegen-safe (must match %s)", path, name, codegenIdentRE.String())
			}
			segmentNames[name] = true
		}

		for method, op := range methods {
			declared := map[string]bool{}
			for _, p := range op.Parameters {
				if p.In != "path" {
					continue
				}
				declared[p.Name] = true
				if !segmentNames[p.Name] {
					t.Errorf("%s %s: operation %s declares path parameter %q not present as {%s} in the literal path", method, path, op.OperationID, p.Name, p.Name)
				}
			}
			for name := range segmentNames {
				if !declared[name] {
					t.Errorf("%s %s: literal path segment {%s} has no matching \"in\":\"path\" parameter on operation %s", method, path, name, op.OperationID)
				}
			}
		}
	}
}

// TestOpenAPI_QueryParamTypesAreCodegenSupported guards the other half of
// scripts/gen-clients.sh's assumption: it only knows how to type-map query
// parameters declared as "integer" (-> Go int64 / TS number, zero/undefined
// meaning "unset") or "string" (-> Go string / TS string, empty/undefined
// meaning "unset"). A query parameter with any other schema type would be
// silently mis-typed by the generator; this test fails loudly instead,
// telling whoever adds one to extend gen-clients.sh's query-param handling
// first.
func TestOpenAPI_QueryParamTypesAreCodegenSupported(t *testing.T) {
	supported := map[string]bool{"string": true, "integer": true}
	for path, methods := range servedPaths(t) {
		for method, op := range methods {
			for _, p := range op.Parameters {
				if p.In != "query" {
					continue
				}
				if !codegenIdentRE.MatchString(p.Name) {
					t.Errorf("%s %s: query parameter name %q is not codegen-safe (must match %s)", method, path, p.Name, codegenIdentRE.String())
				}
				if !supported[p.Schema.Type] {
					t.Errorf("%s %s: operation %s has query parameter %q with schema type %q; scripts/gen-clients.sh only supports \"string\" and \"integer\" — extend it before adding this type", method, path, op.OperationID, p.Name, p.Schema.Type)
				}
			}
		}
	}
}
