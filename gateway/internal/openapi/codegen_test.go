package openapi_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Unluckyathecking/crucible/gateway/internal/auth"
	"github.com/Unluckyathecking/crucible/gateway/internal/config"
	"github.com/Unluckyathecking/crucible/gateway/internal/openapi"
	"github.com/Unluckyathecking/crucible/gateway/internal/server"
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

// stubHealthChecker satisfies server.HealthChecker without a live dependency;
// chi.Walk below only enumerates registered routes, it never dispatches a
// request, so Ping is never actually called.
type stubHealthChecker struct{}

func (stubHealthChecker) Ping(_ context.Context) error { return nil }

// selfServiceRoutes lists the framework self-service /v1 paths documented via
// openapi.Handler's hand-maintained layering functions (usagePathItem,
// keysPathItems, webhookEndpointsPathItems, errorsPathItems) rather than
// derived from server.V1Routes like the per-product invoke routes. Because
// they're hand-maintained in two places — the actual chi registration in
// server/routes.go's NewRouter, and the documentation in openapi.go — the two
// can drift independently; this list is what TestOpenAPI_SelfServiceRoutesMatchRouterMounts
// cross-checks.
var selfServiceRoutes = []string{
	"/v1/keys",
	"/v1/keys/{id}",
	"/v1/keys/{id}/rotate",
	"/v1/webhooks/endpoints",
	"/v1/webhooks/endpoints/{id}",
	"/v1/webhooks/endpoints/{id}/rotate-secret",
	"/v1/errors",
	"/v1/usage",
}

// TestOpenAPI_SelfServiceRoutesMatchRouterMounts guards the gap
// TestV1RoutesDriftGuard (gateway/internal/server/routes_test.go) doesn't
// cover: that one only cross-checks per-product /v1 invoke routes (derived
// from the single V1Routes table both NewRouter and openapi.Build read) — it
// never touches the framework self-service routes, which are registered in
// NewRouter directly and documented via openapi.Handler's layering functions
// separately, with no shared source of truth. If a route in one drifts from
// the other (renamed, removed, or a method added/dropped) without updating
// both, callers of the served OpenAPI document — and therefore the SDKs this
// package's other codegen tests guard — would silently document a route
// that doesn't exist, or miss one that does.
//
// Builds the real router the same way TestV1RoutesDriftGuard does: a mostly
// nil *server.Deps is safe because chi.Walk only enumerates registered
// routes, it never dispatches a request, so handler constructors that close
// over Deps fields never dereference them here (see NewRouter's comments).
// d.DB and d.Auth must be non-nil (zero-value, non-functional stand-ins are
// fine) because the self-service routes are conditionally registered behind
// `if d.DB != nil` / `if d.Auth != nil` checks in NewRouter.
func TestOpenAPI_SelfServiceRoutesMatchRouterMounts(t *testing.T) {
	healthy := stubHealthChecker{}
	authStore := auth.NewStore(&pgxpool.Pool{}, nil, "test-salt")
	t.Cleanup(authStore.Close)

	d := &server.Deps{
		Cfg:   &config.Config{BodyLimitBytes: 1048576, APIKeyPrefix: "cru_", DashboardOrigin: "http://localhost:3001"},
		Redis: healthy,
		PG:    healthy,
		DB:    &pgxpool.Pool{},
		Auth:  authStore,
	}
	router := server.NewRouter(d)
	chiRoutes, ok := router.(chi.Routes)
	if !ok {
		t.Fatalf("NewRouter returned %T which does not implement chi.Routes; chi.Walk unavailable", router)
	}

	mounted := make(map[string]map[string]bool, len(selfServiceRoutes))
	if err := chi.Walk(chiRoutes, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		// chi reports a Route group's own index handler (r.Get("/", ...)
		// inside r.Route("/v1/keys", ...)) with a trailing slash ("/v1/keys/"),
		// unlike named sub-paths ("/v1/webhooks/endpoints"); trim it so that
		// distinction — a chi formatting artifact, not a real path — doesn't
		// register as a false mismatch below.
		route = strings.TrimSuffix(route, "/")
		if mounted[route] == nil {
			mounted[route] = make(map[string]bool)
		}
		mounted[route][method] = true
		return nil
	}); err != nil {
		t.Fatalf("chi.Walk: %v", err)
	}

	documented := servedPaths(t)

	for _, path := range selfServiceRoutes {
		docMethods := documented[path]
		mountedMethods := mounted[path]
		for method := range docMethods {
			if !mountedMethods[strings.ToUpper(method)] {
				t.Errorf("openapi.Handler documents %s %s but server.NewRouter has no such mount", strings.ToUpper(method), path)
			}
		}
		for method := range mountedMethods {
			if _, ok := docMethods[strings.ToLower(method)]; !ok {
				t.Errorf("server.NewRouter mounts %s %s but openapi.Handler does not document it", method, path)
			}
		}
	}
}
