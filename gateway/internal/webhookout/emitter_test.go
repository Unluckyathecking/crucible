package webhookout

import (
	"context"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace/noop"

	"github.com/Unluckyathecking/crucible/gateway/internal/egress"
	"github.com/Unluckyathecking/crucible/gateway/internal/events"
	"github.com/Unluckyathecking/crucible/gateway/internal/observability"
	"github.com/Unluckyathecking/crucible/gateway/internal/tracing"
)

// withGlobalTestTracerProvider registers a SpanRecorder-backed TracerProvider
// as the process-global otel provider — the mechanism Emitter.deliver relies
// on (see webhookoutTracerName's doc comment) since it has no
// constructor-injection point for an explicit TracerProvider. Restores
// whatever was globally registered before the test via t.Cleanup so this
// test's provider can't leak into any other test in the package.
func withGlobalTestTracerProvider(t *testing.T) *tracetest.SpanRecorder {
	t.Helper()
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(tp)
	t.Cleanup(func() {
		otel.SetTracerProvider(prev)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = tp.Shutdown(ctx)
	})
	return sr
}

// TestSign verifies that Sign produces a deterministic, non-empty HMAC-SHA256 hex digest
// and that different inputs produce different signatures.
func TestSign(t *testing.T) {
	secret := []byte("supersecretkey12345678901234567890")
	ts := "1700000000"
	body := []byte(`{"event":"test"}`)

	sig := Sign(secret, ts, body)
	if sig == "" {
		t.Fatal("Sign returned empty string")
	}
	if _, err := hex.DecodeString(sig); err != nil {
		t.Fatalf("Sign returned non-hex string: %q", sig)
	}
	if len(sig) != 64 {
		t.Fatalf("expected 64-char hex digest (SHA-256), got %d", len(sig))
	}

	// Different body → different signature.
	sig2 := Sign(secret, ts, []byte(`{"event":"other"}`))
	if sig == sig2 {
		t.Fatal("different bodies produced the same signature")
	}

	// Different timestamp → different signature.
	sig3 := Sign(secret, "1700000001", body)
	if sig == sig3 {
		t.Fatal("different timestamps produced the same signature")
	}

	// Deterministic: same inputs → same output.
	if Sign(secret, ts, body) != sig {
		t.Fatal("Sign is not deterministic")
	}
}

// TestGenerateSecret verifies length and randomness.
func TestGenerateSecret(t *testing.T) {
	s1, err := GenerateSecret()
	if err != nil {
		t.Fatalf("GenerateSecret: %v", err)
	}
	if len(s1) != 32 {
		t.Fatalf("expected 32 bytes, got %d", len(s1))
	}
	s2, _ := GenerateSecret()
	// Two 32-byte random values should never be equal (birthday probability ≈ 2⁻²⁵⁶).
	equal := true
	for i := range s1 {
		if s1[i] != s2[i] {
			equal = false
			break
		}
	}
	if equal {
		t.Fatal("GenerateSecret returned identical values on consecutive calls")
	}
}

// TestNewEmitter_NilDB verifies the nil-safe constructor.
func TestNewEmitter_NilDB(t *testing.T) {
	e := NewEmitter(context.Background(), nil)
	if e != nil {
		t.Fatal("expected nil Emitter when db is nil")
	}
}

// TestNilEmitterMethods verifies nil-receiver safety for all exported methods.
func TestNilEmitterMethods(t *testing.T) {
	var e *Emitter
	if err := e.Emit(context.Background(), uuid.New(), "test", []byte(`{}`)); err != nil {
		t.Fatalf("nil Emitter.Emit returned unexpected error: %v", err)
	}
	// Stop on nil receiver must not panic.
	e.Stop()
}

// capturedHeaders holds header values captured by the test HTTP server.
type capturedHeaders struct {
	ts        string
	sig       string
	eventID   string
	ct        string
	eventType string
}

