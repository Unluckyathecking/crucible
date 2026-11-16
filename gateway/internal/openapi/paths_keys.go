package openapi

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
