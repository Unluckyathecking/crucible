// Package webhookout delivers outbound signed HTTP POST events to customer-registered
// webhook endpoints.
//
// The delivery worker runs in a background goroutine started by NewEmitter and
// claims rows via SELECT … FOR UPDATE SKIP LOCKED. Signing mirrors the inbound
// Stripe verifier: the payload is "timestamp.body" and the hex HMAC-SHA256 digest
// is carried in X-Crucible-Signature. Each delivery also carries an
// X-Webhook-Event-ID idempotency header so customers can deduplicate at-least-once
// deliveries. After maxAttempts the row transitions to dead_letter.
//
// A nil *Emitter is always a safe no-op — all exported methods nil-check the
// receiver, matching the optional-Deps pattern used by Checkout, ErrorRecorder, DB.
package webhookout

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/zerolog/log"

	"github.com/Unluckyathecking/crucible/gateway/internal/egress"
	"github.com/Unluckyathecking/crucible/gateway/internal/events"
)

const (
	// maxAttempts is the delivery attempt cap before a row is dead-lettered.
	maxAttempts = 7
	// deliveryTimeout caps each individual HTTP POST to a customer endpoint.
	deliveryTimeout = 10 * time.Second
	// workerPeriod is the interval between worker ticks.
	workerPeriod = 5 * time.Second
	// claimPageSize limits how many pending deliveries are claimed per tick.
	claimPageSize = 50
	// stuckDeliveryAge is the threshold past which a 'delivering' row is
	// considered abandoned (process crash) and reset to 'pending'.
	// Must comfortably exceed deliveryTimeout + workerPeriod + scheduling jitter
	// so that in-flight deliveries are never reclaimed as stuck.
	stuckDeliveryAge = 60 * time.Second
	// dbWriteTimeout caps DB state-update calls that happen after HTTP delivery.
	// The worker context may already be cancelled at shutdown; using a fresh
	// background-derived context ensures we still record the delivery outcome.
	dbWriteTimeout = 5 * time.Second
)

// backoffSchedule maps attempt index (0-based) to the delay before the next attempt.
// After maxAttempts the row is dead-lettered, not rescheduled.
var backoffSchedule = []time.Duration{
	5 * time.Second,
	30 * time.Second,
	5 * time.Minute,
	30 * time.Minute,
	1 * time.Hour,
	2 * time.Hour,
	2 * time.Hour,
}

// Emitter queues outbound events into webhook_deliveries for all active endpoints
// of a customer. The background delivery worker runs until Stop is called or the
// parent context passed to NewEmitter is cancelled.
type Emitter struct {
	db     *pgxpool.Pool
	client *http.Client
	cancel context.CancelFunc
}

// NewEmitter constructs an Emitter and starts the background delivery worker.
// Returns nil when db is nil so the caller need not nil-check before calling Emit.
func NewEmitter(ctx context.Context, db *pgxpool.Pool) *Emitter {
	if db == nil {
		return nil
	}
	workerCtx, cancel := context.WithCancel(ctx)
	e := &Emitter{
		db:     db,
		client: &http.Client{Timeout: deliveryTimeout, Transport: egress.GuardedTransport()},
		cancel: cancel,
	}
	go e.run(workerCtx)
	return e
}

// Stop signals the background delivery worker to exit. Safe to call on a nil Emitter.
func (e *Emitter) Stop() {
	if e == nil {
		return
	}
	e.cancel()
}

// Emit fans an event out to all active endpoints registered by customerID that
// are subscribed to eventType, inserting one webhook_deliveries row per matching
// endpoint in a single INSERT…SELECT. An endpoint whose subscribed_events is
// NULL (the default — see 0017_webhook_subscriptions.sql) is subscribed to
// every event type, preserving the pre-0017 all-or-nothing fan-out for rows
// that never set an explicit subscription. A nil Emitter or a customer with no
// matching endpoints is a safe no-op. payload must be valid JSON; an error is
// returned otherwise.
func (e *Emitter) Emit(ctx context.Context, customerID uuid.UUID, eventType string, payload []byte) error {
	if e == nil {
		return nil
	}
	if !json.Valid(payload) {
		return fmt.Errorf("webhookout: emit: payload is not valid JSON")
	}
	eventID := uuid.New().String()
	_, err := e.db.Exec(ctx, `
		INSERT INTO webhook_deliveries (event_id, event_type, endpoint_id, payload)
		SELECT $1, $2, we.id, $3::jsonb
		FROM webhook_endpoints we
		WHERE we.customer_id = $4 AND we.active = TRUE
		  AND (we.subscribed_events IS NULL OR $2 = ANY(we.subscribed_events))
	`, eventID, eventType, string(payload), customerID)
	return err
}

