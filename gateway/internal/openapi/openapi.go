// Package openapi builds and serves the gateway's OpenAPI 3.1 document.
// Zero third-party dependencies: uses only the Go standard library.
// The document is derived from the route descriptor table; no DB or Redis needed.
package openapi

import (
	"encoding/json"
	"log"
	"net/http"
	"strings"

	"github.com/Unluckyathecking/crucible/gateway/internal/events"
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

// webhookSubscriptionNote documents the per-endpoint subscription behavior added
// in 0017_webhook_subscriptions.sql: a registered endpoint only receives event
// types it is subscribed to (webhook_endpoints.subscribed_events), with a
// backward-compatible default of "all events" for endpoints that never set an
// explicit subscription.
const webhookSubscriptionNote = "Delivered only to endpoints subscribed to this event type. " +
	"An endpoint with no explicit subscribed_events value receives every event type (default)."

// webhookEventDescriptors documents the payload schema for each outbound webhook
// event type. Kept manually in sync with gateway/internal/events.AllEventTypes;
// buildWebhooks panics at Build() time (caught by TestBuild_WebhookEventCatalogueLocked)
// if this list and AllEventTypes ever fall out of sync, so a rename or an addition
// to the events package without updating this file fails loudly instead of silently
// shipping an incomplete spec.
var webhookEventDescriptors = []struct {
	eventType string
	summary   string
	schema    *Schema
}{
	{
		eventType: events.SubscriptionUpdated,
		summary:   "Fired when a customer's Stripe subscription is created or updated (including plan changes).",
		schema: &Schema{
			Type: "object",
			Properties: map[string]*Schema{
				"customer_id": {Type: "string", Description: "Internal customer UUID"},
				"plan_id":     {Type: "string", Description: "The plan the customer is now on"},
			},
			Required: []string{"customer_id", "plan_id"},
		},
	},
	{
		eventType: events.SubscriptionDeleted,
		summary:   "Fired when a customer's Stripe subscription is canceled; plan_id reverts to free.",
		schema: &Schema{
			Type: "object",
			Properties: map[string]*Schema{
				"customer_id": {Type: "string", Description: "Internal customer UUID"},
				"plan_id":     {Type: "string", Description: "Always \"free\" for this event"},
			},
			Required: []string{"customer_id", "plan_id"},
		},
	},
	{
		eventType: events.QuotaExceeded,
		summary:   "Fired when a request is rejected because the customer's monthly billable-unit quota is exhausted.",
		schema: &Schema{
			Type: "object",
			Properties: map[string]*Schema{
				"customer_id": {Type: "string", Description: "Internal customer UUID"},
				"plan":        {Type: "string", Description: "The customer's plan id at the time of rejection"},
				"cap":         {Type: "integer", Description: "The plan's monthly billable-unit cap"},
			},
			Required: []string{"customer_id", "plan", "cap"},
		},
	},
	{
		eventType: events.APIKeyRotated,
		summary:   "Fired when a customer rotates an API key; the old key enters its grace-period expiry.",
		schema: &Schema{
			Type: "object",
			Properties: map[string]*Schema{
				"customer_id": {Type: "string", Description: "Internal customer UUID"},
				"old_key_id":  {Type: "string", Description: "The rotated-out key's id"},
				"new_key_id":  {Type: "string", Description: "The newly issued key's id"},
			},
			Required: []string{"customer_id", "old_key_id", "new_key_id"},
		},
	},
	{
		eventType: events.APIKeyRevoked,
		summary:   "Fired when a customer's API key is revoked.",
		schema: &Schema{
			Type: "object",
			Properties: map[string]*Schema{
				"customer_id": {Type: "string", Description: "Internal customer UUID"},
				"key_id":      {Type: "string", Description: "The revoked key's id"},
			},
			Required: []string{"customer_id", "key_id"},
		},
	},
}

// buildWebhooks constructs the `webhooks` document section from webhookEventDescriptors.
// It panics if the descriptor list and events.AllEventTypes disagree on the event-type
// set, so a drift between the two is caught at Build() time rather than shipping an
// incomplete or stale spec.
func buildWebhooks() map[string]PathItem {
	webhooks := make(map[string]PathItem, len(webhookEventDescriptors))
	for _, wd := range webhookEventDescriptors {
		if _, exists := webhooks[wd.eventType]; exists {
			panic("openapi: duplicate webhook event type in webhookEventDescriptors: " + wd.eventType)
		}
		webhooks[wd.eventType] = PathItem{
			Post: &Operation{
				OperationID: "webhook_" + strings.ReplaceAll(strings.ReplaceAll(wd.eventType, ".", "_"), "-", "_"),
				Summary:     wd.summary,
				Description: webhookSubscriptionNote,
				Tags:        []string{"webhooks"},
				RequestBody: &RequestBody{
					Required: true,
					Content: map[string]MediaType{
						contentTypeJSON: {Schema: wd.schema},
					},
				},
				Responses: map[string]Response{
					"200": {Description: "The registered endpoint acknowledged the webhook"},
				},
			},
		}
	}
	if len(webhooks) != len(events.AllEventTypes) {
		panic("openapi: webhookEventDescriptors has drifted from events.AllEventTypes — update both together")
	}
	for _, et := range events.AllEventTypes {
		if _, ok := webhooks[et]; !ok {
			panic("openapi: events.AllEventTypes contains " + et + " but webhookEventDescriptors does not document it")
		}
	}
	return webhooks
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
		Webhooks: buildWebhooks(),
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
	doc := Build(routes)
	// GET /v1/usage is framework infra (see routes.go), not a per-product invoke
	// route, so it is layered onto the served document here rather than added to
	// Build()'s static paths: Build() is also called directly by
	// server.TestV1RoutesDriftGuard, which asserts every /v1/* path it produces
	// (other than /v1/billing/*) is POST-only — the invariant that keeps
	// per-product routes and their documentation from drifting apart. Merging
	// here documents the endpoint in the actual served /openapi.json without
	// perturbing that invoke-route-only guarantee.
	//
	// Set only Get on whatever PathItem is already there rather than replacing
	// it outright: a product clone could name an invoke route "/usage" (POST
	// /v1/usage, routed independently through the per-product V1Routes block —
	// chi dispatches by method, so it coexists fine with this GET at runtime),
	// and a wholesale overwrite here would silently drop that POST operation
	// from the served document.
	usageItem := doc.Paths["/v1/usage"]
	usageItem.Get = usagePathItem().Get
	doc.Paths["/v1/usage"] = usageItem

	// Layer the webhook-endpoint management routes on for the same reason and
	// in the same manner as /v1/usage above: framework infra, not per-product
	// invoke routes, so they must stay out of Build()'s invoke-route-only paths
	// (see server.TestV1RoutesDriftGuard) but should still appear in the
	// document actually served to API consumers.
	for path, item := range webhookEndpointsPathItems() {
		existing := doc.Paths[path]
		if item.Get != nil {
			existing.Get = item.Get
		}
		if item.Post != nil {
			existing.Post = item.Post
		}
		if item.Patch != nil {
			existing.Patch = item.Patch
		}
		if item.Delete != nil {
			existing.Delete = item.Delete
		}
		doc.Paths[path] = existing
	}

	// Layer the customer API-key self-management routes on for the same
	// reason as webhookEndpointsPathItems above.
	for path, item := range keysPathItems() {
		existing := doc.Paths[path]
		if item.Get != nil {
			existing.Get = item.Get
		}
		if item.Post != nil {
			existing.Post = item.Post
		}
		if item.Patch != nil {
			existing.Patch = item.Patch
		}
		if item.Delete != nil {
			existing.Delete = item.Delete
		}
		doc.Paths[path] = existing
	}

	// Layer the customer error-history self-service route on for the same
	// reason as usagePathItem/keysPathItems above.
	for path, item := range errorsPathItems() {
		existing := doc.Paths[path]
		if item.Get != nil {
			existing.Get = item.Get
		}
		if item.Post != nil {
			existing.Post = item.Post
		}
		if item.Patch != nil {
			existing.Patch = item.Patch
		}
		if item.Delete != nil {
			existing.Delete = item.Delete
		}
		doc.Paths[path] = existing
	}

	b, err := json.Marshal(doc)
	if err != nil {
		panic("openapi: failed to marshal static document: " + err.Error())
	}
	body := b
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", contentTypeJSON)
		if _, err := w.Write(body); err != nil {
			log.Printf("openapi: write: %v", err)
		}
	}
}

