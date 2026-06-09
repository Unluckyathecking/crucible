// Package openapi builds and serves the gateway's OpenAPI 3.1 document.
// Zero third-party dependencies: uses only encoding/json, net/http, and sync.
// The document is derived from the static route table; no DB or Redis needed.
package openapi

import (
	"encoding/json"
	"log"
	"net/http"
	"sync"
)

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
type Schema struct {
	Ref                  string             `json:"$ref,omitempty"`
	Type                 string             `json:"type,omitempty"`
	Properties           map[string]*Schema `json:"properties,omitempty"`
	Required             []string           `json:"required,omitempty"`
	AdditionalProperties *Schema            `json:"additionalProperties,omitempty"`
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

// Build constructs and returns the gateway's OpenAPI 3.1 document.
// It is a pure function with no I/O; safe to call multiple times.
func Build() Document {
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
								"request_id": {Type: "string"},
							},
							Required: []string{"code", "message", "retryable"},
						},
					},
					Required: []string{"error"},
				},
			},
		},
		Paths: map[string]PathItem{
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
			"/v1/echo": {
				Post: &Operation{
					OperationID: "invoke_echo",
					Summary:     "Invoke echo worker operation (authenticated)",
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
							contentTypeJSON: {Schema: &Schema{Type: "object"}},
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
				},
			},
		},
	}
}

// --- handler -----------------------------------------------------------------

var (
	documentJSON []byte
	buildOnce    sync.Once
)

func buildDocument() {
	b, err := json.Marshal(Build())
	if err != nil {
		panic("openapi: failed to marshal static document: " + err.Error())
	}
	documentJSON = b
}

// Handler returns an http.HandlerFunc that serves the static OpenAPI document.
// The document is built lazily on first request via sync.Once; no DB or Redis access.
func Handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		buildOnce.Do(buildDocument)
		w.Header().Set("Content-Type", contentTypeJSON)
		if _, err := w.Write(documentJSON); err != nil {
			log.Printf("openapi: write: %v", err)
		}
	}
}
