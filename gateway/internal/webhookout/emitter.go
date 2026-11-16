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
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/zerolog/log"
	"go.opentelemetry.io/otel/propagation"

	"github.com/Unluckyathecking/crucible/gateway/internal/channelsig"
	"github.com/Unluckyathecking/crucible/gateway/internal/egress"
	"github.com/Unluckyathecking/crucible/gateway/internal/events"
	"github.com/Unluckyathecking/crucible/gateway/internal/tracing"
)

// webhookoutTracerName names the Tracer deliver starts webhook.deliver spans
// from. Derived via otel.Tracer, which resolves to whatever provider is
// currently registered via otel.SetTracerProvider — see
// tracing.NewProvider's doc comment for why the async delivery loop uses the
// global registry instead of an explicit TracerProvider field: Emitter has
// no constructor-injection point that main.go (outside this module's edit
// scope) could wire one through.
const webhookoutTracerName = "crucible.webhookout"

// deliverPropagator is the W3C TraceContext propagator used to inject the
// outbound traceparent header on a delivery request. A distinct package-level
// var from tracing's unexported one (that package's is not reachable from
// here); propagation.TraceContext is a zero-size stateless struct so a
// second instance costs nothing.
var deliverPropagator = propagation.TraceContext{}

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
	// failureThreshold is the consecutive-dead-letter count past which an
	// endpoint auto-disables (see recordDeliveryFailure); <= 0 (the zero
	// value) disables auto-disable. Unlike maxInflightPerCustomer this is
	// set post-construction via SetFailureThreshold — routes.go's NewRouter
	// wires it in from config after obtaining the Emitter, which may already
	// be running its background delivery worker goroutine (started by
	// NewEmitter before it returns) — so it needs atomic access rather than
	// a plain field to stay race-free under `go test -race`.
	failureThreshold atomic.Int32
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

// SetFailureThreshold sets the consecutive-dead-letter count after which an
// endpoint auto-disables — see Emitter.failureThreshold's doc comment for why
// this is a post-construction setter rather than a NewEmitter option/field.
// n <= 0 disables auto-disable (the zero-value default, matching
// WEBHOOK_ENDPOINT_FAILURE_THRESHOLD's zero-config-safe default). Safe to
// call on a nil Emitter and safe to call concurrently with the running
// delivery worker.
func (e *Emitter) SetFailureThreshold(n int) {
	if e == nil {
		return
	}
	e.failureThreshold.Store(int32(n))
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
	return e.emit(ctx, e.db, customerID, eventType, payload)
}

// EmitTx fans an event out exactly like Emit, but inserts the
// webhook_deliveries row(s) on the caller's existing transaction tx instead
// of a fresh pool connection — so the enqueue commits or rolls back
// atomically with whatever state change tx also contains. The background
// delivery worker (claimDue/deliver) is unaffected: it only ever sees rows
// once tx has committed. A nil Emitter is a safe no-op, matching Emit.
func (e *Emitter) EmitTx(ctx context.Context, tx pgx.Tx, customerID uuid.UUID, eventType string, payload []byte) error {
	if e == nil {
		return nil
	}
	if tx == nil {
		return fmt.Errorf("webhookout: emit tx: tx is nil")
	}
	return e.emit(ctx, tx, customerID, eventType, payload)
}

// txExecer is the subset of *pgxpool.Pool and pgx.Tx that emit needs —
// extracted so the same insert logic runs against either the emitter's own
// pool (Emit) or a caller-supplied transaction (EmitTx).
type txExecer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

func (e *Emitter) emit(ctx context.Context, x txExecer, customerID uuid.UUID, eventType string, payload []byte) error {
	if !json.Valid(payload) {
		return fmt.Errorf("webhookout: emit: payload is not valid JSON")
	}
	eventID := uuid.New().String()
	// Captured from whatever span (if any) is active in ctx — tracing.Middleware
	// places one there when OTEL tracing is enabled. "" (tracing disabled, or
	// no active span) stores NULL, never an empty string, so deliver's
	// RestoreTraceparent no-op path is exact rather than approximate.
	var traceparentParam *string
	if tv := tracing.CaptureTraceparent(ctx); tv != "" {
		traceparentParam = &tv
	}
	_, err := x.Exec(ctx, `
		INSERT INTO webhook_deliveries (event_id, event_type, endpoint_id, payload, traceparent)
		SELECT $1, $2, we.id, $3::jsonb, $5
		FROM webhook_endpoints we
		WHERE we.customer_id = $4 AND we.active = TRUE
		  AND (we.subscribed_events IS NULL OR $2 = ANY(we.subscribed_events))
	`, eventID, eventType, string(payload), customerID, traceparentParam)
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
	id         int64
	eventID    string
	eventType  string
	payload    []byte
	attempts   int
	url        string
	secret     []byte
	endpointID uuid.UUID
	// customerID is only populated by claimDue's fairness-enabled path
	// (maxInflightPerCustomer > 0); the default disabled path never selects
	// or needs it.
	customerID uuid.UUID
	// traceparent is the W3C traceparent captured by Emit at enqueue time
	// (see tracing.CaptureTraceparent), "" when none was captured — tracing
	// disabled, or Emit's caller context carried no active span.
	traceparent string
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
