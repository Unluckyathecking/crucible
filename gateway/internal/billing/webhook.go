package billing

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/zerolog/log"

	"github.com/Unluckyathecking/crucible/gateway/internal/events"
	"github.com/Unluckyathecking/crucible/gateway/internal/webhookout"
)

// db is the subset of *pgxpool.Pool used by this package. Extracted as an
// interface to allow test mocking without changing runtime behaviour. Also
// used by PlanCache (plans.go), so its surface stays minimal — Webhook's own
// Begin requirement is added on top via txDB below rather than growing this
// shared interface.
type db interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// txDB extends db with Begin, needed only by recordEventAndEmit's
// transactional dedup+emit path (see its doc comment). *pgxpool.Pool and
// pgxmock's mock pool both satisfy it already; kept separate from the
// shared db interface so PlanCache and its own test fakes are unaffected.
type txDB interface {
	db
	Begin(ctx context.Context) (pgx.Tx, error)
}

// CacheDeleter abstracts the Redis DEL operation. Extracting it as an interface
// lets webhook tests inject a spy without importing the redis client.
type CacheDeleter interface {
	Del(ctx context.Context, keys ...string) error
}

// WebhookOption is a functional option for NewWebhook.
type WebhookOption func(*Webhook)

// WithCacheDeleter wires a Redis DEL client so that handlers can immediately
// invalidate auth:<prefix> cache entries after a customer-link or plan change
// (CLAUDE.md invariant #7). Without this option, cache entries expire naturally
// after the 60 s TTL — acceptable for existing deployments but not for upgrades.
func WithCacheDeleter(d CacheDeleter) WebhookOption {
	return func(w *Webhook) { w.cache = d }
}

// Webhook receives Stripe events, verifies HMAC, dedupes via webhook_events,
// and updates the customer's plan_id on subscription lifecycle events.
type Webhook struct {
	secret  string
	db      txDB
	cache   CacheDeleter // optional; nil → no immediate cache invalidation
	now     func() time.Time // injectable for tests
	emitter *webhookout.Emitter // optional; nil → no outbound webhook emission
}

// SetEmitter wires the outbound webhook emitter used to notify customers of
// subscription lifecycle events. Called once from server.NewRouter, since the
// emitter is constructed there (from Deps.DB) after main.go builds the Webhook.
// A nil emitter (e.g. Deps.DB unset) makes emission a safe no-op — Emitter.Emit
// nil-checks its receiver.
func (h *Webhook) SetEmitter(e *webhookout.Emitter) {
	h.emitter = e
}

// NewWebhook constructs a Webhook. opts allows injecting optional dependencies
// (e.g. WithCacheDeleter) without breaking callers that pass only the two required
// args — critical because cmd/gateway/main.go must not be touched (PR #48 disjoint).
func NewWebhook(secret string, pool *pgxpool.Pool, opts ...WebhookOption) *Webhook {
	w := &Webhook{secret: secret, db: pool, now: time.Now}
	for _, o := range opts {
		o(w)
	}
	return w
}

