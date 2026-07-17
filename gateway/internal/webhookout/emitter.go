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
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/zerolog/log"

	"github.com/Unluckyathecking/crucible/gateway/internal/channelsig"
	"github.com/Unluckyathecking/crucible/gateway/internal/egress"
	"github.com/Unluckyathecking/crucible/gateway/internal/events"
	"github.com/Unluckyathecking/crucible/gateway/internal/observability"
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

	// webhookFairClaimOverfetchFactor/webhookFairClaimMaxCandidates bound the
	// candidate window claimDue scans when the Emitter's maxInflightPerCustomer
	// is > 0: wide enough that a customer other than the one occupying the
	// head of the FIFO queue is normally found within a single claim tick, but
	// capped so one tick can never lock an unbounded slice of the backlog. A
	// row outside this window that's eligible only waits one extra tick, not
	// indefinitely — the window is reconsidered fresh (from the same
	// oldest-first order) every tick. Mirrors jobs.fairClaimOverfetchFactor/
	// fairClaimMaxCandidates.
	webhookFairClaimOverfetchFactor = 20
	webhookFairClaimMaxCandidates   = 2000
)

// webhookFairClaimAdvisoryLockKey is an arbitrary fixed key for the
// session-scoped Postgres advisory lock claimDue takes for the whole gateway
// fleet whenever maxInflightPerCustomer > 0 — see its use in claimDue for why
// the per-customer cap needs one. Deliberately distinct from
// jobs.fairClaimAdvisoryLockKey (0x63727563_69626c65, "crucible" in hex) so
// the two independent fair-claim paths can never contend on the same
// advisory lock key.
const webhookFairClaimAdvisoryLockKey int64 = 0x77656268_6f6f6b73 // "webhooks" in hex

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
	// maxInflightPerCustomer bounds how many 'delivering' rows a single
	// customer may occupy at once across processDue's claim (see claimDue).
	// <= 0 (the zero value, and the default when NewEmitter is called
	// without WithMaxInflightPerCustomer) keeps claimDue's original
	// single-query global-FIFO SELECT byte-for-byte unchanged.
	maxInflightPerCustomer int
}

// EmitterOption configures optional Emitter behavior at construction time.
// Added as variadic options (rather than new required NewEmitter parameters)
// so every existing call site keeps compiling unchanged.
type EmitterOption func(*Emitter)

// WithMaxInflightPerCustomer sets the per-customer in-flight delivery cap
// used by claimDue's fairness path — see Emitter.maxInflightPerCustomer's
// doc comment. n <= 0 (also the default when this option is omitted)
// disables the cap.
func WithMaxInflightPerCustomer(n int) EmitterOption {
	return func(e *Emitter) {
		e.maxInflightPerCustomer = n
	}
}

