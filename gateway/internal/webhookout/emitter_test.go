package webhookout

import (
	"context"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Unluckyathecking/crucible/gateway/internal/egress"
	"github.com/Unluckyathecking/crucible/gateway/internal/events"
)

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
