// Package openapi builds and serves the gateway's OpenAPI 3.1 document.
// Zero third-party dependencies: uses only the Go standard library.
// The document is derived from the route descriptor table; no DB or Redis needed.
package openapi

import (
	"encoding/json"
	"log"
	"net/http"
	"strings"
)

// RouteDescriptor describes a single per-product /v1 invoke endpoint.
// It is the sole authoritative source of which /v1 paths exist and which
// opaque worker operation string each maps to.
type RouteDescriptor struct {
	Path          string  // path segment after /v1, e.g. "/echo"
	Operation     string  // opaque string forwarded to the worker
	Summary       string  // human-readable; appears in the OpenAPI document
	RequestSchema *Schema // optional; when set, gateway validates request body against this schema
}

// --- structural types --------------------------------------------------------

// Document is the root OpenAPI 3.1 object.
type Document struct {
	OpenAPI    string              `json:"openapi"`
	Info       Info                `json:"info"`
	Servers    []Server            `json:"servers,omitempty"`
	Paths      map[string]PathItem `json:"paths"`
	Components Components          `json:"components"`
}

// Info holds API metadata.
type Info struct {
	Title       string `json:"title"`
	Version     string `json:"version"`
	Description string `json:"description,omitempty"`
}

// Server represents an API server entry.
type Server struct {
	URL         string `json:"url"`
	Description string `json:"description,omitempty"`
}

// Components holds reusable schema and security objects.
type Components struct {
	Schemas         map[string]*Schema         `json:"schemas,omitempty"`
	SecuritySchemes map[string]*SecurityScheme `json:"securitySchemes,omitempty"`
}

// SecurityScheme describes an authentication method.
// Use Type="apiKey", In="header", Name=<header> for API key header auth.
// Use Type="http", Scheme="bearer" for RFC 6750 Bearer token auth.
type SecurityScheme struct {
	Type        string `json:"type"`
	Scheme      string `json:"scheme,omitempty"`  // for http type
	In          string `json:"in,omitempty"`       // for apiKey type
	Name        string `json:"name,omitempty"`     // for apiKey type
	Description string `json:"description,omitempty"`
}

// Schema represents a JSON Schema node or a $ref pointer.
// When Ref is non-empty, other fields should be zero (omitempty suppresses them).
//
// BoolFalse: when true this Schema marshals as the JSON boolean false rather
// than an object. Set AdditionalProperties: &Schema{BoolFalse: true} to
// express additionalProperties:false in the OpenAPI document and in the
// gateway's request-body validator.
type Schema struct {
	Ref                  string             `json:"$ref,omitempty"`
	Type                 string             `json:"type,omitempty"`
	Description          string             `json:"description,omitempty"`
	Properties           map[string]*Schema `json:"properties,omitempty"`
	Required             []string           `json:"required,omitempty"`
	AdditionalProperties *Schema            `json:"additionalProperties,omitempty"`
	// BoolFalse causes MarshalJSON to emit the JSON boolean false.
	// Used only as AdditionalProperties: &Schema{BoolFalse: true}.
	BoolFalse bool `json:"-"`
	// JSON Schema validation constraints (subset supported by internal/validate).
	Enum      []any    `json:"enum,omitempty"`
	MinLength *int     `json:"minLength,omitempty"`
	MaxLength *int     `json:"maxLength,omitempty"`
	Pattern   string   `json:"pattern,omitempty"`
	Minimum   *float64 `json:"minimum,omitempty"`
	Maximum   *float64 `json:"maximum,omitempty"`
}

// MarshalJSON implements json.Marshaler.
// When BoolFalse is true the schema marshals as the JSON boolean false,
// allowing AdditionalProperties: &Schema{BoolFalse: true} to produce
// "additionalProperties": false in the OpenAPI document.
func (s Schema) MarshalJSON() ([]byte, error) {
	if s.BoolFalse {
		return []byte("false"), nil
	}
	// schemaAlias has the same fields but no MarshalJSON, so encoding/json uses
	// standard struct marshaling. *Schema fields within Properties and
	// AdditionalProperties still call Schema.MarshalJSON recursively.
	type schemaAlias Schema
	return json.Marshal(schemaAlias(s))
}

// PathItem holds the operations for a single URL path.
type PathItem struct {
	Get  *Operation `json:"get,omitempty"`
	Post *Operation `json:"post,omitempty"`
}