// usagePathItem documents GET /v1/usage: the authenticated customer's
// current billing-period usage against their plan quota (see
// gateway/internal/selfusage). Framework infra, not a per-product invoke
// route — kept out of Build()'s invoke-route paths; see Handler.
func usagePathItem() PathItem {
	return PathItem{
		Get: &Operation{
			OperationID: "get_usage",
			Summary:     "Get the authenticated customer's current billing-period usage against their plan quota",
			Tags:        []string{"usage"},
			Security:    []SecurityRequirement{{apiKeyScheme: []string{}}},
			Responses: map[string]Response{
				"200": {
					Description: "Current-period usage — same signals the quota middleware enforces against",
					Content: map[string]MediaType{
						contentTypeJSON: {Schema: &Schema{
							Type: "object",
							Properties: map[string]*Schema{
								"plan_id":      {Type: "string", Description: "The customer's current plan id"},
								"used":         {Type: "integer", Description: "Billable units consumed so far this calendar month"},
								"cap":          {Type: "integer", Description: "Monthly billable-unit cap for the customer's plan (0 = unlimited)"},
								"remaining":    {Type: "integer", Description: "Billable units remaining this month (-1 = unlimited)"},
								"period_start": {Type: "string", Description: "RFC3339 start of the current UTC billing period"},
								"period_end":   {Type: "string", Description: "RFC3339 end of the current UTC billing period"},
								"total_units":  {Type: "integer", Description: "Sum of billable_units across all operations this period"},
								"total_calls":  {Type: "integer", Description: "Count of usage_events rows across all operations this period"},
								"breakdown":    {Type: "array", Description: "Per-operation usage, ordered by total_units descending"},
							},
							Required: []string{"plan_id", "used", "cap", "remaining", "period_start", "period_end", "total_units", "total_calls", "breakdown"},
						}},
					},
				},
				"401": errResp("Unauthorized — missing or invalid API key"),
				"500": errResp("Internal server error"),
			},
		},
	}
}