// NewEmitter constructs an Emitter and starts the background delivery worker.
// Returns nil when db is nil so the caller need not nil-check before calling Emit.
func NewEmitter(ctx context.Context, db *pgxpool.Pool, opts ...EmitterOption) *Emitter {
	if db == nil {
		return nil
	}
	workerCtx, cancel := context.WithCancel(ctx)
	e := &Emitter{
		db:     db,
		client: &http.Client{Timeout: deliveryTimeout, Transport: egress.GuardedTransport()},
		cancel: cancel,
	}
	for _, opt := range opts {
		opt(e)
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
// so the customer-side verification algorithm is symmetric. Delegates to the shared
// channelsig primitive; kept as a wrapper so existing callers and tests are unaffected.
func Sign(secret []byte, timestamp string, body []byte) string {
	return channelsig.Sign(secret, timestamp, body)
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
	// customerID is only populated by claimDue's fairness-enabled path
	// (maxInflightPerCustomer > 0); the default disabled path never selects
	// or needs it.
	customerID uuid.UUID
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

	due, err := e.claimDue(ctx, tx)
	if err != nil {
		return err
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

// claimDue selects due rows to deliver within tx via SELECT … FOR UPDATE
// SKIP LOCKED — concurrent worker instances (multi-replica deployments) skip
// locked rows rather than blocking. The subscribed_events check is
// re-evaluated here (not just at Emit-time) so a customer narrowing an
// endpoint's subscription after a row was already queued stops that row
// from being delivered too, instead of only affecting events emitted
// afterward.
//
// e.maxInflightPerCustomer <= 0 (the zero-value default) runs the original
// single-query global-FIFO SELECT unchanged — every prior release's exact
// behaviour. A positive value switches to a fairness-aware path mirroring
// jobs.Store.Claim: it over-fetches a bounded candidate window (see
// webhookFairClaimOverfetchFactor/webhookFairClaimMaxCandidates), then in Go
// skips any candidate whose customer already has maxInflightPerCustomer rows
// 'delivering' — counted fresh via deliveringCountsByCustomer, plus any
// already selected this same cycle — so a customer with a deep backlog can
// never starve another customer's delivery out of every claim tick. Rows
// skipped this way stay 'pending' and are simply reconsidered next tick;
// nothing is lost.
func (e *Emitter) claimDue(ctx context.Context, tx pgx.Tx) ([]pendingRow, error) {
	if e.maxInflightPerCustomer <= 0 {
		rows, err := tx.Query(ctx, `
			SELECT d.id, d.event_id, d.event_type, d.payload, d.attempts, we.url, we.secret
			FROM webhook_deliveries d
			JOIN webhook_endpoints we ON we.id = d.endpoint_id
			WHERE d.status = 'pending'
			  AND d.next_attempt_at <= NOW()
			  AND we.active = TRUE
			  AND (we.subscribed_events IS NULL OR d.event_type = ANY(we.subscribed_events))
			ORDER BY d.next_attempt_at ASC
			LIMIT $1
			FOR UPDATE OF d SKIP LOCKED
		`, claimPageSize)
		if err != nil {
			return nil, fmt.Errorf("webhookout: claim: %w", err)
		}
		defer rows.Close()

		var due []pendingRow
		for rows.Next() {
			var r pendingRow
			if err := rows.Scan(&r.id, &r.eventID, &r.eventType, &r.payload, &r.attempts, &r.url, &r.secret); err != nil {
				return nil, fmt.Errorf("webhookout: scan: %w", err)
			}
			due = append(due, r)
		}
		if err := rows.Err(); err != nil {
			return nil, fmt.Errorf("webhookout: rows: %w", err)
		}
		return due, nil
	}

	// deliveringCountsByCustomer's read and this transaction's later
	// mark-delivering write are a check-then-act sequence: two concurrent
	// gateway replicas both fair-claiming at once would each read the SAME
	// pre-commit delivering count for a customer at the cap and could
	// jointly push it past maxInflightPerCustomer, since read-committed
	// isolation doesn't let either see the other's uncommitted claims. A
	// session-scoped advisory lock (auto-released at commit/rollback, never
	// needs an explicit unlock) serializes exactly the fairness-enabled
	// claim path across every replica so only one instance is ever
	// mid-decision at a time. The plain FOR UPDATE SKIP LOCKED path above
	// (maxInflightPerCustomer <= 0, still the default) is untouched and
	// stays fully concurrent — this lock is the price of the per-customer
	// cap's correctness, not of claiming in general.
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, webhookFairClaimAdvisoryLockKey); err != nil {
		return nil, fmt.Errorf("webhookout: fair claim lock: %w", err)
	}

	candidateLimit := claimPageSize * webhookFairClaimOverfetchFactor
	if candidateLimit > webhookFairClaimMaxCandidates {
		candidateLimit = webhookFairClaimMaxCandidates
	}

	rows, err := tx.Query(ctx, `
		SELECT d.id, d.event_id, d.event_type, d.payload, d.attempts, we.url, we.secret, we.customer_id
		FROM webhook_deliveries d
		JOIN webhook_endpoints we ON we.id = d.endpoint_id
		WHERE d.status = 'pending'
		  AND d.next_attempt_at <= NOW()
		  AND we.active = TRUE
		  AND (we.subscribed_events IS NULL OR d.event_type = ANY(we.subscribed_events))
		ORDER BY d.next_attempt_at ASC
		LIMIT $1
		FOR UPDATE OF d SKIP LOCKED
	`, candidateLimit)
	if err != nil {
		return nil, fmt.Errorf("webhookout: claim: %w", err)
	}

	var candidates []pendingRow
	for rows.Next() {
		var r pendingRow
		if err := rows.Scan(&r.id, &r.eventID, &r.eventType, &r.payload, &r.attempts, &r.url, &r.secret, &r.customerID); err != nil {
			rows.Close()
			return nil, fmt.Errorf("webhookout: scan: %w", err)
		}
		candidates = append(candidates, r)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("webhookout: rows: %w", err)
	}
	rows.Close()

	if len(candidates) == 0 {
		return nil, nil
	}

	delivering, err := e.deliveringCountsByCustomer(ctx, tx)
	if err != nil {
		return nil, err
	}
	selected, throttled := applyWebhookInflightCap(candidates, delivering, e.maxInflightPerCustomer, claimPageSize)
	if throttled > 0 {
		observability.WebhookDeliveriesThrottledTotal.WithLabelValues("inflight_cap").Add(float64(throttled))
	}
	return selected, nil
}

// deliveringCountsByCustomer returns the number of 'delivering' rows per
// customer, read inside the same transaction as claimDue's candidate SELECT
// so the count reflects a consistent snapshot alongside the rows being
// considered for claim. Only customers with at least one delivering row
// appear in the map; applyWebhookInflightCap treats an absent entry as zero.
func (e *Emitter) deliveringCountsByCustomer(ctx context.Context, tx pgx.Tx) (map[uuid.UUID]int, error) {
	rows, err := tx.Query(ctx, `
		SELECT we.customer_id, COUNT(*)
		FROM webhook_deliveries d
		JOIN webhook_endpoints we ON we.id = d.endpoint_id
		WHERE d.status = 'delivering'
		GROUP BY we.customer_id
	`)
	if err != nil {
		return nil, fmt.Errorf("webhookout: delivering counts: %w", err)
	}
	defer rows.Close()

	counts := make(map[uuid.UUID]int)
	for rows.Next() {
		var (
			customerID uuid.UUID
			n          int
		)
		if err := rows.Scan(&customerID, &n); err != nil {
			return nil, fmt.Errorf("webhookout: delivering counts scan: %w", err)
		}
		counts[customerID] = n
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("webhookout: delivering counts rows: %w", err)
	}
	return counts, nil
}

// applyWebhookInflightCap walks candidates in their original
// oldest-next_attempt_at-first order and selects up to limit of them,
// skipping any whose customer already has maxInflightPerCustomer rows
// accounted for — either already 'delivering' (from the delivering
// snapshot) or already selected earlier in this same walk. Skipped
// candidates are simply left out of the returned slice; claimDue leaves
// them 'pending' for a later tick. throttled counts how many candidates
// were skipped for exactly that reason, so claimDue can report it via
// observability.WebhookDeliveriesThrottledTotal. Mirrors jobs.applyInflightCap.
func applyWebhookInflightCap(candidates []pendingRow, delivering map[uuid.UUID]int, maxInflightPerCustomer, limit int) (selected []pendingRow, throttled int) {
	selected = make([]pendingRow, 0, limit)
	inflight := make(map[uuid.UUID]int, len(delivering))
	for k, v := range delivering {
		inflight[k] = v
	}
	for _, r := range candidates {
		if len(selected) >= limit {
			break
		}
		if inflight[r.customerID] >= maxInflightPerCustomer {
			throttled++
			continue
		}
		inflight[r.customerID]++
		selected = append(selected, r)
	}
	return selected, throttled
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
	tag, err := e.db.Exec(ctx, `
		UPDATE webhook_deliveries AS d
		SET status = 'dead_letter', attempts = $2, last_response_code = $3, claimed_at = NULL
		FROM webhook_endpoints AS we
		WHERE d.id = $1 AND we.id = d.endpoint_id
		  AND (we.subscribed_events IS NULL OR d.event_type = ANY(we.subscribed_events))
	`, id, attempts, statusCode)
	if err == nil && tag.RowsAffected() == 0 {
		err = e.deleteUnsubscribedRow(ctx, id)
	}
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
	tag, err := e.db.Exec(ctx, `
		UPDATE webhook_deliveries AS d
		SET status = 'pending', attempts = $2, next_attempt_at = $3, last_response_code = $4, claimed_at = NULL
		FROM webhook_endpoints AS we
		WHERE d.id = $1 AND we.id = d.endpoint_id
		  AND (we.subscribed_events IS NULL OR d.event_type = ANY(we.subscribed_events))
	`, id, newAttempts, nextAt, statusCode)
	if err == nil && tag.RowsAffected() == 0 {
		err = e.deleteUnsubscribedRow(ctx, id)
	}
	if err != nil {
		log.Warn().Err(err).Int64("delivery_id", id).Msg("webhookout: schedule retry failed")
	}
}

// deleteUnsubscribedRow removes a webhook_deliveries row whose recording UPDATE
// (scheduleRetry/markDeadLetter) matched zero rows because the endpoint's
// subscription no longer includes this row's event type. This is the same
// narrowing the customer already opted into via updateWebhookEndpointSubscription
// (dashboard/lib/db.ts) — without it, a row whose delivery attempt was already
// in flight when the subscription changed would survive that cleanup and could
// be delivered again if the customer later re-subscribed to the dropped type.
func (e *Emitter) deleteUnsubscribedRow(ctx context.Context, id int64) error {
	_, err := e.db.Exec(ctx, `DELETE FROM webhook_deliveries WHERE id = $1`, id)
	return err
}