// ValidateSubscribedEvents reports an error naming the first entry in
// eventTypes that is not a member of events.AllEventTypes. A nil or empty
// slice — meaning "subscribed to every event" — is always valid. Non-Go
// registration paths (e.g. the dashboard's TypeScript API routes) cannot
// import this package directly and must keep their own event-type list in
// sync with events.AllEventTypes; this is the Go-side check for any call path
// that does have access to it.
func ValidateSubscribedEvents(eventTypes []string) error {
	for _, t := range eventTypes {
		if !events.IsValidEventType(t) {
			return fmt.Errorf("webhookout: unknown event type %q", t)
		}
	}
	return nil
}

// GenerateSecret returns 32 cryptographically random bytes for use as an endpoint
// signing secret. Callers encode it as hex for display; it is stored raw as BYTEA
// and never returned after the creation response.
func GenerateSecret() ([]byte, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return nil, fmt.Errorf("webhookout: generate secret: %w", err)
	}
	return b, nil
}

// Sign computes HMAC-SHA256 over "timestamp.body" using secret and returns the
// lowercase hex digest. Mirrors the inbound Stripe verifier's payload construction
// so the customer-side verification algorithm is symmetric.
func Sign(secret []byte, timestamp string, body []byte) string {
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write([]byte(timestamp))
	_, _ = mac.Write([]byte("."))
	_, _ = mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

func (e *Emitter) run(ctx context.Context) {
	ticker := time.NewTicker(workerPeriod)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := e.processDue(ctx); err != nil {
				log.Warn().Err(err).Msg("webhookout: worker tick failed")
			}
		}
	}
}

type pendingRow struct {
	id        int64
	eventID   string
	eventType string
	payload   []byte
	attempts  int
	url       string
	secret    []byte
}