// webhookEndpointsPathItems documents POST/GET /v1/webhooks/endpoints and
// DELETE /v1/webhooks/endpoints/{id}: the customer-facing CRUD tier for
// outbound webhook endpoint registration (see gateway/internal/webhookout).
// Framework infra, not a per-product invoke route — kept out of Build()'s
// invoke-route paths for the same reason as usagePathItem; see Handler.
func webhookEndpointsPathItems() map[string]PathItem {
	endpointProps := map[string]*Schema{
		"id":                {Type: "string", Description: "Endpoint UUID"},
		"url":               {Type: "string", Description: "The registered HTTPS delivery URL"},
		"active":            {Type: "boolean"},
		"subscribed_events": {Type: "array", Description: "Event types this endpoint receives; null means every event type"},
		"created_at":        {Type: "string", Description: "RFC3339 creation timestamp"},
	}
	createdProps := map[string]*Schema{}
	for k, v := range endpointProps {
		createdProps[k] = v
	}
	createdProps["secret_hex"] = &Schema{Type: "string", Description: "Signing secret, hex-encoded. Returned exactly once — store it now, it cannot be retrieved again."}
	createdSchema := &Schema{Type: "object", Properties: createdProps, Required: []string{"id", "url", "active", "created_at", "secret_hex"}}

	createRequestSchema := &Schema{
		Type: "object",
		Properties: map[string]*Schema{
			"url":               {Type: "string", Description: "Must be https:// and must not resolve to a private/loopback/link-local address"},
			"subscribed_events": {Type: "array", Description: "Optional subset of event types; omit or null to receive every event type"},
		},
		Required: []string{"url"},
	}

	updateSubscriptionRequestSchema := &Schema{
		Type: "object",
		Properties: map[string]*Schema{
			"subscribed_events": {Type: "array", Description: "Replaces the endpoint's subscribed event types; omit or null to resubscribe to every event type"},
		},
	}

	rotateSecretResponseSchema := &Schema{
		Type:       "object",
		Properties: map[string]*Schema{"secret_hex": {Type: "string", Description: "The new signing secret, hex-encoded. Returned exactly once — store it now, it cannot be retrieved again. The previous secret stops verifying immediately."}},
		Required:   []string{"secret_hex"},
	}

	return map[string]PathItem{
		"/v1/webhooks/endpoints": {
			Post: &Operation{
				OperationID: "create_webhook_endpoint",
				Summary:     "Register a new outbound webhook endpoint",
				Tags:        []string{"webhooks"},
				Security:    []SecurityRequirement{{apiKeyScheme: []string{}}},
				RequestBody: &RequestBody{
					Required: true,
					Content:  map[string]MediaType{contentTypeJSON: {Schema: createRequestSchema}},
				},
				Responses: map[string]Response{
					"201": {
						Description: "Endpoint registered; secret_hex is present only in this response",
						Content:     map[string]MediaType{contentTypeJSON: {Schema: createdSchema}},
					},
					"400": errResp("Bad request — invalid url, or unknown subscribed_events entry"),
					"401": errResp("Unauthorized — missing or invalid API key"),
					"500": errResp("Internal server error"),
				},
			},
			Get: &Operation{
				OperationID: "list_webhook_endpoints",
				Summary:     "List the authenticated customer's registered webhook endpoints",
				Tags:        []string{"webhooks"},
				Security:    []SecurityRequirement{{apiKeyScheme: []string{}}},
				Parameters: []Parameter{
					{Name: "page", In: "query", Description: "1-indexed page number; default 1", Schema: &Schema{Type: "integer"}},
					{Name: "per_page", In: "query", Description: "Page size; default 20, capped at 100", Schema: &Schema{Type: "integer"}},
				},
				Responses: map[string]Response{
					"200": {
						Description: "The caller's registered endpoints (secrets never included)",
						Content: map[string]MediaType{
							contentTypeJSON: {Schema: &Schema{
								Type:        "object",
								Description: "Paginated envelope of registered webhook endpoints",
								Properties: map[string]*Schema{
									"items": {Type: "array", Description: "Webhook endpoint objects for this page", Properties: map[string]*Schema{}},
									"total": {Type: "integer", Description: "Total registered endpoints across all pages"},
								},
								Required: []string{"items", "total"},
							}},
						},
					},
					"401": errResp("Unauthorized — missing or invalid API key"),
					"500": errResp("Internal server error"),
				},
			},
		},
		"/v1/webhooks/endpoints/{id}": {
			Patch: &Operation{
				OperationID: "update_webhook_endpoint_subscription",
				Summary:     "Replace the subscribed event types for a webhook endpoint owned by the authenticated customer",
				Tags:        []string{"webhooks"},
				Security:    []SecurityRequirement{{apiKeyScheme: []string{}}},
				Parameters: []Parameter{
					{Name: "id", In: "path", Required: true, Description: "Endpoint UUID", Schema: &Schema{Type: "string"}},
				},
				RequestBody: &RequestBody{
					Required: false,
					Content:  map[string]MediaType{contentTypeJSON: {Schema: updateSubscriptionRequestSchema}},
				},
				Responses: map[string]Response{
					"204": {Description: "Subscription updated"},
					"400": errResp("Bad request — malformed endpoint id, invalid json body, or unknown subscribed_events entry"),
					"401": errResp("Unauthorized — missing or invalid API key"),
					"404": errResp("Endpoint not found (includes ids owned by another customer)"),
					"500": errResp("Internal server error"),
				},
			},
			Delete: &Operation{
				OperationID: "delete_webhook_endpoint",
				Summary:     "Deactivate a registered webhook endpoint owned by the authenticated customer",
				Tags:        []string{"webhooks"},
				Security:    []SecurityRequirement{{apiKeyScheme: []string{}}},
				Parameters: []Parameter{
					{Name: "id", In: "path", Required: true, Description: "Endpoint UUID", Schema: &Schema{Type: "string"}},
				},
				Responses: map[string]Response{
					"204": {Description: "Endpoint deactivated"},
					"400": errResp("Bad request — malformed endpoint id"),
					"401": errResp("Unauthorized — missing or invalid API key"),
					"404": errResp("Endpoint not found (includes ids owned by another customer)"),
					"500": errResp("Internal server error"),
				},
			},
		},
		"/v1/webhooks/endpoints/{id}/rotate-secret": {
			Post: &Operation{
				OperationID: "rotate_webhook_endpoint_secret",
				Summary:     "Rotate the signing secret for a webhook endpoint owned by the authenticated customer",
				Tags:        []string{"webhooks"},
				Security:    []SecurityRequirement{{apiKeyScheme: []string{}}},
				Parameters: []Parameter{
					{Name: "id", In: "path", Required: true, Description: "Endpoint UUID", Schema: &Schema{Type: "string"}},
				},
				Responses: map[string]Response{
					"200": {
						Description: "Secret rotated; secret_hex is present only in this response",
						Content:     map[string]MediaType{contentTypeJSON: {Schema: rotateSecretResponseSchema}},
					},
					"400": errResp("Bad request — malformed endpoint id"),
					"401": errResp("Unauthorized — missing or invalid API key"),
					"404": errResp("Endpoint not found (includes ids owned by another customer)"),
					"500": errResp("Internal server error"),
				},
			},
		},
	}
}

