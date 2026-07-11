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

// AsyncRoutes opts a /v1 route (by its RouteDescriptor.Path) into durable
// async execution (gateway/internal/jobs): POST /v1/<path> enqueues a
// Postgres-backed job and returns 202 {job_id} instead of invoking the
// worker inline; the caller polls GET /v1/jobs/{id} for the result. Absent
// means the route stays synchronous — this is the framework default, so
// existing clones are byte-unaffected until they opt a route in. The value
// is an optional per-route job timeout in seconds; 0 or negative means "use
// the gateway's default JOB_TIMEOUT_MS". Use this for products whose worker
// calls exceed WORKER_TIMEOUT_MS's synchronous budget (OCR, transcription,
// PDF/render pipelines, scraping, LLM/inference). One line per async
// endpoint, e.g.:
//
//	var AsyncRoutes = map[string]int{
//		"/transcribe": 0,
//	}
var AsyncRoutes = map[string]int{}
