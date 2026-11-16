package openapi

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

// usageEventsPathItems documents GET /v1/usage/events: the customer-facing
// per-event usage export (see gateway/internal/selfusagedetail), exposing the
// caller's own usage_events rows as a paginated JSON envelope or an RFC-4180
// CSV export. Framework infra, not a per-product invoke route — kept out of
// Build()'s invoke-route paths for the same reason as
// usagePathItem/keysPathItems/errorsPathItems; see Handler.
func usageEventsPathItems() map[string]PathItem {
	eventProps := map[string]*Schema{
		"id":             {Type: "string", Description: "usage_events row id (BIGSERIAL, encoded as a decimal string to avoid precision loss past Number.MAX_SAFE_INTEGER in JS clients)"},
		"operation":      {Type: "string", Description: "The opaque worker operation string this usage row was recorded under (RouteDescriptor.Operation, e.g. \"echo\") — not a /v1/... path"},
		"billable_units": {Type: "string", Description: "Billable units metered for this call (always >= 1, invariant #2), encoded as a decimal string — usage.Recorder.Record accepts any int64 worker-reported value, so a JSON number could silently lose precision past Number.MAX_SAFE_INTEGER in JS clients"},
		"created_at":     {Type: "string", Description: "RFC3339 creation timestamp"},
	}
	responseSchema := &Schema{
		Type: "object",
		Properties: map[string]*Schema{
			// Items (not Properties directly on the array schema) is the
			// standards-correct way to type a list's rows — see Schema.Items'
			// doc comment and jobsPathItems' listResponseSchema, which this
			// mirrors. Off-the-shelf OpenAPI/JSON Schema tooling ignores
			// "properties" on a type:"array" node, so a reconciliation caller
			// using such tooling would otherwise see an untyped array here.
			"data":     {Type: "array", Description: "Matching usage_events rows, newest-first", Items: &Schema{Type: "object", Properties: eventProps, Required: []string{"id", "operation", "billable_units", "created_at"}}},
			"has_more": {Type: "boolean", Description: "True when more rows exist beyond this page"},
			"page":     {Type: "integer"},
			"limit":    {Type: "integer"},
		},
		Required: []string{"data", "has_more", "page", "limit"},
	}

	return map[string]PathItem{
		"/v1/usage/events": {
			Get: &Operation{
				OperationID: "list_usage_events",
				Summary:     "Export the authenticated customer's own per-event metered usage rows",
				// The RFC-4180 CSV representation (?format=csv or Accept: text/csv,
				// see gateway/internal/selfusagedetail) is deliberately NOT declared as
				// a "format" query Parameter below: scripts/gen-clients.sh generates one
				// SDK method per operationId that always JSON-decodes its response, so a
				// typed "format" parameter would let a caller pass format="csv" through
				// the generated client and get back a body it can't parse. The endpoint
				// still honors ?format=csv/Accept: text/csv for direct HTTP callers —
				// it's just not exposed as a generated client parameter.
				Description: "Returns a JSON envelope by default. Direct HTTP callers (not the generated SDKs) may pass ?format=csv or an Accept: text/csv header for an RFC-4180 CSV export instead.",
				Tags:        []string{"usage"},
				Security:    []SecurityRequirement{{apiKeyScheme: []string{}}},
				Parameters: []Parameter{
					{Name: "from", In: "query", Description: "Inclusive ISO 8601 date (YYYY-MM-DD, UTC); defaults to 30 days ago", Schema: &Schema{Type: "string"}},
					{Name: "to", In: "query", Description: "Inclusive ISO 8601 date (YYYY-MM-DD, UTC); defaults to today; range capped at 90 days", Schema: &Schema{Type: "string"}},
					{Name: "operation", In: "query", Description: "Exact match against the opaque worker operation string (e.g. \"echo\"); max 128 characters", Schema: &Schema{Type: "string"}},
					{Name: "page", In: "query", Description: "1-indexed page number; default 1", Schema: &Schema{Type: "integer"}},
					{Name: "limit", In: "query", Description: "Page size; default 50, capped at 200", Schema: &Schema{Type: "integer"}},
				},
				Responses: map[string]Response{
					"200": {
						Description: "The caller's own usage events, newest-first, as a JSON envelope (default) or RFC-4180 CSV (format=csv or Accept: text/csv)",
						Content: map[string]MediaType{
							contentTypeJSON: {Schema: responseSchema},
							"text/csv":      {Schema: &Schema{Type: "string", Description: "Header row: id,operation,billable_units,created_at"}},
						},
					},
					"400": errResp("Bad request — invalid date/operation filter or page"),
					"401": errResp("Unauthorized — missing or invalid API key"),
					"500": errResp("Internal server error"),
				},
			},
		},
	}
}
