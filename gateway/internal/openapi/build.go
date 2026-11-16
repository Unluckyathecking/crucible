package openapi

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
		paths[key] = PathItem{Post: invokeOperation(opID, rt.Summary, rt.RequestSchema, rt.SampleRequest, rt.ResponseSchema, rt.Async)}
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
