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
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/zerolog/log"
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
	stuckDeliveryAge = 2 * deliveryTimeout
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
// of a customer. The background delivery worker runs until ctx is canceled.
type Emitter struct {
	db     *pgxpool.Pool
	client *http.Client
}

// NewEmitter constructs an Emitter and starts the background delivery worker.
// Returns nil when db is nil so the caller need not nil-check before calling Emit.
func NewEmitter(ctx context.Context, db *pgxpool.Pool) *Emitter {
	if db == nil {
		return nil
	}
	e := &Emitter{
		db:     db,
		client: &http.Client{Timeout: deliveryTimeout},
	}
	go e.run(ctx)
	return e
}

// Emit fans an event out to all active endpoints registered by customerID by
// inserting one webhook_deliveries row per endpoint in a single INSERT…SELECT.
// A nil Emitter or a customer with no active endpoints is a safe no-op.
func (e *Emitter) Emit(ctx context.Context, customerID uuid.UUID, eventType string, payload []byte) error {
	if e == nil {
		return nil
	}
	eventID := uuid.New().String()
	_, err := e.db.Exec(ctx, `
		INSERT INTO webhook_deliveries (event_id, endpoint_id, payload)
		SELECT $1, we.id, $2::jsonb
		FROM webhook_endpoints we
		WHERE we.customer_id = $3 AND we.active = TRUE
	`, eventID, string(payload), customerID)
	return err
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
	id       int64
	eventID  string
	payload  []byte
	attempts int
	url      string
	secret   []byte
}

func (e *Emitter) processDue(ctx context.Context) error {
	// Recover rows that were claimed ('delivering') before a crash.
	// stuckDeliveryAge (2 × deliveryTimeout = 20 s) is safely beyond any
	// in-progress HTTP call, so no live delivery is mistakenly reset.
	_, _ = e.db.Exec(ctx, `
		UPDATE webhook_deliveries
		SET status = 'pending'
		WHERE status = 'delivering'
		  AND next_attempt_at < NOW() - ($1 * INTERVAL '1 second')
	`, stuckDeliveryAge.Seconds())

	tx, err := e.db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("webhookout: begin tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	// SELECT … FOR UPDATE SKIP LOCKED claims due rows; concurrent worker instances
	// (multi-replica deployments) skip locked rows rather than blocking.
	rows, err := tx.Query(ctx, `
		SELECT d.id, d.event_id, d.payload, d.attempts, we.url, we.secret
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

	var due []pendingRow
	for rows.Next() {
		var r pendingRow
		if err := rows.Scan(&r.id, &r.eventID, &r.payload, &r.attempts, &r.url, &r.secret); err != nil {
			rows.Close()
			return fmt.Errorf("webhookout: scan: %w", err)
		}
		due = append(due, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("webhookout: rows: %w", err)
	}

	if len(due) == 0 {
		return nil
	}

	// Mark claimed rows 'delivering' inside the same transaction.
	// Commit is fast (no I/O); locks are held only for DB round-trips.
	ids := make([]int64, len(due))
	for i, r := range due {
		ids[i] = r.id
	}
	if _, err := tx.Exec(ctx, `
		UPDATE webhook_deliveries SET status = 'delivering' WHERE id = ANY($1)
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

	ts := strconv.FormatInt(time.Now().Unix(), 10)
	sig := Sign(r.secret, ts, r.payload)

	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, r.url, bytes.NewReader(r.payload))
	if err != nil {
		// URL is permanently malformed; dead-letter immediately.
		e.markDeadLetter(ctx, r.id, r.attempts+1, 0)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Crucible-Timestamp", ts)
	req.Header.Set("X-Crucible-Signature", "t="+ts+",v1="+sig)
	req.Header.Set("X-Webhook-Event-ID", r.eventID)

	resp, doErr := e.client.Do(req)
	if doErr == nil {
		resp.Body.Close()
	}

	newAttempts := r.attempts + 1
	if doErr == nil && resp.StatusCode >= 200 && resp.StatusCode < 300 {
		e.markDelivered(ctx, r.id, newAttempts, resp.StatusCode)
		return
	}

	var statusCode int
	if doErr == nil {
		statusCode = resp.StatusCode
	}

	if newAttempts >= maxAttempts {
		e.markDeadLetter(ctx, r.id, newAttempts, statusCode)
		return
	}
	e.scheduleRetry(ctx, r.id, newAttempts, statusCode)
}

func (e *Emitter) markDelivered(ctx context.Context, id int64, attempts, statusCode int) {
	if e.db == nil {
		return
	}
	_, err := e.db.Exec(ctx, `
		UPDATE webhook_deliveries
		SET status = 'delivered', attempts = $2, last_response_code = $3
		WHERE id = $1
	`, id, attempts, statusCode)
	if err != nil {
		log.Warn().Err(err).Int64("delivery_id", id).Msg("webhookout: mark delivered failed")
	}
}

func (e *Emitter) markDeadLetter(ctx context.Context, id int64, attempts, statusCode int) {
	if e.db == nil {
		return
	}
	var scPtr *int
	if statusCode != 0 {
		scPtr = &statusCode
	}
	_, err := e.db.Exec(ctx, `
		UPDATE webhook_deliveries
		SET status = 'dead_letter', attempts = $2, last_response_code = $3
		WHERE id = $1
	`, id, attempts, scPtr)
	if err != nil {
		log.Warn().Err(err).Int64("delivery_id", id).Msg("webhookout: mark dead_letter failed")
	}
}

func (e *Emitter) scheduleRetry(ctx context.Context, id int64, newAttempts, statusCode int) {
	if e.db == nil {
		return
	}
	// backoffSchedule index is the 0-based attempt count that JUST failed.
	// newAttempts = failedAttempts + 1, so the index is newAttempts - 1.
	idx := newAttempts - 1
	if idx >= len(backoffSchedule) {
		idx = len(backoffSchedule) - 1
	}
	nextAt := time.Now().Add(backoffSchedule[idx])

	var scPtr *int
	if statusCode != 0 {
		scPtr = &statusCode
	}
	_, err := e.db.Exec(ctx, `
		UPDATE webhook_deliveries
		SET status = 'pending', attempts = $2, next_attempt_at = $3, last_response_code = $4
		WHERE id = $1
	`, id, newAttempts, nextAt, scPtr)
	if err != nil {
		log.Warn().Err(err).Int64("delivery_id", id).Msg("webhookout: schedule retry failed")
	}
}
