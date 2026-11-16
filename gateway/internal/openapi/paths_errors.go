package openapi

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