// Parameter describes a single operation parameter (header, query, path, or cookie).
type Parameter struct {
	Name        string  `json:"name"`
	In          string  `json:"in"`
	Description string  `json:"description,omitempty"`
	Required    bool    `json:"required,omitempty"`
	Schema      *Schema `json:"schema,omitempty"`
}

// Operation describes a single HTTP operation.
type Operation struct {
	OperationID string                `json:"operationId,omitempty"`
	Summary     string                `json:"summary,omitempty"`
	Tags        []string              `json:"tags,omitempty"`
	Security    []SecurityRequirement `json:"security,omitempty"`
	Parameters  []Parameter           `json:"parameters,omitempty"`
	RequestBody *RequestBody          `json:"requestBody,omitempty"`
	Responses   map[string]Response   `json:"responses"`
}

// SecurityRequirement maps a scheme name to its required scopes (empty slice = no scopes).
type SecurityRequirement map[string][]string

// RequestBody describes the request payload.
type RequestBody struct {
	Required bool                 `json:"required,omitempty"`
	Content  map[string]MediaType `json:"content"`
}

// MediaType pairs a schema with a MIME type.
type MediaType struct {
	Schema *Schema `json:"schema,omitempty"`
}

// Header describes a single response header.
type Header struct {
	Description string  `json:"description,omitempty"`
	Schema      *Schema `json:"schema,omitempty"`
}

// Response describes a single HTTP response.
type Response struct {
	Description string               `json:"description,omitempty"`
	Headers     map[string]Header    `json:"headers,omitempty"`
	Content     map[string]MediaType `json:"content,omitempty"`
}

// --- constants ---------------------------------------------------------------

const (
	errorSchemaRef  = "#/components/schemas/Error"
	apiKeyScheme    = "ApiKeyAuth"
	contentTypeJSON = "application/json"
)

// --- builder -----------------------------------------------------------------

// intHeader is a shorthand for a response header with an integer schema.
func intHeader(desc string) Header {
	return Header{Description: desc, Schema: &Schema{Type: "integer"}}
}

// rateLimitHeaders returns the six RateLimit-*/X-RateLimit-* headers that are
// set on every admitted or rate-limited response (limited plans only; unlimited
// plans omit them at runtime). These are the headers guaranteed on every 429.
func rateLimitHeaders() map[string]Header {
	return map[string]Header{
		"RateLimit-Limit":       intHeader("Per-minute request cap for the customer's plan"),
		"RateLimit-Remaining":   intHeader("Requests remaining in the current sliding window"),
		"RateLimit-Reset":       intHeader("Unix timestamp when the sliding window fully resets"),
		"X-RateLimit-Limit":     intHeader("Alias for RateLimit-Limit"),
		"X-RateLimit-Remaining": intHeader("Alias for RateLimit-Remaining"),
		"X-RateLimit-Reset":     intHeader("Alias for RateLimit-Reset"),
	}
}

// rateLimitAndQuotaHeaders returns all nine observability headers. Used on 200
// responses where both rate-limit and quota middleware have run. X-Quota-*
// headers are omitted at runtime for unlimited plans but documented here for
// completeness.
func rateLimitAndQuotaHeaders() map[string]Header {
	h := rateLimitHeaders()
	h["X-Quota-Limit"] = intHeader("Monthly billable-unit cap for the customer's plan")
	h["X-Quota-Remaining"] = intHeader("Billable units remaining in the current calendar month")
	h["X-Quota-Reset"] = intHeader("Unix timestamp when the monthly quota resets (UTC month-end)")
	return h
}

// errResp returns a Response whose content schema is a $ref to the Error component.
func errResp(desc string) Response {
	return Response{
		Description: desc,
		Content: map[string]MediaType{
			contentTypeJSON: {Schema: &Schema{Ref: errorSchemaRef}},
		},
	}
}

