// Package openapi builds and serves the gateway's OpenAPI 3.1 document.
// Zero third-party dependencies: uses only the Go standard library.
// The document is derived from the route descriptor table; no DB or Redis needed.
package openapi

// --- constants ---------------------------------------------------------------

const (
	errorSchemaRef  = "#/components/schemas/Error"
	apiKeyScheme    = "ApiKeyAuth"
	contentTypeJSON = "application/json"
)