func (h *Webhook) Handle(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, "read error", http.StatusBadRequest)
		return
	}
	if err := h.verifySignature(r.Header.Get("Stripe-Signature"), body); err != nil {
		log.Warn().Err(err).Msg("stripe webhook signature verification failed")
		http.Error(w, "invalid signature", http.StatusBadRequest)
		return
	}

	var event stripeEvent
	if err := json.Unmarshal(body, &event); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}

	// Dedupe check (read-only). If we've seen this event_id before, ack and stop.
	seen, err := h.eventSeen(r.Context(), event.ID)
	if err != nil {
		log.Error().Err(err).Msg("webhook seen-check failed")
		http.Error(w, "persist error", http.StatusInternalServerError)
		return
	}
	if seen {
		w.WriteHeader(http.StatusOK)
		return
	}

	// Dispatch FIRST. If the handler fails, do NOT record the event so Stripe's retry can re-process.
	// Recording before dispatch (the old order) caused permanent loss of events on transient handler errors.
	emission, err := h.dispatch(r.Context(), &event)
	if err != nil {
		log.Error().Err(err).Str("event_type", event.Type).Msg("webhook handler failed")
		http.Error(w, "handler error", http.StatusInternalServerError)
		return
	}

	// emission != nil means the dispatched handler has a subscription webhook
	// to enqueue (an emitter is configured and the customer.subscription
	// upsert/delete actually applied). recordEventAndEmit records the dedup
	// row and enqueues that webhook atomically — either both commit, or (on
	// any failure, including a lost ON CONFLICT dedup race against a
	// concurrent duplicate delivery) neither does, so a lost webhook can never
	// be marked processed. On failure we return 500 so Stripe retries the
	// SAME event, giving the enqueue another chance instead of silently
	// dropping the customer notification the way a post-hoc best-effort Emit
	// call would have.
	if emission != nil {
		if _, err := h.recordEventAndEmit(r.Context(), event.ID, event.Type, body, emission); err != nil {
			log.Error().Err(err).Str("event_type", event.Type).Msg("webhook record+emit tx failed — event not marked processed, Stripe will retry")
			http.Error(w, "persist error", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
		return
	}

	// No pending emission (no emitter configured, or this event type/outcome
	// carries no customer-facing webhook): record for dedup exactly as
	// before dispatch handlers ever gained an EmitTx path.
	if _, err := h.recordEvent(r.Context(), event.ID, event.Type, body); err != nil {
		log.Error().Err(err).Msg("webhook record failed AFTER successful dispatch — duplicate dispatch possible on retry")
		// Still return 200 — the action ran. A re-dispatch is at worst a no-op (handler is idempotent on subscription state).
	}
	w.WriteHeader(http.StatusOK)
}

func (h *Webhook) eventSeen(ctx context.Context, eventID string) (bool, error) {
	var exists bool
	err := h.db.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM webhook_events WHERE event_id = $1)`,
		eventID,
	).Scan(&exists)
	return exists, err
}

// VerifySignature is exported for test reuse. Format follows Stripe's t=...,v1=... scheme.
func (h *Webhook) VerifySignature(header string, body []byte) error {
	return h.verifySignature(header, body)
}

func (h *Webhook) verifySignature(header string, body []byte) error {
	if header == "" {
		return errors.New("missing stripe-signature header")
	}
	// maxSigCandidates bounds how many v1= signatures we parse and constant-time
	// compare. The endpoint is unauthenticated, so an attacker could otherwise stuff
	// the header with unbounded v1= values to force unbounded HMAC comparisons. We
	// keep only the first N candidates; any beyond the cap are ignored (a valid
	// signature past position N is treated as not present).
	const maxSigCandidates = 8
	var timestamp string
	var sigs []string
	for _, p := range strings.Split(header, ",") {
		kv := strings.SplitN(p, "=", 2)
		if len(kv) != 2 {
			continue
		}
		switch kv[0] {
		case "t":
			timestamp = kv[1]
		case "v1":
			if len(sigs) < maxSigCandidates {
				sigs = append(sigs, kv[1])
			}
		}
	}
	if timestamp == "" || len(sigs) == 0 {
		return errors.New("malformed stripe-signature header")
	}
	ts, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil {
		return errors.New("bad timestamp")
	}
	if h.now().Sub(time.Unix(ts, 0)) > 5*time.Minute {
		return errors.New("event too old (replay protection)")
	}

	payload := timestamp + "." + string(body)
	mac := hmac.New(sha256.New, []byte(h.secret))
	_, _ = mac.Write([]byte(payload))
	expected := mac.Sum(nil)

	const stripeSignatureHexLen = 64
	for _, sig := range sigs {
		if len(sig) != stripeSignatureHexLen {
			continue
		}
		sigMAC, err := hex.DecodeString(strings.ToUpper(sig))
		if err != nil {
			continue
		}
		if hmac.Equal(sigMAC, expected) {
			return nil
		}
	}
	return errors.New("no signature matched")
}

type stripeEvent struct {
	ID   string          `json:"id"`
	Type string          `json:"type"`
	Data stripeEventData `json:"data"`
}
type stripeEventData struct {
	Object json.RawMessage `json:"object"`
}

func (h *Webhook) recordEvent(ctx context.Context, id, eventType string, payload []byte) (bool, error) {
	tag, err := h.db.Exec(ctx, `
		INSERT INTO webhook_events (event_id, type, payload) VALUES ($1, $2, $3)
		ON CONFLICT (event_id) DO NOTHING
	`, id, eventType, payload)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// Note: dispatch handlers are idempotent on subscription/customer state — running them
// twice on the same event yields the same result. The dispatch-then-record ordering above
// accepts a low-probability double-dispatch (record race) in exchange for never losing an event.

// recordEventAndEmit atomically records the Stripe event as processed (the
// same webhook_events dedup insert recordEvent performs) and enqueues
// emission's outbound customer webhook, on one transaction: either both
// survive, or neither does. This closes two loss windows at once:
//
//  1. The crash window between recordEvent committing and a separate Emit
//     call running — previously an Emit failure here (or a crash before it
//     ran) permanently lost the customer notification, since a recorded
//     event is never re-dispatched.
//  2. Concurrent duplicate deliveries of the SAME Stripe event (retries
//     landing before the first delivery's dedup insert commits): the
//     ON CONFLICT DO NOTHING on webhook_events is evaluated by Postgres
//     itself inside this transaction, so only the delivery that genuinely
//     wins the race ever reaches the EmitTx call — the others observe
//     RowsAffected()==0 and roll back before enqueuing anything. This is
//     the same dedup guarantee the old post-hoc "only emit if recordEvent
//     confirms non-race-loser" pattern provided, just enforced by the
//     database instead of by ordering emission after the fact.
//
// recorded reports whether this call won the dedup race — Handle doesn't
// currently need the distinction (both outcomes return the same 200), but
// the return value is kept for symmetry with recordEvent and future callers.
// Note this is deliberately NOT "the same tx as the customers.plan_id
// upsert" itself: that UPDATE already runs (and commits) inside dispatch,
// before Handle can know whether this delivery will win the dedup race —
// merging it here would mean re-applying it from a rolled-back loser's tx,
// which handleSubscriptionUpsert/handleSubscriptionDeleted's callers (real
// Postgres integration tests AND pgxmock-based unit tests exercising those
// handlers directly) don't expect. Fusing the dedup insert with the
// emission it gates is the atomicity boundary that's both safe to add and
// closes the loss window that actually matters: whether the CUSTOMER
// notification survives a crash, independent of dispatch's own state write.
func (h *Webhook) recordEventAndEmit(ctx context.Context, id, eventType string, body []byte, emission *pendingEmission) (recorded bool, err error) {
	tx, err := h.db.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	tag, err := tx.Exec(ctx, `
		INSERT INTO webhook_events (event_id, type, payload) VALUES ($1, $2, $3)
		ON CONFLICT (event_id) DO NOTHING
	`, id, eventType, body)
	if err != nil {
		return false, fmt.Errorf("record event: %w", err)
	}
	if tag.RowsAffected() != 1 {
		// Lost the dedup race: another delivery already recorded and emitted
		// this event. Roll back (deferred) — nothing to commit.
		return false, nil
	}

	payload, err := json.Marshal(events.SubscriptionEventPayload{CustomerID: emission.customerID.String(), PlanID: emission.planID})
	if err != nil {
		return false, fmt.Errorf("marshal webhook payload: %w", err)
	}
	if h.emitter != nil {
		if err := h.emitter.EmitTx(ctx, tx, emission.customerID, emission.eventType, payload); err != nil {
			return false, fmt.Errorf("webhook emit tx: %w", err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("commit: %w", err)
	}
	return true, nil
}

// pendingEmission carries the outbound-webhook call deferred until
// recordEventAndEmit's dedup insert confirms this delivery is not a loser of
// the dedup race (see Handle). A nil *pendingEmission means the dispatched
// handler has nothing to emit (no emitter configured, or the event type
// carries no lifecycle event).
type pendingEmission struct {
	eventType  string
	customerID uuid.UUID
	planID     string
}

func (h *Webhook) dispatch(ctx context.Context, event *stripeEvent) (*pendingEmission, error) {
	switch event.Type {
	case "customer.subscription.created", "customer.subscription.updated":
		return h.handleSubscriptionUpsert(ctx, event)
	case "customer.subscription.deleted":
		return h.handleSubscriptionDeleted(ctx, event)
	case "checkout.session.completed":
		return nil, h.handleCheckoutSessionCompleted(ctx, event)
	case "customer.created":
		return nil, h.handleCustomerCreated(ctx, event)
	default:
		log.Info().Str("event_type", event.Type).Msg("webhook event ignored (no handler)")
		return nil, nil
	}
}

func (h *Webhook) handleSubscriptionUpsert(ctx context.Context, event *stripeEvent) (*pendingEmission, error) {
	var obj struct {
		Customer string `json:"customer"`
		Items    struct {
			Data []struct {
				Price struct {
					ID string `json:"id"`
				} `json:"price"`
			} `json:"data"`
		} `json:"items"`
	}
	if err := json.Unmarshal(event.Data.Object, &obj); err != nil {
		return nil, err
	}
	if obj.Customer == "" || len(obj.Items.Data) == 0 {
		return nil, errors.New("subscription missing customer or items")
	}

	priceID := obj.Items.Data[0].Price.ID
	var planID string
	if err := h.db.QueryRow(ctx, `SELECT id FROM plans WHERE stripe_price_id = $1`, priceID).Scan(&planID); err != nil {
		return nil, err
	}

	tag, err := h.db.Exec(ctx, `
		UPDATE customers SET plan_id = $1, updated_at = NOW()
		WHERE stripe_customer_id = $2
	`, planID, obj.Customer)
	if err != nil {
		return nil, err
	}

	// Look up the internal customer UUID once for both cache invalidation
	// (CLAUDE.md invariant #7: plan changes bypass the revocation path, so the
	// cache must be flushed explicitly) and outbound webhook emission. Gated on
	// RowsAffected() > 0: if the UPDATE above matched no row (customer not yet
	// linked to this stripe_customer_id), a customer link racing in between this
	// UPDATE and the SELECT below could otherwise make the SELECT succeed and
	// emit subscription.updated for a plan_id this handler never actually set.
	var emission *pendingEmission
	if tag.RowsAffected() > 0 && (h.cache != nil || h.emitter != nil) {
		var customerID uuid.UUID
		if err := h.db.QueryRow(ctx, `SELECT id FROM customers WHERE stripe_customer_id = $1 LIMIT 1`, obj.Customer).Scan(&customerID); err == nil {
			if h.cache != nil {
				h.invalidateCustomerCache(ctx, customerID.String())
			}
			if h.emitter != nil {
				emission = &pendingEmission{eventType: events.SubscriptionUpdated, customerID: customerID, planID: planID}
			}
		} else if !errors.Is(err, pgx.ErrNoRows) {
			log.Warn().Err(err).Str("stripe_customer_id", obj.Customer).Msg("customer lookup failed after subscription upsert")
		}
	}
	return emission, nil
}

func (h *Webhook) handleSubscriptionDeleted(ctx context.Context, event *stripeEvent) (*pendingEmission, error) {
	var obj struct {
		Customer string `json:"customer"`
		Status   string `json:"status"`
	}
	if err := json.Unmarshal(event.Data.Object, &obj); err != nil {
		return nil, err
	}
	// Only downgrade if the subscription is actually canceled. A retried deleted
	// event for a customer who has since re-subscribed will have a different active subscription.
	if obj.Customer == "" {
		log.Info().Msg("customer.subscription.deleted: missing customer field; skipping")
		return nil, nil
	}
	if obj.Status != "canceled" {
		return nil, nil
	}

	tag, err := h.db.Exec(ctx, `
		UPDATE customers SET plan_id = 'free', updated_at = NOW()
		WHERE stripe_customer_id = $1
	`, obj.Customer)
	if err != nil {
		return nil, err
	}

	// Same RowsAffected() gate as handleSubscriptionUpsert: skip the extra SELECT
	// (and never emit) when the UPDATE above matched no row, so a customer link
	// racing in between this UPDATE and the SELECT below can't make us emit
	// subscription.deleted for a downgrade this handler never actually applied.
	var emission *pendingEmission
	if tag.RowsAffected() > 0 && (h.cache != nil || h.emitter != nil) {
		var customerID uuid.UUID
		if err := h.db.QueryRow(ctx, `SELECT id FROM customers WHERE stripe_customer_id = $1 LIMIT 1`, obj.Customer).Scan(&customerID); err == nil {
			if h.cache != nil {
				h.invalidateCustomerCache(ctx, customerID.String())
			}
			if h.emitter != nil {
				emission = &pendingEmission{eventType: events.SubscriptionDeleted, customerID: customerID, planID: "free"}
			}
		} else if !errors.Is(err, pgx.ErrNoRows) {
			log.Warn().Err(err).Str("stripe_customer_id", obj.Customer).Msg("customer lookup failed after subscription deletion")
		}
	}
	return emission, nil
}

// handleCheckoutSessionCompleted links the Stripe customer to our customer row via
// client_reference_id (which was set to the customer UUID at session creation time).
func (h *Webhook) handleCheckoutSessionCompleted(ctx context.Context, event *stripeEvent) error {
	var obj struct {
		ClientReferenceID string `json:"client_reference_id"`
		Customer          string `json:"customer"`
	}
	if err := json.Unmarshal(event.Data.Object, &obj); err != nil {
		return err
	}
	if obj.ClientReferenceID == "" || obj.Customer == "" {
		log.Info().Msg("checkout.session.completed: missing client_reference_id or customer; skipping link")
		return nil
	}

	tag, err := h.db.Exec(ctx, `
		UPDATE customers SET stripe_customer_id = $1, updated_at = NOW()
		WHERE id = $2 AND (stripe_customer_id IS NULL OR stripe_customer_id = $1)
	`, obj.Customer, obj.ClientReferenceID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		// Already linked to a different customer or id unknown; safe to skip.
		log.Info().Str("customer_id", obj.ClientReferenceID).Msg("checkout.session.completed: no customer row updated; skipping cache invalidation")
		return nil
	}

	// Flush the auth cache so the customer's new stripe_customer_id is seen immediately
	// by the flusher (which filters on stripe_customer_id IS NOT NULL).
	if h.cache != nil {
		h.invalidateCustomerCache(ctx, obj.ClientReferenceID)
	}
	return nil
}

// handleCustomerCreated links the Stripe customer to our customer row via email.
// This event fires before checkout.session.completed so it provides an early link;
// both handlers are idempotent (the WHERE guard prevents overwriting with a different ID).
// Note: customer.created fires even when checkout is abandoned. An abandoned customer
// will have stripe_customer_id set but remain on the free plan and hold no subscription.
// The billing portal guard (LookupStripeCustomerID) checks for the ID but NOT for an
// active subscription, so such customers CAN open the portal — this is intentional:
// the portal lets them complete setup or view an empty subscription list.
func (h *Webhook) handleCustomerCreated(ctx context.Context, event *stripeEvent) error {
	var obj struct {
		ID    string `json:"id"`
		Email string `json:"email"`
	}
	if err := json.Unmarshal(event.Data.Object, &obj); err != nil {
		return err
	}
	if obj.ID == "" || obj.Email == "" {
		log.Info().Msg("customer.created: missing id or email; skipping link")
		return nil
	}

	// LOWER() on both sides guards against case-variation between the email stored
	// in our DB (from the OAuth provider) and the email Stripe recorded at checkout.
	var customerID string
	if err := h.db.QueryRow(ctx, `SELECT id FROM customers WHERE LOWER(email) = LOWER($1)`, obj.Email).Scan(&customerID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// Customer not in our DB yet (e.g. Stripe test event for unknown email). Safe to skip.
			log.Info().Str("email", obj.Email).Msg("customer.created: no matching customer row; skipping link")
			return nil
		}
		return err
	}

	// Use the UUID fetched above rather than email in the WHERE clause: avoids a
	// TOCTOU window where the email could change between the SELECT and the UPDATE.
	tag, err := h.db.Exec(ctx, `
		UPDATE customers SET stripe_customer_id = $1, updated_at = NOW()
		WHERE id = $2 AND (stripe_customer_id IS NULL OR stripe_customer_id = $1)
	`, obj.ID, customerID)
	if err != nil {
		return err
	}
	// Only invalidate when the row actually changed, consistent with handleCheckoutSessionCompleted.
	if tag.RowsAffected() > 0 && h.cache != nil {
		h.invalidateCustomerCache(ctx, customerID)
	}
	return nil
}

// invalidateCustomerCache deletes auth:<prefix> Redis entries for all active API keys
// belonging to customerID. This ensures plan or stripe_customer_id changes take effect
// immediately rather than waiting for the 60 s TTL (CLAUDE.md invariant #7).
// Keys are flushed in batches of 100 to bound Redis command payload size.
// Result set is bounded at 1000 rows; customers with more active keys will have the
// remainder flushed by TTL expiry within 60 s (same as before this feature existed).
// This runs synchronously in the webhook handler. The webhook HTTP server timeout
// bounds total latency; the Stripe retry window (5 min) means a slow cache flush
// that delays the 200 response will simply cause a harmless retry.
// Best-effort: errors are logged but not returned.
func (h *Webhook) invalidateCustomerCache(ctx context.Context, customerID string) {
	if h.cache == nil {
		return
	}
	if err := ctx.Err(); err != nil {
		log.Warn().Err(err).Str("customer_id", customerID).Msg("cache invalidation: context already canceled, skipping")
		return
	}
	// maxCacheInvalidationPrefixes caps how many Redis keys are evicted per webhook
	// event. Customers with more active keys have the remainder flushed by TTL (≤60s).
	const maxCacheInvalidationPrefixes = 1000
	// ORDER BY prefix gives a deterministic subset when LIMIT is hit,
	// so repeated invalidations cover the same keys rather than an arbitrary shard.
	rows, err := h.db.Query(ctx, `SELECT prefix FROM api_keys WHERE customer_id = $1 AND revoked_at IS NULL ORDER BY prefix LIMIT $2`, customerID, maxCacheInvalidationPrefixes)
	if err != nil {
		log.Warn().Err(err).Str("customer_id", customerID).Msg("cache invalidation: prefix query failed")
		return
	}
	defer rows.Close()

	const batchSize = 100
	batch := make([]string, 0, batchSize)
	flush := func() {
		if len(batch) == 0 {
			return
		}
		if err := h.cache.Del(ctx, batch...); err != nil {
			log.Warn().Err(err).Strs("keys", batch).Msg("cache invalidation: redis DEL failed")
		}
		batch = batch[:0]
	}

	for rows.Next() {
		var prefix string
		if err := rows.Scan(&prefix); err != nil {
			log.Warn().Err(err).Str("customer_id", customerID).Msg("cache invalidation: prefix scan failed for row")
			continue
		}
		batch = append(batch, "auth:"+prefix)
		if len(batch) >= batchSize {
			flush()
		}
	}
	if rows.Err() != nil {
		log.Warn().Err(rows.Err()).Str("customer_id", customerID).Msg("cache invalidation: prefix scan error")
	}
	flush()
}
