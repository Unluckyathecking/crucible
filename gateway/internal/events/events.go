// Package events defines the canonical outbound-webhook event catalogue: stable
// event-type constants and the JSON payload shape each one carries. Every
// call-site that emits an outbound webhook (Stripe webhook handler, quota
// middleware, auth store) imports this package so the wire format for a given
// event type is defined exactly once and can't drift per call-site.
package events

const (
	// SubscriptionUpdated fires when a Stripe subscription is created or updated
	// (including plan changes) and the customer's plan_id is upserted.
	SubscriptionUpdated = "subscription.updated"
	// SubscriptionDeleted fires when a Stripe subscription is canceled and the
	// customer's plan_id reverts to free.
	SubscriptionDeleted = "subscription.deleted"
	// QuotaExceeded fires when a request is rejected because the customer's
	// monthly billable-unit cap has been reached.
	QuotaExceeded = "quota.exceeded"
	// APIKeyRotated fires when Store.Rotate mints a replacement API key.
	APIKeyRotated = "api_key.rotated"
	// APIKeyRevoked fires when Store.Revoke marks an API key revoked.
	APIKeyRevoked = "api_key.revoked"
)

// AllEventTypes is the single source of truth for the full event-type set.
// gateway/internal/openapi documents exactly these types and panics at Build()
// time if its webhook descriptor list falls out of sync with this slice.
var AllEventTypes = []string{
	SubscriptionUpdated,
	SubscriptionDeleted,
	QuotaExceeded,
	APIKeyRotated,
	APIKeyRevoked,
}

// IsValidEventType reports whether eventType is a member of AllEventTypes.
// Callers that accept an event-type value from outside this package — e.g.
// webhook endpoint subscription registration — should validate against this
// helper rather than re-deriving the catalogue set, so additions to
// AllEventTypes are picked up automatically instead of drifting out of sync.
func IsValidEventType(eventType string) bool {
	for _, t := range AllEventTypes {
		if t == eventType {
			return true
		}
	}
	return false
}

// SubscriptionEventPayload is the payload for SubscriptionUpdated and
// SubscriptionDeleted. PlanID is "free" for SubscriptionDeleted.
type SubscriptionEventPayload struct {
	CustomerID string `json:"customer_id"`
	PlanID     string `json:"plan_id"`
}

// QuotaExceededPayload is the payload for QuotaExceeded.
type QuotaExceededPayload struct {
	CustomerID string `json:"customer_id"`
	Plan       string `json:"plan"`
	Cap        int64  `json:"cap"`
}

// APIKeyRotatedPayload is the payload for APIKeyRotated.
type APIKeyRotatedPayload struct {
	CustomerID string `json:"customer_id"`
	OldKeyID   string `json:"old_key_id"`
	NewKeyID   string `json:"new_key_id"`
}

// APIKeyRevokedPayload is the payload for APIKeyRevoked.
type APIKeyRevokedPayload struct {
	CustomerID string `json:"customer_id"`
	KeyID      string `json:"key_id"`
}
