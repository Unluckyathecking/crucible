package openapi

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
		"disabled_at":       {Type: "string", Description: "RFC3339 timestamp the endpoint was auto-disabled at; null unless currently auto-disabled"},
		"disabled_reason":   {Type: "string", Description: "Why the endpoint was auto-disabled, e.g. \"delivery_failures\"; null unless currently auto-disabled"},
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
		"/v1/webhooks/endpoints/{id}/enable": {
			Post: &Operation{
				OperationID: "enable_webhook_endpoint",
				Summary:     "Re-enable a webhook endpoint owned by the authenticated customer that was auto-disabled for chronic delivery failure",
				Tags:        []string{"webhooks"},
				Security:    []SecurityRequirement{{apiKeyScheme: []string{}}},
				Parameters: []Parameter{
					{Name: "id", In: "path", Required: true, Description: "Endpoint UUID", Schema: &Schema{Type: "string"}},
				},
				Responses: map[string]Response{
					"204": {Description: "Endpoint re-enabled"},
					"400": errResp("Bad request — malformed endpoint id"),
					"401": errResp("Unauthorized — missing or invalid API key"),
					"404": errResp("Endpoint not found, owned by another customer, or not currently auto-disabled (includes customer soft-deleted endpoints)"),
					"500": errResp("Internal server error"),
				},
			},
		},
	}
}

// webhookDeliveriesPathItems documents GET /v1/webhooks/deliveries: the
// customer-facing paginated delivery log exposing the caller's own outbound
// webhook delivery rows across all their registered endpoints (see
// webhookDeliveriesHandler in gateway/internal/server/routes.go). Framework
// infra, not a per-product invoke route — kept out of Build()'s invoke-route
// paths for the same reason as usagePathItem/keysPathItems/errorsPathItems;
// see Handler.
func webhookDeliveriesPathItems() map[string]PathItem {
	itemProps := map[string]*Schema{
		"id":                 {Type: "string", Description: "Delivery UUID"},
		"event_id":           {Type: "string", Description: "The outbound webhook event type (e.g. invocation.succeeded)"},
		"endpoint_url":       {Type: "string", Description: "The registered HTTPS delivery URL the event was sent to"},
		"status":             {Type: "string", Description: "pending | delivered | failed"},
		"attempts":           {Type: "integer", Description: "Number of delivery attempts made so far"},
		"last_response_code": {Type: "integer", Description: "HTTP status code returned by the endpoint on the last attempt; absent if no attempt has completed"},
		"created_at":         {Type: "string", Description: "RFC3339 creation timestamp"},
	}
	responseSchema := &Schema{
		Type:        "object",
		Description: "Paginated envelope of the caller's own webhook deliveries",
		Properties: map[string]*Schema{
			"items": {
				Type:        "array",
				Description: "Matching webhook_deliveries rows, newest-first",
				Items:       &Schema{Type: "object", Properties: itemProps, Required: []string{"id", "event_id", "endpoint_url", "status", "attempts", "created_at"}},
			},
			"total": {Type: "integer", Description: "Total matching deliveries across all pages"},
		},
		Required: []string{"items", "total"},
	}

	return map[string]PathItem{
		"/v1/webhooks/deliveries": {
			Get: &Operation{
				OperationID: "list_webhook_deliveries",
				Summary:     "List the authenticated customer's outbound webhook deliveries across all their registered endpoints",
				Tags:        []string{"webhooks"},
				Security:    []SecurityRequirement{{apiKeyScheme: []string{}}},
				Parameters: []Parameter{
					{Name: "page", In: "query", Description: "1-indexed page number; default 1", Schema: &Schema{Type: "integer"}},
					{Name: "per_page", In: "query", Description: "Page size; default 100, capped at 100", Schema: &Schema{Type: "integer"}},
				},
				Responses: map[string]Response{
					"200": {
						Description: "The caller's own webhook deliveries, newest-first",
						Content:     map[string]MediaType{contentTypeJSON: {Schema: responseSchema}},
					},
					"400": errResp("Bad request — page too large"),
					"401": errResp("Unauthorized — missing or invalid API key"),
					"500": errResp("Internal server error"),
				},
			},
		},
	}
}
