// routes_table.go is the per-product ADAPT edit point for /v1 invoke endpoints.
// Add one RouteDescriptor per endpoint; NewRouter and openapi.Build() both consume this table.
package server

import (
	"encoding/json"

	"github.com/Unluckyathecking/crucible/gateway/internal/openapi"
)

// V1Routes is the per-product ADAPT edit point. Add one entry per /v1 invoke endpoint.
// NewRouter mounts these routes; openapi.Build() derives the /v1/* OpenAPI Paths from
// this same slice — a single declaration is the only place /v1 paths appear.
var V1Routes = []openapi.RouteDescriptor{
	{
		Path:          "/echo",
		Operation:     "echo",
		Summary:       "Invoke echo worker operation (authenticated)",
		SampleRequest: json.RawMessage(`{"input":"hello"}`),
	},
}

// RespCacheTTLSeconds opts a /v1 route (by its RouteDescriptor.Path) into the
// framework's response-result-cache (gateway/internal/respcache): the gateway
// serves a cached worker response for identical (operation, payload) requests
// instead of re-invoking the worker. Absent, zero, or negative means the route
// is never cached — this is the framework default. A positive value is the
// per-entry TTL in seconds, clamped to config.RespCacheMaxTTLSeconds by
// NewRouter. One line per cacheable endpoint, e.g.:
//
//	var RespCacheTTLSeconds = map[string]int{
//		"/lookup": 300,
//	}
var RespCacheTTLSeconds = map[string]int{}