func (e *Emitter) processDue(ctx context.Context) error {
	// Recover rows abandoned by a crashed process: 'delivering' rows where
	// claimed_at (set when the row was locked) is older than stuckDeliveryAge.
	// next_attempt_at is the scheduled retry time and must NOT be used here —
	// it reflects when delivery should run, not how long the row has been in-flight.
	_, _ = e.db.Exec(ctx, `
		UPDATE webhook_deliveries
		SET status = 'pending', claimed_at = NULL
		WHERE status = 'delivering'
		  AND claimed_at IS NOT NULL
		  AND claimed_at < NOW() - ($1 * INTERVAL '1 second')
	`, stuckDeliveryAge.Seconds())

	tx, err := e.db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("webhookout: begin tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	// SELECT … FOR UPDATE SKIP LOCKED claims due rows; concurrent worker instances
	// (multi-replica deployments) skip locked rows rather than blocking.
	rows, err := tx.Query(ctx, `
		SELECT d.id, d.event_id, d.event_type, d.payload, d.attempts, we.url, we.secret
		FROM webhook_deliveries d
		JOIN webhook_endpoints we ON we.id = d.endpoint_id
		WHERE d.status = 'pending'
		  AND d.next_attempt_at <= NOW()
		  AND we.active = TRUE
		ORDER BY d.next_attempt_at ASC
		LIMIT $1
		FOR UPDATE OF d SKIP LOCKED
	`, claimPageSize)
	if err != nil {
		return fmt.Errorf("webhookout: claim: %w", err)
	}
	defer rows.Close()

	var due []pendingRow
	for rows.Next() {
		var r pendingRow
		if err := rows.Scan(&r.id, &r.eventID, &r.eventType, &r.payload, &r.attempts, &r.url, &r.secret); err != nil {
			return fmt.Errorf("webhookout: scan: %w", err)
		}
		due = append(due, r)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("webhookout: rows: %w", err)
	}

	if len(due) == 0 {
		return nil
	}

	// Mark claimed rows 'delivering' inside the same transaction, recording when
	// each row was claimed so crash-recovery can identify stuck rows correctly.
	ids := make([]int64, len(due))
	for i, r := range due {
		ids[i] = r.id
	}
	if _, err := tx.Exec(ctx, `
		UPDATE webhook_deliveries SET status = 'delivering', claimed_at = NOW() WHERE id = ANY($1)
	`, ids); err != nil {
		return fmt.Errorf("webhookout: mark delivering: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("webhookout: commit: %w", err)
	}

	// Deliver outside the transaction so the DB connection is free during HTTP I/O.
	for i := range due {
		e.deliver(ctx, due[i])
	}
	return nil
}

func (e *Emitter) deliver(ctx context.Context, r pendingRow) {
	reqCtx, cancel := context.WithTimeout(ctx, deliveryTimeout)
	defer cancel()

	ts := strconv.FormatInt(time.Now().UTC().Unix(), 10)
	sig := Sign(r.secret, ts, r.payload)

	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, r.url, bytes.NewReader(r.payload))
	if err != nil {
		// URL is permanently malformed; dead-letter immediately, no HTTP status.
		e.markDeadLetter(r.id, r.attempts+1, nil)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Crucible-Timestamp", ts)
	req.Header.Set("X-Crucible-Signature", "t="+ts+",v1="+sig)
	req.Header.Set("X-Webhook-Event-ID", r.eventID)
	req.Header.Set("X-Webhook-Event-Type", r.eventType)

	resp, doErr := e.client.Do(req)
	if doErr == nil {
		// Drain before close so the transport can reuse the TCP connection.
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}

	newAttempts := r.attempts + 1
	if doErr == nil && resp.StatusCode >= 200 && resp.StatusCode < 300 {
		e.markDelivered(r.id, newAttempts, resp.StatusCode)
		return
	}

	// nil means no HTTP response (network error); non-nil is the actual status code.
	var statusCode *int
	if doErr == nil {
		sc := resp.StatusCode
		statusCode = &sc
	}

	if newAttempts >= maxAttempts {
		e.markDeadLetter(r.id, newAttempts, statusCode)
		return
	}
	e.scheduleRetry(r.id, newAttempts, statusCode)
}

// markDelivered, markDeadLetter, scheduleRetry use a fresh background-derived context
// so a shutting-down worker context does not prevent recording the delivery outcome.

func (e *Emitter) markDelivered(id int64, attempts, statusCode int) {
	if e.db == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), dbWriteTimeout)
	defer cancel()
	_, err := e.db.Exec(ctx, `
		UPDATE webhook_deliveries
		SET status = 'delivered', attempts = $2, last_response_code = $3, claimed_at = NULL
		WHERE id = $1
	`, id, attempts, statusCode)
	if err != nil {
		log.Warn().Err(err).Int64("delivery_id", id).Msg("webhookout: mark delivered failed")
	}
}

func (e *Emitter) markDeadLetter(id int64, attempts int, statusCode *int) {
	if e.db == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), dbWriteTimeout)
	defer cancel()
	_, err := e.db.Exec(ctx, `
		UPDATE webhook_deliveries
		SET status = 'dead_letter', attempts = $2, last_response_code = $3, claimed_at = NULL
		WHERE id = $1
	`, id, attempts, statusCode)
	if err != nil {
		log.Warn().Err(err).Int64("delivery_id", id).Msg("webhookout: mark dead_letter failed")
	}
}

func (e *Emitter) scheduleRetry(id int64, newAttempts int, statusCode *int) {
	if e.db == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), dbWriteTimeout)
	defer cancel()
	// backoffSchedule index is the 0-based attempt count that JUST failed.
	// newAttempts = failedAttempts + 1, so the index is newAttempts - 1.
	idx := newAttempts - 1
	if idx >= len(backoffSchedule) {
		idx = len(backoffSchedule) - 1
	}
	nextAt := time.Now().Add(backoffSchedule[idx])
	_, err := e.db.Exec(ctx, `
		UPDATE webhook_deliveries
		SET status = 'pending', attempts = $2, next_attempt_at = $3, last_response_code = $4, claimed_at = NULL
		WHERE id = $1
	`, id, newAttempts, nextAt, statusCode)
	if err != nil {
		log.Warn().Err(err).Int64("delivery_id", id).Msg("webhookout: schedule retry failed")
	}
}
