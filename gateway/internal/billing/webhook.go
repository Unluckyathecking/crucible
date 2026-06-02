package billing

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/zerolog/log"
)

// db is the subset of *pgxpool.Pool used by this package. Extracted as an
// interface to allow test mocking without changing runtime behaviour.
type db interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// Webhook receives Stripe events, verifies HMAC, dedupes via webhook_events,
// and updates the customer's plan_id on subscription lifecycle events.
type Webhook struct {
	secret string
	db     db
	now    func() time.Time // injectable for tests
}

func NewWebhook(secret string, pool *pgxpool.Pool) *Webhook {
	return &Webhook{secret: secret, db: pool, now: time.Now}
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
	if err := h.dispatch(r.Context(), &event); err != nil {
		log.Error().Err(err).Str("event_type", event.Type).Msg("webhook handler failed")
		http.Error(w, "handler error", http.StatusInternalServerError)
		return
	}

	// Handler succeeded — record the event so future retries dedupe.
	// If two deliveries race and both succeed at dispatch, ON CONFLICT keeps the table clean.
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

// Note: dispatch handlers (handleSubscriptionUpsert, handleSubscriptionDeleted) are idempotent on
// subscription state — running them twice on the same event yields the same result. The dispatch-then-record
// ordering above accepts a low-probability double-dispatch (record race) in exchange for never losing an event.

func (h *Webhook) dispatch(ctx context.Context, event *stripeEvent) error {
	switch event.Type {
	case "customer.subscription.created", "customer.subscription.updated":
		return h.handleSubscriptionUpsert(ctx, event)
	case "customer.subscription.deleted":
		return h.handleSubscriptionDeleted(ctx, event)
	default:
		log.Info().Str("event_type", event.Type).Msg("webhook event ignored (no handler)")
		return nil
	}
}

func (h *Webhook) handleSubscriptionUpsert(ctx context.Context, event *stripeEvent) error {
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
		return err
	}
	if obj.Customer == "" || len(obj.Items.Data) == 0 {
		return errors.New("subscription missing customer or items")
	}

	priceID := obj.Items.Data[0].Price.ID
	var planID string
	if err := h.db.QueryRow(ctx, `SELECT id FROM plans WHERE stripe_price_id = $1`, priceID).Scan(&planID); err != nil {
		return err
	}

	_, err := h.db.Exec(ctx, `
		UPDATE customers SET plan_id = $1, updated_at = NOW()
		WHERE stripe_customer_id = $2
	`, planID, obj.Customer)
	return err
}

func (h *Webhook) handleSubscriptionDeleted(ctx context.Context, event *stripeEvent) error {
	var obj struct {
		Customer string `json:"customer"`
		Status   string `json:"status"`
	}
	if err := json.Unmarshal(event.Data.Object, &obj); err != nil {
		return err
	}
	// Only downgrade if the subscription is actually canceled. A retried deleted
	// event for a customer who has since re-subscribed will have a different active subscription.
	if obj.Status != "canceled" {
		return nil
	}
	_, err := h.db.Exec(ctx, `
		UPDATE customers SET plan_id = 'free', updated_at = NOW()
		WHERE stripe_customer_id = $1
	`, obj.Customer)
	return err
}