// TestDeliver_Success verifies that a 2xx response marks the delivery succeeded
// and that the correct headers are set on the outgoing request.
//
// Header values are sent through a buffered channel to avoid a data race between
// the server goroutine (writes) and the test goroutine (reads): the channel
// establishes the required happens-before relationship.
func TestDeliver_Success(t *testing.T) {
	secret, _ := GenerateSecret()

	// Buffered so the handler never blocks if deliver returns before we read.
	captured := make(chan capturedHeaders, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured <- capturedHeaders{
			ts:        r.Header.Get("X-Crucible-Timestamp"),
			sig:       r.Header.Get("X-Crucible-Signature"),
			eventID:   r.Header.Get("X-Webhook-Event-ID"),
			ct:        r.Header.Get("Content-Type"),
			eventType: r.Header.Get("X-Webhook-Event-Type"),
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	row := pendingRow{
		id:        1,
		eventID:   "evt-abc",
		eventType: "order.created",
		payload:   []byte(`{"type":"test"}`),
		attempts:  0,
		url:       srv.URL,
		secret:    secret,
	}

	// Use a no-op DB so we can call deliver without a real Postgres.
	e := &Emitter{client: &http.Client{Timeout: deliveryTimeout}}
	e.deliver(context.Background(), row)

	var h capturedHeaders
	select {
	case h = <-captured:
	case <-time.After(5 * time.Second):
		t.Fatal("server was not called within timeout")
	}

	if h.ct != "application/json" {
		t.Errorf("Content-Type: got %q, want application/json", h.ct)
	}
	if h.eventID != "evt-abc" {
		t.Errorf("X-Webhook-Event-ID: got %q, want evt-abc", h.eventID)
	}
	if h.eventType != "order.created" {
		t.Errorf("X-Webhook-Event-Type: got %q, want order.created", h.eventType)
	}
	if h.ts == "" {
		t.Error("X-Crucible-Timestamp is empty")
	}
	// Verify signature format: "t=<ts>,v1=<hex>"
	expectedSig := "t=" + h.ts + ",v1=" + Sign(secret, h.ts, row.payload)
	if h.sig != expectedSig {
		t.Errorf("X-Crucible-Signature mismatch:\n got  %q\n want %q", h.sig, expectedSig)
	}
}

// TestDeliver_Failure_MaxAttempts verifies that a non-2xx response on the last
// attempt does not attempt further retries (dead_letter path). Since markDeadLetter
// needs a DB, we verify the flow doesn't panic and the server was called.
func TestDeliver_Failure_MaxAttempts(t *testing.T) {
	secret, _ := GenerateSecret()

	called := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called <- struct{}{}
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	row := pendingRow{
		id:       2,
		eventID:  "evt-dead",
		payload:  []byte(`{}`),
		attempts: maxAttempts - 1, // one more attempt will hit the cap
		url:      srv.URL,
		secret:   secret,
	}

	e := &Emitter{client: &http.Client{Timeout: deliveryTimeout}}
	e.deliver(context.Background(), row) // must not panic

	select {
	case <-called:
	case <-time.After(5 * time.Second):
		t.Error("expected the endpoint to be called")
	}
}

// TestDeliver_PrivateTarget_BlockedByGuard verifies that when the Emitter's
// client uses the egress-guarded transport (as NewEmitter constructs it), a
// delivery to a loopback URL never reaches the HTTP server: the guard fails
// the dial closed, and deliver() records it through the normal failure path
// (doErr != nil) rather than mistaking it for a successful 2xx response.
func TestDeliver_PrivateTarget_BlockedByGuard(t *testing.T) {
	secret, _ := GenerateSecret()

	called := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called <- struct{}{}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	row := pendingRow{
		id:       3,
		eventID:  "evt-blocked",
		payload:  []byte(`{}`),
		attempts: 0,
		url:      srv.URL, // httptest servers listen on 127.0.0.1 — loopback, must be blocked
		secret:   secret,
	}

	// db is left nil: deliver()'s mark*/scheduleRetry calls all nil-check e.db
	// and no-op, so this exercises the guard without requiring Postgres.
	e := &Emitter{client: &http.Client{Timeout: deliveryTimeout, Transport: egress.GuardedTransport()}}
	e.deliver(context.Background(), row)

	select {
	case <-called:
		t.Fatal("guarded transport allowed a connection to a loopback target")
	case <-time.After(200 * time.Millisecond):
		// Expected: the dial was blocked before any request reached the server.
	}
}

// TestBackoffSchedule verifies that backoffSchedule has at least maxAttempts entries
// or the last entry is used as the cap (defensive bound check).
func TestBackoffSchedule(t *testing.T) {
	for i := 0; i < maxAttempts; i++ {
		idx := i
		if idx >= len(backoffSchedule) {
			idx = len(backoffSchedule) - 1
		}
		if backoffSchedule[idx] <= 0 {
			t.Errorf("backoffSchedule[%d] = %v; want positive duration", idx, backoffSchedule[idx])
		}
	}
}

// TestWorkerTickCancelledContext verifies the worker exits when ctx is cancelled.
func TestWorkerTickCancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	e := &Emitter{client: &http.Client{}}
	done := make(chan struct{})
	go func() {
		e.run(ctx)
		close(done)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("worker did not exit after context cancellation")
	}
}

// TestEmitInvalidJSON verifies that Emit rejects payloads that are not valid JSON.
// Uses a non-nil Emitter with a nil DB so the json.Valid check is reached but no
// DB call is made.
func TestEmitInvalidJSON(t *testing.T) {
	e := &Emitter{db: nil}
	if err := e.Emit(context.Background(), uuid.New(), "test", []byte(`not-json`)); err == nil {
		t.Fatal("expected error for invalid JSON payload, got nil")
	}
}

// seedEndpointSubscribed inserts an active webhook_endpoints row with an explicit
// subscribed_events value. Passing subscribed == nil stores SQL NULL (subscribed
// to every event type); a non-nil slice — including an empty one — is stored as-is.
func seedEndpointSubscribed(t *testing.T, pool *pgxpool.Pool, customerID uuid.UUID, url string, subscribed []string) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	secret, err := GenerateSecret()
	if err != nil {
		t.Fatalf("GenerateSecret: %v", err)
	}
	var id uuid.UUID
	err = pool.QueryRow(ctx,
		`INSERT INTO webhook_endpoints (customer_id, url, secret, active, subscribed_events) VALUES ($1, $2, $3, TRUE, $4) RETURNING id`,
		customerID, url, secret, subscribed,
	).Scan(&id)
	if err != nil {
		t.Fatalf("seedEndpointSubscribed: %v", err)
	}
	return id
}

