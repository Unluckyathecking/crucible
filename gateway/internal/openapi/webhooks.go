package openapi

import (
	"strings"

	"github.com/Unluckyathecking/crucible/gateway/internal/events"
)

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
	{
		eventType: events.JobSucceeded,
		summary:   "Fired when a durable async job reaches the terminal succeeded state.",
		schema: &Schema{
			Type: "object",
			Properties: map[string]*Schema{
				"job_id":    {Type: "string", Description: "The async job's id; fetch the result via GET /v1/jobs/{id}"},
				"operation": {Type: "string", Description: "The operation string the job was enqueued with"},
				"status":    {Type: "string", Description: "Always \"succeeded\" for this event"},
			},
			Required: []string{"job_id", "operation", "status"},
		},
	},
	{
		eventType: events.JobFailed,
		summary:   "Fired when a durable async job reaches the terminal failed state (structured worker error, billable_units<1 contract violation, or retry-exhausted dead-letter).",
		schema: &Schema{
			Type: "object",
			Properties: map[string]*Schema{
				"job_id":     {Type: "string", Description: "The async job's id; fetch details via GET /v1/jobs/{id}"},
				"operation":  {Type: "string", Description: "The operation string the job was enqueued with"},
				"status":     {Type: "string", Description: "Always \"failed\" for this event"},
				"error_code": {Type: "string", Description: "The sanitized (or full, per WORKER_ERROR_EXPOSURE) error code"},
			},
			Required: []string{"job_id", "operation", "status", "error_code"},
		},
	},
	{
		eventType: events.EndpointDisabled,
		summary:   "Fired when one of the customer's own webhook endpoints auto-disables after exceeding the consecutive delivery-failure threshold. Delivered to the customer's other active endpoints, never to the one just disabled.",
		schema: &Schema{
			Type: "object",
			Properties: map[string]*Schema{
				"endpoint_id": {Type: "string", Description: "The auto-disabled endpoint's UUID"},
				"reason":      {Type: "string", Description: "Always \"delivery_failures\" for this event"},
			},
			Required: []string{"endpoint_id", "reason"},
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
