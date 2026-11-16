package openapi

// jobsPathItems documents GET /v1/jobs/{id}: the status and eventual result
// of a durable async job enqueued by a POST /v1/<op> route opted into
// routes_table.go's AsyncRoutes (see gateway/internal/jobs). Framework
// infra, not a per-product invoke route — kept out of Build()'s invoke-route
// paths for the same reason as usagePathItem/keysPathItems/errorsPathItems;
// see Handler.
func jobsPathItems() map[string]PathItem {
	responseSchema := &Schema{
		Type: "object",
		Properties: map[string]*Schema{
			"job_id":         {Type: "string", Description: "Job UUID, returned by the enqueuing POST /v1/<op> call"},
			"status":         {Type: "string", Description: "queued | running | succeeded | failed | cancelled"},
			"result":         {Type: "object", Description: "The worker's payload; present only when status is succeeded"},
			"error":          {Type: "object", Description: "Structured error {code, message}; present only when status is failed"},
			"billable_units": {Type: "integer", Description: "Present only when status is succeeded; always >= 1 (invariant #2)"},
			"units_label":    {Type: "string"},
			"created_at":     {Type: "string", Description: "RFC3339 enqueue timestamp"},
			"updated_at":     {Type: "string", Description: "RFC3339 timestamp of the last status change"},
		},
		Required: []string{"job_id", "status", "created_at", "updated_at"},
	}

	listItemProps := map[string]*Schema{
		"job_id":         {Type: "string", Description: "Job UUID, returned by the enqueuing POST /v1/<op> call"},
		"operation":      {Type: "string", Description: "The opaque operation string of the POST /v1/<op> route that enqueued this job"},
		"status":         {Type: "string", Description: "queued | running | succeeded | failed | cancelled"},
		"error":          {Type: "object", Description: "Structured error {code, message}; present only when status is failed"},
		"billable_units": {Type: "integer", Description: "Present only when status is succeeded; always >= 1 (invariant #2)"},
		"units_label":    {Type: "string"},
		"created_at":     {Type: "string", Description: "RFC3339 enqueue timestamp"},
		"updated_at":     {Type: "string", Description: "RFC3339 timestamp of the last status change"},
	}
	listResponseSchema := &Schema{
		Type:        "object",
		Description: "Paginated envelope of the caller's own async jobs",
		Properties: map[string]*Schema{
			"items": {
				Type:        "array",
				Description: "Matching async_jobs rows, newest-first",
				Items:       &Schema{Type: "object", Properties: listItemProps, Required: []string{"job_id", "operation", "status", "created_at", "updated_at"}},
			},
			"total": {Type: "integer", Description: "Total matching jobs across all pages"},
		},
		Required: []string{"items", "total"},
	}

	return map[string]PathItem{
		"/v1/jobs/{id}": {
			Get: &Operation{
				OperationID: "get_job",
				Summary:     "Get the status and result of an async job owned by the authenticated customer",
				Tags:        []string{"jobs"},
				Security:    []SecurityRequirement{{apiKeyScheme: []string{}}},
				Parameters: []Parameter{
					{Name: "id", In: "path", Required: true, Description: "Job UUID", Schema: &Schema{Type: "string"}},
				},
				Responses: map[string]Response{
					"200": {
						Description: "Current job status; result/error/billable_units populate once the job reaches a terminal state",
						Content:     map[string]MediaType{contentTypeJSON: {Schema: responseSchema}},
					},
					"400": errResp("Bad request — malformed job id"),
					"401": errResp("Unauthorized — missing or invalid API key"),
					"404": errResp("Job not found (includes ids owned by another customer)"),
					"500": errResp("Internal server error"),
				},
			},
		},
		"/v1/jobs/{id}/cancel": {
			Post: &Operation{
				OperationID: "cancel_job",
				Summary:     "Cancel a still-queued async job owned by the authenticated customer",
				Description: "Only a job in the queued state can be cancelled this way; there is no cooperative cancellation of a job that has already started running. Cancelling transitions the job to the terminal cancelled state — it is never claimed or billed.",
				Tags:        []string{"jobs"},
				Security:    []SecurityRequirement{{apiKeyScheme: []string{}}},
				Parameters: []Parameter{
					{Name: "id", In: "path", Required: true, Description: "Job UUID", Schema: &Schema{Type: "string"}},
				},
				Responses: map[string]Response{
					"200": {
						Description: "Job cancelled",
						Content:     map[string]MediaType{contentTypeJSON: {Schema: responseSchema}},
					},
					"400": errResp("Bad request — malformed job id"),
					"401": errResp("Unauthorized — missing or invalid API key"),
					"404": errResp("Job not found (includes ids owned by another customer)"),
					"409": errResp("Job cannot be cancelled in its current state (not queued)"),
					"500": errResp("Internal server error"),
				},
			},
		},
		"/v1/jobs": {
			Get: &Operation{
				OperationID: "list_jobs",
				Summary:     "List the authenticated customer's own async jobs",
				Tags:        []string{"jobs"},
				Security:    []SecurityRequirement{{apiKeyScheme: []string{}}},
				Parameters: []Parameter{
					{Name: "page", In: "query", Description: "1-indexed page number; default 1", Schema: &Schema{Type: "integer"}},
					{Name: "per_page", In: "query", Description: "Page size; default 20, capped at 100", Schema: &Schema{Type: "integer"}},
					{Name: "status", In: "query", Description: "Filter to exactly one status: queued | running | succeeded | failed | cancelled", Schema: &Schema{Type: "string"}},
					{Name: "operation", In: "query", Description: "Filter to jobs enqueued with this exact operation string", Schema: &Schema{Type: "string"}},
				},
				Responses: map[string]Response{
					"200": {
						Description: "The caller's own async jobs matching the given filters, newest-first",
						Content:     map[string]MediaType{contentTypeJSON: {Schema: listResponseSchema}},
					},
					"400": errResp("Bad request — unknown status filter, or page too large"),
					"401": errResp("Unauthorized — missing or invalid API key"),
					"500": errResp("Internal server error"),
				},
			},
		},
	}
}
