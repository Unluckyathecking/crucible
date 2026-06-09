// routes_table.go is the per-product ADAPT edit point for /v1 invoke endpoints.
// Add one RouteDescriptor per endpoint; NewRouter and openapi.Build() both consume this table.
package server

import "github.com/Unluckyathecking/crucible/gateway/internal/openapi"

// V1Routes is the per-product ADAPT edit point. Add one entry per /v1 invoke endpoint.
// NewRouter mounts these routes; openapi.Build() derives the /v1/* OpenAPI Paths from
// this same slice — a single declaration is the only place /v1 paths appear.
var V1Routes = []openapi.RouteDescriptor{
	{Path: "/echo", Operation: "echo", Summary: "Invoke echo worker operation (authenticated)"},
}