// invokeOperation builds the standard Operation for a per-product /v1 invoke endpoint.
// schema is the route's RequestSchema; when nil the request body is documented as a
// generic object ({"type":"object"}) preserving backwards-compatibility.
func invokeOperation(operationID, summary string, schema *Schema) *Operation {
	reqBodySchema := &Schema{Type: "object"}
	if schema != nil {
		reqBodySchema = schema
	}
	return &Operation{
		OperationID: operationID,
		Summary:     summary,
		Tags:        []string{"invoke"},
		Security:    []SecurityRequirement{{apiKeyScheme: []string{}}},
		Parameters: []Parameter{
			{
				Name:        "Idempotency-Key",
				In:          "header",
				Description: "Optional deduplication key (max 255 chars). Identical-key retries within 24 h replay the stored response without re-invoking the worker or re-billing.",
				Schema:      &Schema{Type: "string"},
			},
		},
		RequestBody: &RequestBody{
			Required: true,
			Content: map[string]MediaType{
				contentTypeJSON: {Schema: reqBodySchema},
			},
		},
		Responses: map[string]Response{
			"200": {
				Description: "Successful invocation",
				Headers:     rateLimitAndQuotaHeaders(),
				Content: map[string]MediaType{
					contentTypeJSON: {Schema: &Schema{Type: "object"}},
				},
			},
			"400": errResp("Bad request — invalid JSON body"),
			"401": errResp("Unauthorized — missing or invalid API key"),
			"409": errResp("Idempotency conflict — concurrent request with same key"),
			"422": errResp("Idempotency key reused with a different request body"),
			"429": {
				Description: "Rate limited or quota exceeded. " +
					"RateLimit-*/X-RateLimit-* headers are always present. " +
					"X-Quota-* headers are present only on quota-triggered 429s.",
				Headers: rateLimitAndQuotaHeaders(),
				Content: map[string]MediaType{
					contentTypeJSON: {Schema: &Schema{Ref: errorSchemaRef}},
				},
			},
			"500": errResp("Internal server error"),
			"502": errResp("Worker unavailable"),
		},
	}
}

// OperationIDFromPath derives the OpenAPI operationId for a per-product /v1 invoke route.
// Hyphens are replaced with _ and path separators (/) are replaced with __ (double underscore)
// so the result is a valid Go/TS identifier and multi-segment paths cannot collide with
// hyphenated single-segment paths (e.g., /a-b → invoke_a_b, /a/b → invoke_a__b).
// gen-clients.sh uses _ as a split boundary to produce CamelCase SDK method names.
func OperationIDFromPath(path string) string {
	if len(path) < 2 || path[0] != '/' {
		panic("openapi: OperationIDFromPath: path must start with / and have at least one segment: " + path)
	}
	if path[len(path)-1] == '/' {
		panic("openapi: OperationIDFromPath: path must not end with /: " + path)
	}
	s := path[1:]
	s = strings.ReplaceAll(s, "/", "__")
	s = strings.ReplaceAll(s, "-", "_")
	return "invoke_" + s
}

// validateRouteDescriptor panics on invalid RouteDescriptor fields.
// Panics surface at program startup (route registration), not at HTTP request time.
func validateRouteDescriptor(rt RouteDescriptor) {
	if rt.Path == "" || rt.Path[0] != '/' {
		panic("openapi: RouteDescriptor.Path must start with /: " + rt.Path)
	}
	if rt.Path == "/" {
		// "/" is the only 1-character path that passes the rt.Path[0]=='/' check above.
		// It would mount at /v1, colliding with the /v1 route group itself.
		panic("openapi: RouteDescriptor.Path must have at least one segment after /: " + rt.Path)
	}
	if rt.Path[len(rt.Path)-1] == '/' {
		panic("openapi: RouteDescriptor.Path must not end with /: " + rt.Path)
	}
	for _, seg := range strings.Split(rt.Path[1:], "/") {
		if seg == "" {
			panic("openapi: RouteDescriptor.Path must not contain empty segments: " + rt.Path)
		}
	}
	if strings.Contains(rt.Path, "_") {
		// OperationIDFromPath escapes - as _ and / as __.
		// A literal _ in the path would collide with a hyphen escape (e.g., /a_b and /a-b
		// both map to invoke_a_b), breaking SDK codegen. Use - for word separation.
		panic("openapi: RouteDescriptor.Path must not contain underscore (use - for word separation): " + rt.Path)
	}
	if rt.Operation == "" {
		panic("openapi: RouteDescriptor.Operation must not be empty for path: " + rt.Path)
	}
	if rt.Summary == "" {
		panic("openapi: RouteDescriptor.Summary must not be empty for path: " + rt.Path)
	}
}

