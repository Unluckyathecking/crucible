package openapi

import (
	"encoding/json"
)

// RouteDescriptor describes a single per-product /v1 invoke endpoint.
// It is the sole authoritative source of which /v1 paths exist and which
// opaque worker operation string each maps to.
type RouteDescriptor struct {
	Path          string  // path segment after /v1, e.g. "/echo"
	Operation     string  // opaque string forwarded to the worker
	Summary       string  // human-readable; appears in the OpenAPI document
	RequestSchema *Schema // optional; when set, gateway validates request body against this schema
	// SampleRequest is an optional example request body (raw JSON) for this route.
	// When set it drives three things from one declaration: the "example" value
	// on the request body's MediaType in the served /openapi.json (see
	// invokeOperation), the request payload gateway/test/acceptance sends instead
	// of a hardcoded {}, and a startup cross-check (server.NewRouter, next to
	// validate.CompileSchemaPatterns) that log.Fatals if the sample does not
	// validate against RequestSchema — catching a drifted example before it ships
	// in the OpenAPI document or silently no-ops the acceptance test. Nil means
	// no example is declared; consumers fall back to an empty JSON object.
	SampleRequest json.RawMessage
	// ResponseSchema is an optional schema for the worker's "payload" field
	// within the frozen /v1 invoke success envelope (proxy.InvokeResponse:
	// {payload, billable_units, units_label}). Symmetric complement to
	// RequestSchema: when set, invokeOperation documents the 200 body as
	// that envelope typed around this schema, which scripts/gen-clients.sh
	// reads to emit a named *Response struct instead of its untyped
	// map[string]any fallback. Nil means the 200 body stays the current
	// generic {"type":"object"} — backward-compatible with routes declared
	// before this field existed.
	ResponseSchema *Schema
	// Async documents that this route has been opted into
	// routes_table.go's AsyncRoutes: the runtime contract for POST
	// /v1/<path> is 202 {job_id} (poll GET /v1/jobs/{id} for the result),
	// not the synchronous invoke envelope. server.NewRouter sets this on
	// its route snapshot (never on the shared V1Routes slice) before
	// building the served document, so SDKs/clients generated from
	// /openapi.json describe the actual response shape instead of a stale
	// synchronous one. False (the default) is the existing synchronous
	// documentation — zero-config-safe for routes that never opt in.
	Async bool
}

// --- structural types --------------------------------------------------------

// Document is the root OpenAPI 3.1 object.
type Document struct {
	OpenAPI string              `json:"openapi"`
	Info    Info                `json:"info"`
	Servers []Server            `json:"servers,omitempty"`
	Paths   map[string]PathItem `json:"paths"`
	// Webhooks documents the outbound-webhook event catalogue (OpenAPI 3.1's
	// top-level `webhooks` field): one entry per gateway/internal/events event
	// type, describing the payload the gateway POSTs to a registered endpoint.
	Webhooks   map[string]PathItem `json:"webhooks,omitempty"`
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
	Scheme      string `json:"scheme,omitempty"` // for http type
	In          string `json:"in,omitempty"`     // for apiKey type
	Name        string `json:"name,omitempty"`   // for apiKey type
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
	// Items describes the element schema of a Type:"array" node — the
	// standards-correct way to type a list's rows (as opposed to the older
	// idiom elsewhere in this file of attaching Properties directly to the
	// array schema itself, which off-the-shelf OpenAPI tooling doesn't
	// recognize as describing the array's elements).
	// scripts/gen-clients.sh's array_item_properties reads either shape, so
	// using Items here doesn't change the generated Go/TS client output.
	Items                *Schema `json:"items,omitempty"`
	AdditionalProperties *Schema `json:"additionalProperties,omitempty"`
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
	// schemaAlias prevents infinite recursion at the top level: json.Marshal of a
	// schemaAlias value uses standard struct marshaling (no MarshalJSON on the alias).
	// Nested *Schema fields (Properties, AdditionalProperties) are still marshaled
	// via Schema.MarshalJSON because *Schema retains the method; recursion is bounded
	// by finite schema depth and terminates at leaves or BoolFalse:true nodes.
	type schemaAlias Schema
	return json.Marshal(schemaAlias(s))
}

// PathItem holds the operations for a single URL path.
type PathItem struct {
	Get    *Operation `json:"get,omitempty"`
	Post   *Operation `json:"post,omitempty"`
	Patch  *Operation `json:"patch,omitempty"`
	Delete *Operation `json:"delete,omitempty"`
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
	Description string                `json:"description,omitempty"`
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
	// Example is an optional literal example value for this media type,
	// e.g. a route's RouteDescriptor.SampleRequest. json.RawMessage marshals
	// verbatim so the example appears as real JSON, not a quoted string.
	Example json.RawMessage `json:"example,omitempty"`
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
