// Package server wires the HTTP router and per-route handlers for the Crucible gateway.
// V1Routes (this file) is the single ADAPT edit point for per-product /v1 invoke endpoints.
package server

import "github.com/Unluckyathecking/crucible/gateway/internal/openapi"

// V1Routes is the per-product ADAPT edit point. Add one entry per /v1 invoke endpoint.
// NewRouter mounts these routes; openapi.Build() derives the /v1/* OpenAPI Paths from
// this same slice — a single declaration is the only place /v1 paths appear.
var V1Routes = []openapi.RouteDescriptor{
	{Path: "/echo", Operation: "echo", Summary: "Invoke echo worker operation (authenticated)"},
}