// keysPathItems documents GET /v1/keys, POST /v1/keys/{id}/rotate, and
// DELETE /v1/keys/{id}: the customer-facing self-management tier for API
// keys — the API-key-authenticated counterpart to the dashboard's key
// management UI (dashboard/app/api/keys; see gateway/internal/auth/keyshttp.go).
// Framework infra, not a per-product invoke route — kept out of Build()'s
// invoke-route paths for the same reason as webhookEndpointsPathItems; see
// Handler.
func keysPathItems() map[string]PathItem {
	keyItemSchema := &Schema{
		Type: "object",
		Properties: map[string]*Schema{
			"id":           {Type: "string", Description: "API key UUID"},
			"prefix":       {Type: "string", Description: "Visible key prefix, e.g. \"cru_live_A3F9NK4M7QHGBVTP\""},
			"name":         {Type: "string", Description: "Customer-supplied label; null if unset"},
			"last_used_at": {Type: "string", Description: "RFC3339 timestamp of last successful auth; null if never used"},
			"expires_at":   {Type: "string", Description: "RFC3339 expiry set during a rotation grace window; null otherwise"},
			"created_at":   {Type: "string", Description: "RFC3339 creation timestamp"},
		},
		Required: []string{"id", "prefix", "created_at"},
	}

	rotateRequestSchema := &Schema{
		Type: "object",
		Properties: map[string]*Schema{
			"grace_secs": {Type: "integer", Description: "Seconds the old key stays valid after rotation; clamped to [0, 604800] (7 days); default 3600 (1 hour)"},
		},
	}

	rotateResponseSchema := &Schema{
		Type:       "object",
		Properties: map[string]*Schema{"key": {Type: "string", Description: "The new full API key. Returned exactly once — store it now, it cannot be retrieved again."}},
		Required:   []string{"key"},
	}

	return map[string]PathItem{
		"/v1/keys": {
			Get: &Operation{
				OperationID: "list_keys",
				Summary:     "List the authenticated customer's active API keys",
				Tags:        []string{"keys"},
				Security:    []SecurityRequirement{{apiKeyScheme: []string{}}},
				Parameters: []Parameter{
					{Name: "page", In: "query", Description: "1-indexed page number; default 1", Schema: &Schema{Type: "integer"}},
					{Name: "per_page", In: "query", Description: "Page size; default 20, capped at 100", Schema: &Schema{Type: "integer"}},
				},
				Responses: map[string]Response{
					"200": {
						Description: "The caller's active API keys (hash and full key never included)",
						Content: map[string]MediaType{
							contentTypeJSON: {Schema: &Schema{
								Type:        "object",
								Description: "Paginated envelope of the caller's active API keys",
								Properties: map[string]*Schema{
									"items": {Type: "array", Description: "API key objects for this page", Properties: keyItemSchema.Properties},
									"total": {Type: "integer", Description: "Total active keys across all pages"},
								},
								Required: []string{"items", "total"},
							}},
						},
					},
					"401": errResp("Unauthorized — missing or invalid API key"),
					"500": errResp("Internal server error"),
				},
			},
		},
		"/v1/keys/{id}/rotate": {
			Post: &Operation{
				OperationID: "rotate_key",
				Summary:     "Rotate an API key owned by the authenticated customer",
				Tags:        []string{"keys"},
				Security:    []SecurityRequirement{{apiKeyScheme: []string{}}},
				RequestBody: &RequestBody{
					Required: false,
					Content:  map[string]MediaType{contentTypeJSON: {Schema: rotateRequestSchema}},
				},
				Parameters: []Parameter{
					{Name: "id", In: "path", Required: true, Description: "API key UUID", Schema: &Schema{Type: "string"}},
				},
				Responses: map[string]Response{
					"200": {
						Description: "Key rotated; the new full key is present only in this response",
						Content:     map[string]MediaType{contentTypeJSON: {Schema: rotateResponseSchema}},
					},
					"400": errResp("Bad request — malformed key id or invalid json body"),
					"401": errResp("Unauthorized — missing or invalid API key"),
					"404": errResp("Key not found (includes ids owned by another customer)"),
					"500": errResp("Internal server error"),
				},
			},
		},
		"/v1/keys/{id}": {
			Delete: &Operation{
				OperationID: "revoke_key",
				Summary:     "Revoke an API key owned by the authenticated customer",
				Tags:        []string{"keys"},
				Security:    []SecurityRequirement{{apiKeyScheme: []string{}}},
				Parameters: []Parameter{
					{Name: "id", In: "path", Required: true, Description: "API key UUID", Schema: &Schema{Type: "string"}},
				},
				Responses: map[string]Response{
					"204": {Description: "Key revoked"},
					"400": errResp("Bad request — malformed key id"),
					"401": errResp("Unauthorized — missing or invalid API key"),
					"404": errResp("Key not found (includes ids owned by another customer)"),
					"500": errResp("Internal server error"),
				},
			},
		},
	}
}