// Build constructs the gateway's OpenAPI 3.1 document from the given invoke route descriptors.
// It is a pure function with no I/O; safe to call multiple times.
func Build(invokeRoutes []RouteDescriptor) Document {
	paths := map[string]PathItem{
		"/healthz": {
			Get: &Operation{
				OperationID: "healthz",
				Summary:     "Liveness check",
				Tags:        []string{"system"},
				Responses: map[string]Response{
					"200": {
						Description: "Service is alive",
						Content: map[string]MediaType{
							contentTypeJSON: {
								Schema: &Schema{
									Type: "object",
									Properties: map[string]*Schema{
										"status": {Type: "string"},
									},
								},
							},
						},
					},
				},
			},
		},
		"/readyz": {
			Get: &Operation{
				OperationID: "readyz",
				Summary:     "Readiness check — reports dependency health",
				Tags:        []string{"system"},
				Responses: map[string]Response{
					"200": {
						Description: "Dependency health report",
						Content: map[string]MediaType{
							contentTypeJSON: {
								Schema: &Schema{
									Type: "object",
									Properties: map[string]*Schema{
										"status": {Type: "string"},
										"checks": {
											Type:                 "object",
											AdditionalProperties: &Schema{Type: "string"},
										},
									},
								},
							},
						},
					},
				},
			},
		},
		"/metrics": {
			Get: &Operation{
				OperationID: "metrics",
				Summary:     "Prometheus metrics",
				Tags:        []string{"system"},
				Responses: map[string]Response{
					"200": {
						Description: "Prometheus exposition format",
						Content: map[string]MediaType{
							"text/plain": {Schema: &Schema{Type: "string"}},
						},
					},
				},
			},
		},
		"/webhooks/stripe": {
			Post: &Operation{
				OperationID: "stripe_webhook",
				Summary:     "Stripe billing webhook receiver (unauthenticated)",
				Tags:        []string{"billing"},
				Responses: map[string]Response{
					"200": {Description: "Webhook processed"},
				},
			},
		},
	}

	seenOpIDs := make(map[string]string, len(invokeRoutes)) // opID → first path that produced it
	for _, rt := range invokeRoutes {
		validateRouteDescriptor(rt)
		key := "/v1" + rt.Path
		if _, exists := paths[key]; exists {
			panic("openapi: RouteDescriptor.Path collides with existing route at: " + key)
		}
		opID := OperationIDFromPath(rt.Path)
		if firstPath, collision := seenOpIDs[opID]; collision {
			panic("openapi: paths " + firstPath + " and " + key + " produce duplicate operationId " + opID + " — rename one path")
		}
		seenOpIDs[opID] = key
		paths[key] = PathItem{Post: invokeOperation(opID, rt.Summary, rt.RequestSchema)}
	}

	return Document{
		OpenAPI: "3.1.0",
		Info: Info{
			Title:       "Crucible Gateway",
			Version:     "1.0.0",
			Description: "Clone-and-adapt framework for high-volume metered API products.",
		},
		Servers: []Server{
			{URL: "https://api.example.com", Description: "Replace with your deployment URL"},
		},
		Components: Components{
			SecuritySchemes: map[string]*SecurityScheme{
				apiKeyScheme: {
					Type:        "apiKey",
					In:          "header",
					Name:        "X-API-Key",
					Description: "API key for authenticating requests to protected endpoints",
				},
			},
			Schemas: map[string]*Schema{
				"Error": {
					Type: "object",
					Properties: map[string]*Schema{
						"error": {
							Type: "object",
							Properties: map[string]*Schema{
								"code":       {Type: "string"},
								"message":    {Type: "string"},
								"retryable":  {Type: "boolean"},
								"request_id": {Type: "string", Description: "X-Request-ID value echoed from the request; always present in error responses (value may be empty string if no request ID was generated). Use for support correlation."},
							},
							Required: []string{"code", "message", "retryable", "request_id"},
						},
					},
					Required: []string{"error"},
				},
			},
		},
		Paths: paths,
	}
}

// --- handler -----------------------------------------------------------------

// Handler returns an http.HandlerFunc that serves the OpenAPI document built from invokeRoutes.
// The document is built eagerly at construction time so invalid descriptors panic at server
// startup rather than on the first request. No DB or Redis access.
func Handler(invokeRoutes []RouteDescriptor) http.HandlerFunc {
	// Defensive copy: the caller may mutate elements of the original slice after
	// Handler returns. make+copy gives the closure its own backing array, isolating
	// it from such mutations.
	routes := make([]RouteDescriptor, len(invokeRoutes))
	copy(routes, invokeRoutes)
	b, err := json.Marshal(Build(routes))
	if err != nil {
		panic("openapi: failed to marshal static document: " + err.Error())
	}
	doc := b
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", contentTypeJSON)
		if _, err := w.Write(doc); err != nil {
			log.Printf("openapi: write: %v", err)
		}
	}
}
