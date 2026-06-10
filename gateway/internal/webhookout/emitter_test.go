package webhookout

import (
	"context"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
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
