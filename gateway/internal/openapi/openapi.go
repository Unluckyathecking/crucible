// Package openapi builds and serves the gateway's OpenAPI 3.1 document.
// Zero third-party dependencies: uses only encoding/json and net/http.
// The document is derived from the static route table; no DB or Redis needed.
package openapi

import (
	"encoding/json"
	"net/http"
)

// --- structural types --------------------------------------------------------

// Document is the root OpenAPI 3.1 object.
type Document struct {
	OpenAPI    string              `json:"openapi"`
	Info       Info                `json:"info"`
	Paths      map[string]PathItem `json:"paths"`
	Components Components          `json:"components"`
}

// Info holds API metadata.
type Info struct {
	Title       string `json:"title"`
	Version     string `json:"version"`
	Description string `json:"description,omitempty"`
}

// Components holds reusable schema and security objects.
type Components struct {
	Schemas         map[string]*Schema         `json:"schemas,omitempty"`
	SecuritySchemes map[string]*SecurityScheme `json:"securitySchemes,omitempty"`
}

// SecurityScheme describes an authentication method.
type SecurityScheme struct {
	Type        string `json:"type"`
	In          string `json:"in,omitempty"`
	Name        string `json:"name,omitempty"`
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

// Operation describes a single HTTP operation.
type Operation struct {
	OperationID string               `json:"operationId,omitempty"`
	Summary     string               `json:"summary,omitempty"`
	Tags        []string             `json:"tags,omitempty"`
	Security    []SecurityRequirement `json:"security,omitempty"`
	RequestBody *RequestBody         `json:"requestBody,omitempty"`
	Responses   map[string]Response  `json:"responses"`
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

// Response describes a single HTTP response.
type Response struct {
	Description string               `json:"description,omitempty"`
	Content     map[string]MediaType `json:"content,omitempty"`
}

// --- constants ---------------------------------------------------------------

const (
	errorSchemaRef = "#/components/schemas/Error"
	apiKeyScheme   = "ApiKeyAuth"
)

// --- builder -----------------------------------------------------------------

// Build constructs and returns the gateway's OpenAPI 3.1 document.
// It is a pure function with no I/O; safe to call multiple times.
func Build() Document {
	errSchema := &Schema{Ref: errorSchemaRef}
	errResp := func(desc string) Response {
		return Response{
			Description: desc,
			Content:     map[string]MediaType{"application/json": {Schema: errSchema}},
		}
	}

	return Document{
		OpenAPI: "3.1.0",
		Info: Info{
			Title:       "Crucible Gateway",
			Version:     "1.0.0",
			Description: "Clone-and-adapt framework for high-volume metered API products.",
		},
		Components: Components{
			SecuritySchemes: map[string]*SecurityScheme{
				apiKeyScheme: {
					Type:        "apiKey",
					In:          "header",
					Name:        "Authorization",
					Description: "Bearer token — value format: Bearer <api-key>",
				},
			},
			Schemas: map[string]*Schema{
				"Error": {
					Type: "object",
					Properties: map[string]*Schema{
						"error": {
							Type: "object",
							Properties: map[string]*Schema{
								"code":      {Type: "string"},
								"message":   {Type: "string"},
								"retryable": {Type: "boolean"},
							},
							Required: []string{"code", "message"}, // retryable absent in auth-layer errors (see auth/middleware.go)
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
								"application/json": {
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
								"application/json": {
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
			"/webhooks/stripe": {
				Post: &Operation{
					OperationID: "stripe_webhook",
					Summary:     "Stripe billing webhook receiver",
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
					RequestBody: &RequestBody{
						Required: true,
						Content: map[string]MediaType{
							"application/json": {Schema: &Schema{Type: "object"}},
						},
					},
					Responses: map[string]Response{
						"200": {
							Description: "Successful invocation",
							Content: map[string]MediaType{
								"application/json": {Schema: &Schema{Type: "object"}},
							},
						},
						"400": errResp("Bad request — invalid JSON body"),
						"401": errResp("Unauthorized — missing or invalid API key"),
						"429": errResp("Rate limited"),
						"502": errResp("Worker unavailable"),
					},
				},
			},
		},
	}
}

// --- handler -----------------------------------------------------------------

var documentJSON = func() []byte {
	b, err := json.Marshal(Build())
	if err != nil {
		panic("openapi: failed to marshal document: " + err.Error())
	}
	return b
}()

// Handler returns an http.HandlerFunc that serves the static OpenAPI document.
// The response is pre-computed at init time; no DB or Redis access is performed.
func Handler() http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(documentJSON)
	}
}