// countDeliveriesForEndpoint returns the number of webhook_deliveries rows queued
// for endpointID, regardless of status.
func countDeliveriesForEndpoint(t *testing.T, pool *pgxpool.Pool, endpointID uuid.UUID) int {
	t.Helper()
	var n int
	err := pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM webhook_deliveries WHERE endpoint_id = $1`, endpointID,
	).Scan(&n)
	if err != nil {
		t.Fatalf("countDeliveriesForEndpoint: %v", err)
	}
	return n
}

// TestEmit_SubscriptionFilter_UnsubscribedEndpointGetsZeroRows is the acceptance
// check for the per-endpoint subscription predicate: an endpoint subscribed to a
// different event type than the one emitted must receive no delivery row at all.
func TestEmit_SubscriptionFilter_UnsubscribedEndpointGetsZeroRows(t *testing.T) {
	pool := newTestPostgres(t)
	custID := seedCustomer(t, pool, "emit-sub-unsubscribed@example.com")
	epID := seedEndpointSubscribed(t, pool, custID, "https://example.com/hook", []string{events.SubscriptionUpdated})

	e := &Emitter{db: pool}
	if err := e.Emit(context.Background(), custID, events.QuotaExceeded, []byte(`{}`)); err != nil {
		t.Fatalf("Emit: %v", err)
	}

	if n := countDeliveriesForEndpoint(t, pool, epID); n != 0 {
		t.Errorf("deliveries for unsubscribed event type: got %d, want 0", n)
	}
}

// TestEmit_SubscriptionFilter_SubscribedEndpointReceives verifies that an
// endpoint subscribed to the emitted event type does get a delivery row.
func TestEmit_SubscriptionFilter_SubscribedEndpointReceives(t *testing.T) {
	pool := newTestPostgres(t)
	custID := seedCustomer(t, pool, "emit-sub-subscribed@example.com")
	epID := seedEndpointSubscribed(t, pool, custID, "https://example.com/hook", []string{events.QuotaExceeded})

	e := &Emitter{db: pool}
	if err := e.Emit(context.Background(), custID, events.QuotaExceeded, []byte(`{}`)); err != nil {
		t.Fatalf("Emit: %v", err)
	}

	if n := countDeliveriesForEndpoint(t, pool, epID); n != 1 {
		t.Errorf("deliveries for subscribed event type: got %d, want 1", n)
	}
}

// TestEmit_SubscriptionFilter_NilSubscriptionMeansAllEvents is the backward-
// compatibility check: rows with no explicit subscription (subscribed_events IS
// NULL) must keep receiving every catalogue event, matching pre-0017 behavior.
func TestEmit_SubscriptionFilter_NilSubscriptionMeansAllEvents(t *testing.T) {
	pool := newTestPostgres(t)
	custID := seedCustomer(t, pool, "emit-sub-nil@example.com")
	epID := seedEndpointSubscribed(t, pool, custID, "https://example.com/hook", nil)

	e := &Emitter{db: pool}
	if err := e.Emit(context.Background(), custID, events.APIKeyRevoked, []byte(`{}`)); err != nil {
		t.Fatalf("Emit: %v", err)
	}

	if n := countDeliveriesForEndpoint(t, pool, epID); n != 1 {
		t.Errorf("deliveries for endpoint with nil subscription: got %d, want 1", n)
	}
}

// TestEmitTx_RollbackLeavesZeroRows proves EmitTx's core atomicity guarantee:
// a delivery row inserted on a caller transaction that is then rolled back
// never becomes visible.
func TestEmitTx_RollbackLeavesZeroRows(t *testing.T) {
	pool := newTestPostgres(t)
	custID := seedCustomer(t, pool, "emittx-rollback@example.com")
	epID := seedEndpoint(t, pool, custID, "https://example.com/hook")

	e := &Emitter{db: pool}
	tx, err := pool.Begin(context.Background())
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	if err := e.EmitTx(context.Background(), tx, custID, "order.created", []byte(`{"ok":true}`)); err != nil {
		t.Fatalf("EmitTx: %v", err)
	}
	if err := tx.Rollback(context.Background()); err != nil {
		t.Fatalf("rollback: %v", err)
	}

	if n := countDeliveriesForEndpoint(t, pool, epID); n != 0 {
		t.Errorf("deliveries after rollback: got %d, want 0", n)
	}
}

// TestEmitTx_CommitPersistsRow is TestEmitTx_RollbackLeavesZeroRows's positive
// counterpart: committing the caller transaction makes the row durable and
// claimable by the background delivery worker exactly like Emit's.
func TestEmitTx_CommitPersistsRow(t *testing.T) {
	pool := newTestPostgres(t)
	custID := seedCustomer(t, pool, "emittx-commit@example.com")
	epID := seedEndpoint(t, pool, custID, "https://example.com/hook")

	e := &Emitter{db: pool}
	tx, err := pool.Begin(context.Background())
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	if err := e.EmitTx(context.Background(), tx, custID, "order.created", []byte(`{"ok":true}`)); err != nil {
		t.Fatalf("EmitTx: %v", err)
	}
	if err := tx.Commit(context.Background()); err != nil {
		t.Fatalf("commit: %v", err)
	}

	if n := countDeliveriesForEndpoint(t, pool, epID); n != 1 {
		t.Errorf("deliveries after commit: got %d, want 1", n)
	}
}

// TestEmitTx_ConcurrentCommitsRace is the -race acceptance test: many
// concurrent goroutines each open their own transaction, EmitTx, and commit —
// proving EmitTx is safe for concurrent callers sharing one *Emitter and that
// every committed transaction's row survives (none are lost or duplicated).
func TestEmitTx_ConcurrentCommitsRace(t *testing.T) {
	pool := newTestPostgres(t)
	custID := seedCustomer(t, pool, "emittx-race@example.com")
	epID := seedEndpoint(t, pool, custID, "https://example.com/hook")

	e := &Emitter{db: pool}
	const n = 20
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx := context.Background()
			tx, err := pool.Begin(ctx)
			if err != nil {
				t.Errorf("begin tx: %v", err)
				return
			}
			if err := e.EmitTx(ctx, tx, custID, "order.created", []byte(`{"ok":true}`)); err != nil {
				t.Errorf("EmitTx: %v", err)
				_ = tx.Rollback(ctx)
				return
			}
			if err := tx.Commit(ctx); err != nil {
				t.Errorf("commit: %v", err)
			}
		}()
	}
	wg.Wait()

	if got := countDeliveriesForEndpoint(t, pool, epID); got != n {
		t.Errorf("deliveries after %d concurrent commits: got %d, want %d", n, got, n)
	}
}

// TestEmitTx_NilEmitter_NoOp verifies the nil-receiver safety EmitTx mirrors
// from Emit — no panic, no error, regardless of the tx argument.
func TestEmitTx_NilEmitter_NoOp(t *testing.T) {
	var e *Emitter
	if err := e.EmitTx(context.Background(), nil, uuid.New(), "test", []byte(`{}`)); err != nil {
		t.Fatalf("nil Emitter.EmitTx returned unexpected error: %v", err)
	}
}

// TestEmitTx_NilTx verifies the defensive nil-tx error path on a non-nil Emitter.
func TestEmitTx_NilTx(t *testing.T) {
	e := &Emitter{db: nil}
	if err := e.EmitTx(context.Background(), nil, uuid.New(), "test", []byte(`{}`)); err == nil {
		t.Fatal("expected error for nil tx, got nil")
	}
}

// TestValidateSubscribedEvents covers the nil/valid/invalid cases of the Go-side
// registration helper.
func TestValidateSubscribedEvents(t *testing.T) {
	if err := ValidateSubscribedEvents(nil); err != nil {
		t.Errorf("nil slice: got err %v, want nil", err)
	}
	if err := ValidateSubscribedEvents([]string{}); err != nil {
		t.Errorf("empty slice: got err %v, want nil", err)
	}
	if err := ValidateSubscribedEvents([]string{events.QuotaExceeded, events.APIKeyRotated}); err != nil {
		t.Errorf("valid types: got err %v, want nil", err)
	}
	if err := ValidateSubscribedEvents([]string{"bogus.event"}); err == nil {
		t.Error("unknown event type: got nil error, want non-nil")
	}
	if err := ValidateSubscribedEvents([]string{events.QuotaExceeded, "bogus.event"}); err == nil {
		t.Error("mixed valid/unknown event types: got nil error, want non-nil")
	}
}

// TestProcessDue_SkipsRowsNotMatchingCurrentSubscription verifies that the
// delivery worker's claim query re-checks subscribed_events at claim time, not
// just at Emit-time: a row queued before a customer narrows an endpoint's
// subscription must never be delivered once it falls outside the current
// subscription, so it's left pending (never claimed) rather than delivered.
func TestProcessDue_SkipsRowsNotMatchingCurrentSubscription(t *testing.T) {
	pool := newTestPostgres(t)
	custID := seedCustomer(t, pool, "claim-sub-filter@example.com")

	called := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case called <- struct{}{}:
		default:
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	// seedDelivery always inserts event_type 'test.event' (see replay_test.go);
	// subscribing the endpoint to a different type means that row no longer
	// matches its current subscription.
	epID := seedEndpointSubscribed(t, pool, custID, srv.URL, []string{events.QuotaExceeded})
	id := seedDelivery(t, pool, epID, "pending", seedDeliveryOpts{attempts: 0})

	e := &Emitter{db: pool, client: &http.Client{Timeout: deliveryTimeout}}
	if err := e.processDue(context.Background()); err != nil {
		t.Fatalf("processDue: %v", err)
	}

	select {
	case <-called:
		t.Fatal("endpoint was called for an event type outside its current subscription")
	case <-time.After(200 * time.Millisecond):
		// Expected: the row was never claimed.
	}

	got := fetchDelivery(t, pool, id)
	if got.status != "pending" {
		t.Errorf("status: got %q, want pending (row should remain unclaimed)", got.status)
	}
	if got.attempts != 0 {
		t.Errorf("attempts: got %d, want 0", got.attempts)
	}
}

// TestProcessDue_DeliversRowsMatchingCurrentSubscription is the companion
// positive case: the claim-time subscription predicate must not over-filter
// rows whose event type the endpoint is still subscribed to.
func TestProcessDue_DeliversRowsMatchingCurrentSubscription(t *testing.T) {
	pool := newTestPostgres(t)
	custID := seedCustomer(t, pool, "claim-sub-match@example.com")

	called := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case called <- struct{}{}:
		default:
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	epID := seedEndpointSubscribed(t, pool, custID, srv.URL, []string{"test.event"})
	id := seedDelivery(t, pool, epID, "pending", seedDeliveryOpts{attempts: 0})

	e := &Emitter{db: pool, client: &http.Client{Timeout: deliveryTimeout}}
	if err := e.processDue(context.Background()); err != nil {
		t.Fatalf("processDue: %v", err)
	}

	select {
	case <-called:
	case <-time.After(5 * time.Second):
		t.Fatal("expected the endpoint to be called for a matching subscription")
	}

	deadline := time.Now().Add(2 * time.Second)
	for {
		s := fetchDelivery(t, pool, id)
		if s.status == "delivered" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("row never delivered: got %q", s.status)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// rowExists reports whether a webhook_deliveries row with the given id exists.
func rowExists(t *testing.T, pool *pgxpool.Pool, id int64) bool {
	t.Helper()
	var exists bool
	err := pool.QueryRow(context.Background(),
		`SELECT EXISTS(SELECT 1 FROM webhook_deliveries WHERE id = $1)`, id,
	).Scan(&exists)
	if err != nil {
		t.Fatalf("rowExists(%d): %v", id, err)
	}
	return exists
}

// TestScheduleRetry_SubscribedRow_UpdatesNormally verifies scheduleRetry's
// added subscription check doesn't change behavior for a row whose event type
// the endpoint is still subscribed to: seedDelivery's rows are always
// event_type 'test.event', so subscribing to exactly that type must update
// normally, not delete.
func TestScheduleRetry_SubscribedRow_UpdatesNormally(t *testing.T) {
	pool := newTestPostgres(t)
	custID := seedCustomer(t, pool, "retry-subscribed@example.com")
	epID := seedEndpointSubscribed(t, pool, custID, "https://example.com/hook", []string{"test.event"})
	id := seedDelivery(t, pool, epID, "delivering", seedDeliveryOpts{attempts: 1})

	e := &Emitter{db: pool}
	e.scheduleRetry(id, 2, nil)

	got := fetchDelivery(t, pool, id)
	if got.status != "pending" {
		t.Errorf("status: got %q, want pending", got.status)
	}
	if got.attempts != 2 {
		t.Errorf("attempts: got %d, want 2", got.attempts)
	}
}

// TestScheduleRetry_UnsubscribedRow_DeletesRowInstead verifies that when a
// delivery attempt fails for a row whose event type the endpoint is no longer
// subscribed to (narrowed while the attempt was in flight), scheduleRetry
// deletes the row instead of writing it back to 'pending' — otherwise the row
// would survive indefinitely and become deliverable again if the customer
// later re-subscribed to that event type.
func TestScheduleRetry_UnsubscribedRow_DeletesRowInstead(t *testing.T) {
	pool := newTestPostgres(t)
	custID := seedCustomer(t, pool, "retry-unsubscribed@example.com")
	epID := seedEndpointSubscribed(t, pool, custID, "https://example.com/hook", []string{"quota.exceeded"})
	id := seedDelivery(t, pool, epID, "delivering", seedDeliveryOpts{attempts: 1})

	e := &Emitter{db: pool}
	e.scheduleRetry(id, 2, nil)

	if rowExists(t, pool, id) {
		t.Error("row for an unsubscribed event type was not deleted by scheduleRetry")
	}
}

// TestMarkDeadLetter_SubscribedRow_UpdatesNormally is markDeadLetter's
// companion to TestScheduleRetry_SubscribedRow_UpdatesNormally.
func TestMarkDeadLetter_SubscribedRow_UpdatesNormally(t *testing.T) {
	pool := newTestPostgres(t)
	custID := seedCustomer(t, pool, "deadletter-subscribed@example.com")
	epID := seedEndpointSubscribed(t, pool, custID, "https://example.com/hook", []string{"test.event"})
	id := seedDelivery(t, pool, epID, "delivering", seedDeliveryOpts{attempts: maxAttempts - 1})

	e := &Emitter{db: pool}
	e.markDeadLetter(id, maxAttempts, nil)

	got := fetchDelivery(t, pool, id)
	if got.status != "dead_letter" {
		t.Errorf("status: got %q, want dead_letter", got.status)
	}
	if got.attempts != maxAttempts {
		t.Errorf("attempts: got %d, want %d", got.attempts, maxAttempts)
	}
}

// TestMarkDeadLetter_UnsubscribedRow_DeletesRowInstead is markDeadLetter's
// companion to TestScheduleRetry_UnsubscribedRow_DeletesRowInstead.
func TestMarkDeadLetter_UnsubscribedRow_DeletesRowInstead(t *testing.T) {
	pool := newTestPostgres(t)
	custID := seedCustomer(t, pool, "deadletter-unsubscribed@example.com")
	epID := seedEndpointSubscribed(t, pool, custID, "https://example.com/hook", []string{"quota.exceeded"})
	id := seedDelivery(t, pool, epID, "delivering", seedDeliveryOpts{attempts: maxAttempts - 1})

	e := &Emitter{db: pool}
	e.markDeadLetter(id, maxAttempts, nil)

	if rowExists(t, pool, id) {
		t.Error("row for an unsubscribed event type was not deleted by markDeadLetter")
	}
}

// seedEndpointInactiveSubscribed inserts an inactive (active=FALSE) endpoint row
// with an explicit subscribed_events list. Used to verify that DeleteEndpoint's
// soft-delete (which sets active=FALSE) stops new events from queuing even when the
// endpoint's subscription still matches the emitted event type.
func seedEndpointInactiveSubscribed(t *testing.T, pool *pgxpool.Pool, customerID uuid.UUID, url string, subscribed []string) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	secret, err := GenerateSecret()
	if err != nil {
		t.Fatalf("GenerateSecret: %v", err)
	}
	var id uuid.UUID
	err = pool.QueryRow(ctx,
		`INSERT INTO webhook_endpoints (customer_id, url, secret, active, subscribed_events) VALUES ($1, $2, $3, FALSE, $4) RETURNING id`,
		customerID, url, secret, subscribed,
	).Scan(&id)
	if err != nil {
		t.Fatalf("seedEndpointInactiveSubscribed: %v", err)
	}
	return id
}

// TestEmit_InactiveEndpoint_GetsZeroRows verifies the active=TRUE guard in Emit
// (emitter.go:120): a soft-deleted endpoint (active=FALSE, as set by DeleteEndpoint)
// must not receive any webhook_deliveries rows, even when it is subscribed to the
// emitted event type. An active sibling endpoint subscribed to the same type must
// receive exactly one row, confirming Emit is not suppressing all deliveries.
func TestEmit_InactiveEndpoint_GetsZeroRows(t *testing.T) {
	pool := newTestPostgres(t)
	custID := seedCustomer(t, pool, "emit-inactive-endpoint@example.com")

	inactiveID := seedEndpointInactiveSubscribed(t, pool, custID, "https://example.com/hook-inactive", []string{events.QuotaExceeded})
	activeID := seedEndpointSubscribed(t, pool, custID, "https://example.com/hook-active", []string{events.QuotaExceeded})

	e := &Emitter{db: pool}
	if err := e.Emit(context.Background(), custID, events.QuotaExceeded, []byte(`{}`)); err != nil {
		t.Fatalf("Emit: %v", err)
	}

	if n := countDeliveriesForEndpoint(t, pool, inactiveID); n != 0 {
		t.Errorf("deliveries for inactive (soft-deleted) endpoint: got %d, want 0", n)
	}
	if n := countDeliveriesForEndpoint(t, pool, activeID); n != 1 {
		t.Errorf("deliveries for active sibling endpoint: got %d, want 1", n)
	}
}

// TestWebhookFairClaimAdvisoryLockKey_DistinctFromJobsKey guards against the
// two independent fair-claim paths (this package's claimDue and
// jobs.Store.Claim) ever contending on the same session-scoped Postgres
// advisory lock. jobs.fairClaimAdvisoryLockKey is unexported in package jobs
// (0x63727563_69626c65, "crucible" in hex — see gateway/internal/jobs/store.go),
// so its literal value is duplicated here as the only way to compare across
// packages.
func TestWebhookFairClaimAdvisoryLockKey_DistinctFromJobsKey(t *testing.T) {
	const jobsFairClaimAdvisoryLockKey int64 = 0x63727563_69626c65
	if webhookFairClaimAdvisoryLockKey == jobsFairClaimAdvisoryLockKey {
		t.Fatal("webhookFairClaimAdvisoryLockKey must differ from jobs.fairClaimAdvisoryLockKey to avoid cross-package advisory lock contention")
	}
}

// TestWithMaxInflightPerCustomer_SetsField verifies the functional-option
// wiring NewEmitter uses, without needing a live Postgres connection.
func TestWithMaxInflightPerCustomer_SetsField(t *testing.T) {
	e := &Emitter{}
	WithMaxInflightPerCustomer(5)(e)
	if e.maxInflightPerCustomer != 5 {
		t.Errorf("maxInflightPerCustomer = %d, want 5", e.maxInflightPerCustomer)
	}
}

// TestClaimDue_ZeroDisables_DoesNotSelectCustomerID is the "zero disables"
// acceptance check: with maxInflightPerCustomer at its default (0), claimDue
// must run the exact original single-query global-FIFO SELECT, which never
// fetches we.customer_id — so a claimed row's pendingRow.customerID is left
// at its zero value. Only the fairness-enabled path (> 0) fetches and relies
// on that column.
func TestClaimDue_ZeroDisables_DoesNotSelectCustomerID(t *testing.T) {
	pool := newTestPostgres(t)
	custID := seedCustomer(t, pool, "webhookout-zero-disabled@example.com")
	epID := seedEndpoint(t, pool, custID, "https://example.com/hook")
	seedDelivery(t, pool, epID, "pending", seedDeliveryOpts{attempts: 0})

	e := &Emitter{db: pool}
	tx, err := pool.Begin(context.Background())
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer tx.Rollback(context.Background()) //nolint:errcheck

	due, err := e.claimDue(context.Background(), tx)
	if err != nil {
		t.Fatalf("claimDue: %v", err)
	}
	if len(due) != 1 {
		t.Fatalf("due: got %d rows, want 1", len(due))
	}
	if due[0].customerID != uuid.Nil {
		t.Errorf("customerID: got %v, want uuid.Nil — the disabled path's query must not select we.customer_id", due[0].customerID)
	}
}

// claimAndMarkDelivering exercises exactly the transaction shape processDue
// uses around claimDue: begin, claim, mark 'delivering', commit. Used by the
// fairness tests below in place of processDue itself so they exercise the
// claim primitive without also making real HTTP deliveries.
func claimAndMarkDelivering(t *testing.T, pool *pgxpool.Pool, maxInflightPerCustomer int) []pendingRow {
	t.Helper()
	ctx := context.Background()
	e := &Emitter{db: pool, maxInflightPerCustomer: maxInflightPerCustomer}

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	due, err := e.claimDue(ctx, tx)
	if err != nil {
		t.Fatalf("claimDue: %v", err)
	}
	if len(due) == 0 {
		return nil
	}

	ids := make([]int64, len(due))
	for i, r := range due {
		ids[i] = r.id
	}
	if _, err := tx.Exec(ctx, `UPDATE webhook_deliveries SET status = 'delivering', claimed_at = NOW() WHERE id = ANY($1)`, ids); err != nil {
		t.Fatalf("mark delivering: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	return due
}

// TestClaimDue_FairnessPreventsBacklogStarvation is the acceptance test for
// the fair-claim path: with maxInflightPerCustomer disabled (0), customer
// B's due delivery is starved out of the claim page by customer A's deep
// backlog (bigger than claimPageSize). With the cap enabled, the exact same
// queue state claims B within the very first claim.
func TestClaimDue_FairnessPreventsBacklogStarvation(t *testing.T) {
	pool := newTestPostgres(t)

	run := func(t *testing.T, maxInflightPerCustomer int) bool {
		custA := seedCustomer(t, pool, "webhookout-fair-a-"+uuid.New().String()+"@example.com")
		custB := seedCustomer(t, pool, "webhookout-fair-b-"+uuid.New().String()+"@example.com")
		epA := seedEndpoint(t, pool, custA, "https://example.com/hook-a")
		epB := seedEndpoint(t, pool, custB, "https://example.com/hook-b")

		// A's backlog exceeds claimPageSize so the disabled (pure-FIFO) path
		// fills its entire page from A alone, before B's row is even seeded.
		for i := 0; i < claimPageSize+5; i++ {
			seedDelivery(t, pool, epA, "pending", seedDeliveryOpts{attempts: 0})
		}
		idB := seedDelivery(t, pool, epB, "pending", seedDeliveryOpts{attempts: 0})

		e := &Emitter{db: pool, maxInflightPerCustomer: maxInflightPerCustomer}
		tx, err := pool.Begin(context.Background())
		if err != nil {
			t.Fatalf("begin tx: %v", err)
		}
		defer tx.Rollback(context.Background()) //nolint:errcheck

		due, err := e.claimDue(context.Background(), tx)
		if err != nil {
			t.Fatalf("claimDue: %v", err)
		}
		for _, r := range due {
			if r.id == idB {
				return true
			}
		}
		return false
	}

	t.Run("disabled_starves_B", func(t *testing.T) {
		if run(t, 0) {
			t.Fatal("customer B claimed within the first cycle even though the fairness cap is disabled — pure-FIFO should have exhausted the page on A's backlog first")
		}
	})

	t.Run("enabled_claims_B_within_first_cycle", func(t *testing.T) {
		if !run(t, 1) {
			t.Fatal("customer B was not claimed within the first cycle despite maxInflightPerCustomer=1 — fairness cap did not protect against A's backlog")
		}
	})
}

// TestClaimDue_ThrottledMetric_IncrementsByDeferredCount is the acceptance
// test for crucible_webhook_deliveries_throttled_total{reason="inflight_cap"}:
// it must increment by exactly the number of candidate rows the cap deferred,
// no more and no less.
func TestClaimDue_ThrottledMetric_IncrementsByDeferredCount(t *testing.T) {
	pool := newTestPostgres(t)
	custA := seedCustomer(t, pool, "webhookout-throttle-metric-a-"+uuid.New().String()+"@example.com")
	custB := seedCustomer(t, pool, "webhookout-throttle-metric-b-"+uuid.New().String()+"@example.com")
	epA := seedEndpoint(t, pool, custA, "https://example.com/hook-a")
	epB := seedEndpoint(t, pool, custB, "https://example.com/hook-b")

	const maxInflightPerCustomer = 2
	const numA = 5
	for i := 0; i < numA; i++ {
		seedDelivery(t, pool, epA, "pending", seedDeliveryOpts{attempts: 0})
	}
	seedDelivery(t, pool, epB, "pending", seedDeliveryOpts{attempts: 0})

	before := testutil.ToFloat64(observability.WebhookDeliveriesThrottledTotal.WithLabelValues("inflight_cap"))

	e := &Emitter{db: pool, maxInflightPerCustomer: maxInflightPerCustomer}
	tx, err := pool.Begin(context.Background())
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer tx.Rollback(context.Background()) //nolint:errcheck

	due, err := e.claimDue(context.Background(), tx)
	if err != nil {
		t.Fatalf("claimDue: %v", err)
	}

	var claimedA int
	for _, r := range due {
		if r.customerID == custA {
			claimedA++
		}
	}
	if claimedA != maxInflightPerCustomer {
		t.Fatalf("claimedA: got %d, want %d (cap)", claimedA, maxInflightPerCustomer)
	}
	wantThrottled := numA - claimedA

	after := testutil.ToFloat64(observability.WebhookDeliveriesThrottledTotal.WithLabelValues("inflight_cap"))
	if got := after - before; got != float64(wantThrottled) {
		t.Errorf("WebhookDeliveriesThrottledTotal increment: got %v, want %v", got, wantThrottled)
	}
}

// TestClaimDue_MaxInflightPerCustomer_RaceEnforced is the -race test for the
// per-customer cap itself: many concurrent claimAndMarkDelivering callers
// (simulating concurrent gateway replicas) against one customer's deep
// backlog must never let that customer exceed maxInflightPerCustomer
// simultaneously 'delivering' rows, while a second customer's deliveries
// still make progress concurrently. Mirrors
// jobs.TestStore_Claim_MaxInflightPerCustomer_RaceEnforced.
func TestClaimDue_MaxInflightPerCustomer_RaceEnforced(t *testing.T) {
	pool := newTestPostgres(t)
	custA := seedCustomer(t, pool, "webhookout-fair-race-a-"+uuid.New().String()+"@example.com")
	custB := seedCustomer(t, pool, "webhookout-fair-race-b-"+uuid.New().String()+"@example.com")
	epA := seedEndpoint(t, pool, custA, "https://example.com/hook-a")
	epB := seedEndpoint(t, pool, custB, "https://example.com/hook-b")

	// Kept deliberately small: this is a genuine concurrency test (not a
	// throughput benchmark), and CI runs it alongside every other package's
	// DB-backed suite against one shared Postgres instance — a large worker
	// count here adds contention that can destabilize unrelated marginal
	// tests elsewhere without making this test any better at catching a
	// real cap violation.
	const (
		numDeliveriesA         = 16
		numDeliveriesB         = 6
		maxInflightPerCustomer = 3
		numWorkers             = 8
	)
	for i := 0; i < numDeliveriesA; i++ {
		seedDelivery(t, pool, epA, "pending", seedDeliveryOpts{attempts: 0})
	}
	for i := 0; i < numDeliveriesB; i++ {
		seedDelivery(t, pool, epB, "pending", seedDeliveryOpts{attempts: 0})
	}

	var (
		wg          sync.WaitGroup
		mu          sync.Mutex
		maxSeenA    int
		claimedForB int
	)
	checkDeliveringCap := func() {
		var n int
		if err := pool.QueryRow(context.Background(), `
			SELECT COUNT(*) FROM webhook_deliveries d
			JOIN webhook_endpoints we ON we.id = d.endpoint_id
			WHERE we.customer_id = $1 AND d.status = 'delivering'
		`, custA).Scan(&n); err != nil {
			t.Errorf("count delivering: %v", err)
			return
		}
		mu.Lock()
		if n > maxSeenA {
			maxSeenA = n
		}
		mu.Unlock()
	}

	for w := 0; w < numWorkers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 4; i++ {
				due := claimAndMarkDelivering(t, pool, maxInflightPerCustomer)
				checkDeliveringCap()
				mu.Lock()
				for _, r := range due {
					if r.customerID == custB {
						claimedForB++
					}
				}
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	if maxSeenA > maxInflightPerCustomer {
		t.Errorf("customer A had %d rows simultaneously delivering, want <= %d (maxInflightPerCustomer)", maxSeenA, maxInflightPerCustomer)
	}
	if claimedForB == 0 {
		t.Error("customer B's deliveries never progressed while A's backlog was being claimed — fairness cap did not protect B")
	}
}

// TestDeliver_PropagatesTraceContext proves the restore-at-execute half of
// the round trip: a pendingRow carrying a traceparent captured at Emit time
// produces a webhook.deliver span that continues the SAME trace, and an
// outbound HTTP request whose traceparent header carries that trace ID
// alongside the existing X-Crucible-*/X-Webhook-* headers.
func TestDeliver_PropagatesTraceContext(t *testing.T) {
	sr := withGlobalTestTracerProvider(t)

	parentCtx, parentSpan := otel.Tracer("test").Start(context.Background(), "emit")
	parentSC := parentSpan.SpanContext()
	traceparent := tracing.CaptureTraceparent(parentCtx)
	parentSpan.End()
	if traceparent == "" {
		t.Fatal("CaptureTraceparent returned empty string for an active span")
	}

	secret, _ := GenerateSecret()
	captured := make(chan http.Header, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured <- r.Header.Clone()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	row := pendingRow{
		id:          1,
		eventID:     "evt-trace",
		eventType:   "order.created",
		payload:     []byte(`{"type":"test"}`),
		attempts:    0,
		url:         srv.URL,
		secret:      secret,
		traceparent: traceparent,
	}

	e := &Emitter{client: &http.Client{Timeout: deliveryTimeout}}
	e.deliver(context.Background(), row)

	var h http.Header
	select {
	case h = <-captured:
	case <-time.After(5 * time.Second):
		t.Fatal("server was not called within timeout")
	}

	outboundTP := h.Get("traceparent")
	if outboundTP == "" {
		t.Fatal("expected a traceparent header on the outbound delivery request")
	}
	if !strings.Contains(outboundTP, parentSC.TraceID().String()) {
		t.Errorf("outbound traceparent %q does not carry trace ID %s", outboundTP, parentSC.TraceID())
	}

	var deliverSpan sdktrace.ReadOnlySpan
	for _, s := range sr.Ended() {
		if s.Name() == "webhook.deliver" {
			deliverSpan = s
		}
	}
	if deliverSpan == nil {
		t.Fatal("no webhook.deliver span recorded")
	}
	if deliverSpan.SpanContext().TraceID() != parentSC.TraceID() {
		t.Errorf("webhook.deliver trace ID = %s, want %s (must continue the enqueue trace)", deliverSpan.SpanContext().TraceID(), parentSC.TraceID())
	}
	if deliverSpan.Parent().SpanID() != parentSC.SpanID() {
		t.Errorf("webhook.deliver parent span ID = %s, want %s", deliverSpan.Parent().SpanID(), parentSC.SpanID())
	}
}

// TestDeliver_TracingDisabled_NoOutboundTraceparentHeader proves the
// zero-overhead disabled path: with the global TracerProvider at its no-op
// default (mirrors OtelTracingEnabled=false, under which tracing.NewProvider
// — the only place that calls otel.SetTracerProvider — is never invoked),
// deliver produces no outbound traceparent header and the row itself carries
// no captured traceparent either.
func TestDeliver_TracingDisabled_NoOutboundTraceparentHeader(t *testing.T) {
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(noop.NewTracerProvider())
	t.Cleanup(func() { otel.SetTracerProvider(prev) })

	secret, _ := GenerateSecret()
	captured := make(chan http.Header, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured <- r.Header.Clone()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	row := pendingRow{
		id:        1,
		eventID:   "evt-notracing",
		eventType: "order.created",
		payload:   []byte(`{"type":"test"}`),
		attempts:  0,
		url:       srv.URL,
		secret:    secret,
		// traceparent left "" — never captured (Emit's ctx carried no active
		// span, or tracing was disabled at enqueue time too).
	}

	e := &Emitter{client: &http.Client{Timeout: deliveryTimeout}}
	e.deliver(context.Background(), row)

	var h http.Header
	select {
	case h = <-captured:
	case <-time.After(5 * time.Second):
		t.Fatal("server was not called within timeout")
	}
	if tp := h.Get("traceparent"); tp != "" {
		t.Errorf("expected no traceparent header when tracing is disabled, got %q", tp)
	}
}

// TestEmitterEmit_CapturesTraceparent proves the capture-at-enqueue half of
// the round trip against a real database: Emit called under an active span
// persists a traceparent on the resulting webhook_deliveries row that
// carries the same trace ID as the enqueueing span.
func TestEmitterEmit_CapturesTraceparent(t *testing.T) {
	withGlobalTestTracerProvider(t)

	pool := newTestPostgres(t)
	custA := seedCustomer(t, pool, "webhookout-trace-"+uuid.New().String()+"@example.com")
	seedEndpoint(t, pool, custA, "https://example.com/hook")

	e := &Emitter{db: pool}

	parentCtx, parentSpan := otel.Tracer("test").Start(context.Background(), "emit")
	parentSC := parentSpan.SpanContext()

	if err := e.Emit(parentCtx, custA, "order.created", []byte(`{"ok":true}`)); err != nil {
		t.Fatalf("Emit: %v", err)
	}
	parentSpan.End()

	var tp *string
	if err := pool.QueryRow(context.Background(), `
		SELECT d.traceparent
		FROM webhook_deliveries d
		JOIN webhook_endpoints we ON we.id = d.endpoint_id
		WHERE we.customer_id = $1
	`, custA).Scan(&tp); err != nil {
		t.Fatalf("query traceparent: %v", err)
	}
	if tp == nil || *tp == "" {
		t.Fatal("expected a captured traceparent on the webhook_deliveries row")
	}
	if !strings.Contains(*tp, parentSC.TraceID().String()) {
		t.Errorf("stored traceparent %q does not carry trace ID %s", *tp, parentSC.TraceID())
	}
}

// TestEmitterEmit_NoActiveSpan_LeavesTraceparentNull proves the
// disabled/no-span path never fabricates a traceparent: a delivery row
// created by Emit under a context with no active span has a NULL
// traceparent column.
func TestEmitterEmit_NoActiveSpan_LeavesTraceparentNull(t *testing.T) {
	pool := newTestPostgres(t)
	custA := seedCustomer(t, pool, "webhookout-notrace-"+uuid.New().String()+"@example.com")
	seedEndpoint(t, pool, custA, "https://example.com/hook")

	e := &Emitter{db: pool}
	if err := e.Emit(context.Background(), custA, "order.created", []byte(`{"ok":true}`)); err != nil {
		t.Fatalf("Emit: %v", err)
	}

	var tp *string
	if err := pool.QueryRow(context.Background(), `
		SELECT d.traceparent
		FROM webhook_deliveries d
		JOIN webhook_endpoints we ON we.id = d.endpoint_id
		WHERE we.customer_id = $1
	`, custA).Scan(&tp); err != nil {
		t.Fatalf("query traceparent: %v", err)
	}
	if tp != nil {
		t.Errorf("traceparent = %q, want NULL for an Emit call with no active span", *tp)
	}
}
