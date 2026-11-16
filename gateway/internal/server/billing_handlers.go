package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"

	"github.com/rs/zerolog/log"

	"github.com/Unluckyathecking/crucible/gateway/internal/apierror"
	"github.com/Unluckyathecking/crucible/gateway/internal/auth"
	"github.com/Unluckyathecking/crucible/gateway/internal/billing"
	mw "github.com/Unluckyathecking/crucible/gateway/internal/middleware"
)

// planIDRE mirrors the dashboard's PLAN_ID_RE: lowercase alphanumeric + hyphens, max 32 chars.
// The gateway is the trust boundary; revalidating here prevents DB probing via arbitrary plan IDs.
var planIDRE = regexp.MustCompile(`^[a-z0-9-]{1,32}$`)

// BillingService is the subset of billing.CheckoutClient used by the billing
// route handlers. Extracted as an interface so tests can inject stubs without
// requiring a live Stripe API or database.
type BillingService interface {
	CreateCheckoutSession(ctx context.Context, customerID, planID string) (string, error)
	CreatePortalSession(ctx context.Context, stripeCustomerID string) (string, error)
	LookupStripeCustomerID(ctx context.Context, customerID string) (string, error)
}

// billingCheckoutHandler creates a Stripe Checkout session and returns the redirect URL.
// POST /v1/billing/checkout body: {"plan_id":"pro"}
// Response: {"url":"https://checkout.stripe.com/..."}
func billingCheckoutHandler(d *Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rid, _ := r.Context().Value(mw.RequestIDKey).(string)
		if d.Checkout == nil {
			apierror.Write(w, rid, http.StatusServiceUnavailable, apierror.NOT_CONFIGURED, "billing not configured", false)
			return
		}
		key := auth.FromContext(r.Context())
		if key == nil {
			apierror.Write(w, rid, http.StatusUnauthorized, apierror.UNAUTHORIZED, "no auth context", false)
			return
		}

		var body struct {
			PlanID string `json:"plan_id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.PlanID == "" {
			apierror.Write(w, rid, http.StatusBadRequest, apierror.BAD_REQUEST, "plan_id required", false)
			return
		}
		if !planIDRE.MatchString(body.PlanID) {
			apierror.Write(w, rid, http.StatusBadRequest, apierror.BAD_REQUEST, "invalid plan_id", false)
			return
		}

		redirectURL, err := d.Checkout.CreateCheckoutSession(r.Context(), key.Customer.ID.String(), body.PlanID)
		if err != nil {
			if errors.Is(err, billing.ErrPlanNotFound) {
				apierror.Write(w, rid, http.StatusUnprocessableEntity, apierror.PLAN_NOT_FOUND, "plan not found or not upgradeable", false)
				return
			}
			log.Error().Err(err).Msg("create checkout session failed")
			apierror.Write(w, rid, http.StatusBadGateway, apierror.STRIPE_ERROR, "billing unavailable", false)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"url": redirectURL})
	}
}

// billingPortalHandler creates a Stripe Billing Portal session and returns the redirect URL.
// POST /v1/billing/portal
// Response: {"url":"https://billing.stripe.com/..."}
func billingPortalHandler(d *Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rid, _ := r.Context().Value(mw.RequestIDKey).(string)
		if d.Checkout == nil {
			apierror.Write(w, rid, http.StatusServiceUnavailable, apierror.NOT_CONFIGURED, "billing not configured", false)
			return
		}
		key := auth.FromContext(r.Context())
		if key == nil {
			apierror.Write(w, rid, http.StatusUnauthorized, apierror.UNAUTHORIZED, "no auth context", false)
			return
		}

		stripeCustomerID, err := d.Checkout.LookupStripeCustomerID(r.Context(), key.Customer.ID.String())
		if err != nil {
			log.Error().Err(err).Msg("lookup stripe customer id failed")
			apierror.Write(w, rid, http.StatusInternalServerError, apierror.INTERNAL, "lookup failed", false)
			return
		}
		if stripeCustomerID == "" {
			apierror.Write(w, rid, http.StatusPaymentRequired, apierror.NO_STRIPE_CUSTOMER, "complete checkout first", false)
			return
		}

		redirectURL, err := d.Checkout.CreatePortalSession(r.Context(), stripeCustomerID)
		if err != nil {
			log.Error().Err(err).Msg("create portal session failed")
			apierror.Write(w, rid, http.StatusBadGateway, apierror.STRIPE_ERROR, "billing unavailable", false)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"url": redirectURL})
	}
}