// errorsPathItems documents GET /v1/errors: the customer-facing self-service
// error-history endpoint (see gateway/internal/selferrors), exposing the
// caller's own error_events rows recorded by errorlog.ErrorRecorder on every
// non-2xx /v1 response. Framework infra, not a per-product invoke route —
// kept out of Build()'s invoke-route paths for the same reason as
// usagePathItem/keysPathItems; see Handler.
func errorsPathItems() map[string]PathItem {
	eventProps := map[string]*Schema{
		"id":              {Type: "integer", Description: "error_events row id"},
		"operation":       {Type: "string", Description: "The /v1 route pattern that produced the error"},
		"error_code":      {Type: "string", Description: "Stable error code, e.g. RATE_LIMITED"},
		"http_status":     {Type: "integer"},
		"message":         {Type: "string"},
		"request_id":      {Type: "string", Description: "X-Request-ID of the failed request"},
		"created_at":      {Type: "string", Description: "RFC3339 creation timestamp"},
		"request_payload": {Type: "string", Description: "Captured request body, bounded UTF-8; null if capture was off or the row predates it"},
	}
	responseSchema := &Schema{
		Type: "object",
		Properties: map[string]*Schema{
			"data":     {Type: "array", Description: "Matching error_events rows, newest-first", Properties: eventProps},
			"has_more": {Type: "boolean", Description: "True when more rows exist beyond this page"},
			"page":     {Type: "integer"},
			"limit":    {Type: "integer"},
		},
		Required: []string{"data", "has_more", "page", "limit"},
	}

	return map[string]PathItem{
		"/v1/errors": {
			Get: &Operation{
				OperationID: "list_errors",
				Summary:     "List the authenticated customer's own error-history events",
				Tags:        []string{"errors"},
				Security:    []SecurityRequirement{{apiKeyScheme: []string{}}},
				Parameters: []Parameter{
					{Name: "from", In: "query", Description: "Inclusive ISO 8601 date (YYYY-MM-DD, UTC); defaults to 30 days ago", Schema: &Schema{Type: "string"}},
					{Name: "to", In: "query", Description: "Inclusive ISO 8601 date (YYYY-MM-DD, UTC); defaults to today; range capped at 90 days", Schema: &Schema{Type: "string"}},
					{Name: "operation", In: "query", Description: "Exact /v1/... path filter", Schema: &Schema{Type: "string"}},
					{Name: "code", In: "query", Description: "Exact error code filter, e.g. RATE_LIMITED", Schema: &Schema{Type: "string"}},
					{Name: "page", In: "query", Description: "1-indexed page number; default 1", Schema: &Schema{Type: "integer"}},
					{Name: "limit", In: "query", Description: "Page size; default 50, capped at 200", Schema: &Schema{Type: "integer"}},
				},
				Responses: map[string]Response{
					"200": {
						Description: "The caller's own error-history events, newest-first",
						Content:     map[string]MediaType{contentTypeJSON: {Schema: responseSchema}},
					},
					"400": errResp("Bad request — invalid date/operation/code filter or page"),
					"401": errResp("Unauthorized — missing or invalid API key"),
					"500": errResp("Internal server error"),
				},
			},
		},
	}
}
