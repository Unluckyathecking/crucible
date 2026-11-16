package openapi

import (
	"encoding/json"
	"strings"
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

// invokeResponseEnvelope wraps a route's ResponseSchema into the documented
// shape of the frozen /v1 invoke success envelope (proxy.InvokeResponse),
// mirroring its json tags exactly: payload (the worker's typed result),
// billable_units (always present on success — enforced at the trust boundary,
// see server/routes.go), and units_label (optional, human-readable unit name).
func invokeResponseEnvelope(responseSchema *Schema) *Schema {
	return &Schema{
		Type: "object",
		Properties: map[string]*Schema{
			"payload":        responseSchema,
			"billable_units": {Type: "integer"},
			"units_label":    {Type: "string"},
		},
		Required: []string{"payload", "billable_units"},
	}
}

// invokeOperation builds the standard Operation for a per-product /v1 invoke endpoint.
// schema is the route's RequestSchema; when nil the request body is documented as a
// generic object ({"type":"object"}) preserving backwards-compatibility. sampleRequest
// is the route's RouteDescriptor.SampleRequest, if any; when non-nil it is emitted
// as the request body's "example" value. responseSchema is the route's ResponseSchema;
// when nil the 200 body stays the generic {"type":"object"} preserving
// backwards-compatibility, otherwise it's wrapped into the invoke success envelope by
// invokeResponseEnvelope. async is the route's RouteDescriptor.Async: when true the
// documented success response is 202 {job_id} (durable async execution, see
// gateway/internal/jobs) instead of the synchronous 200 invoke envelope, and 502
// (which can only happen on the inline synchronous worker call) is omitted — an
// async POST never calls the worker itself, so it can't fail that way.
func invokeOperation(operationID, summary string, schema *Schema, sampleRequest json.RawMessage, responseSchema *Schema, async bool) *Operation {
	reqBodySchema := &Schema{Type: "object"}
	if schema != nil {
		reqBodySchema = schema
	}

	responses := map[string]Response{
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
	}
	if async {
		responses["202"] = Response{
			Description: "Job accepted for durable async execution. Poll GET /v1/jobs/{job_id} for status and result; quota/rate-limit admission for this request already ran here — the job's real billable_units are metered separately when it completes.",
			Headers:     rateLimitAndQuotaHeaders(),
			Content: map[string]MediaType{
				contentTypeJSON: {Schema: &Schema{
					Type: "object",
					Properties: map[string]*Schema{
						"job_id": {Type: "string", Description: "Pass to GET /v1/jobs/{job_id} to poll for status and result"},
					},
					Required: []string{"job_id"},
				}},
			},
		}
	} else {
		respBodySchema := &Schema{Type: "object"}
		if responseSchema != nil {
			respBodySchema = invokeResponseEnvelope(responseSchema)
		}
		responses["200"] = Response{
			Description: "Successful invocation",
			Headers:     rateLimitAndQuotaHeaders(),
			Content: map[string]MediaType{
				contentTypeJSON: {Schema: respBodySchema},
			},
		}
		responses["502"] = errResp("Worker unavailable")
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
				contentTypeJSON: {Schema: reqBodySchema, Example: sampleRequest},
			},
		},
		Responses: responses,
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
