package webhookout

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// newTestPostgres mirrors the same pattern used in internal/operator/store_test.go
// and internal/auth/store_test.go: skip when Postgres is unreachable, unless the
// DSN was explicitly requested (CI), in which case failure is fatal.
func newTestPostgres(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	explicit := dsn != ""
	if !explicit {
		if v := os.Getenv("POSTGRES_DSN"); v != "" {
			dsn = v
			explicit = true
		} else {
			dsn = "postgres://crucible@localhost:5432/crucible?sslmode=disable"
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		if explicit {
			t.Fatalf("TEST_DATABASE_URL set but postgres unavailable: %v", err)
		}
		t.Skipf("postgres unavailable, skipping: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		if explicit {
			t.Fatalf("TEST_DATABASE_URL set but postgres ping failed: %v", err)
		}
		t.Skipf("postgres ping failed, skipping: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// seedCustomer inserts a minimal customers row and registers cascade-safe cleanup.
// webhook_endpoints and webhook_deliveries both cascade-delete from customers via
// their FK chains, so deleting the customer is sufficient teardown.
func seedCustomer(t *testing.T, pool *pgxpool.Pool, email string) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	var id uuid.UUID
	err := pool.QueryRow(ctx,
		`INSERT INTO customers (email, plan_id) VALUES ($1, 'free') RETURNING id`,
		email,
	).Scan(&id)
	if err != nil {
		t.Fatalf("seedCustomer: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM customers WHERE id = $1`, id)
	})
	return id
}

// seedEndpoint inserts an active webhook_endpoints row for customerID.
func seedEndpoint(t *testing.T, pool *pgxpool.Pool, customerID uuid.UUID, url string) uuid.UUID {
	t.Helper()
	return seedEndpointActive(t, pool, customerID, url, true)
}

// seedEndpointActive inserts a webhook_endpoints row for customerID with an
// explicit active flag, so tests can exercise the inactive-endpoint replay guard.
func seedEndpointActive(t *testing.T, pool *pgxpool.Pool, customerID uuid.UUID, url string, active bool) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	secret, err := GenerateSecret()
	if err != nil {
		t.Fatalf("GenerateSecret: %v", err)
	}
	var id uuid.UUID
	err = pool.QueryRow(ctx,
		`INSERT INTO webhook_endpoints (customer_id, url, secret, active) VALUES ($1, $2, $3, $4) RETURNING id`,
		customerID, url, secret, active,
	).Scan(&id)
	if err != nil {
		t.Fatalf("seedEndpointActive: %v", err)
	}
	return id
}

// seedDeliveryOpts controls the fields of a seeded webhook_deliveries row not
// covered by seedDelivery's required arguments.
type seedDeliveryOpts struct {
	attempts         int
	lastResponseCode *int
	createdAt        time.Time // zero value lets Postgres default to NOW()
}

func seedDelivery(t *testing.T, pool *pgxpool.Pool, endpointID uuid.UUID, status string, opts seedDeliveryOpts) int64 {
	t.Helper()
	ctx := context.Background()
	createdAt := opts.createdAt
	if createdAt.IsZero() {
		createdAt = time.Now()
	}
	var id int64
	err := pool.QueryRow(ctx, `
		INSERT INTO webhook_deliveries (event_id, event_type, endpoint_id, payload, status, attempts, last_response_code, created_at)
		VALUES ($1, 'test.event', $2, '{}'::jsonb, $3, $4, $5, $6)
		RETURNING id
	`, uuid.NewString(), endpointID, status, opts.attempts, opts.lastResponseCode, createdAt).Scan(&id)
	if err != nil {
		t.Fatalf("seedDelivery: %v", err)
	}
	return id
}

// fetchDelivery reads back the mutable fields of a webhook_deliveries row for assertions.
type deliverySnapshot struct {
	status           string
	attempts         int
	claimedAt        *time.Time
	nextAttemptAt    time.Time
	lastResponseCode *int
}

func fetchDelivery(t *testing.T, pool *pgxpool.Pool, id int64) deliverySnapshot {
	t.Helper()
	var s deliverySnapshot
	err := pool.QueryRow(context.Background(), `
		SELECT status, attempts, claimed_at, next_attempt_at, last_response_code
		FROM webhook_deliveries WHERE id = $1
	`, id).Scan(&s.status, &s.attempts, &s.claimedAt, &s.nextAttemptAt, &s.lastResponseCode)
	if err != nil {
		t.Fatalf("fetchDelivery(%d): %v", id, err)
	}
	return s
}

func TestReplayByID_RequeuesDeadLetterRow(t *testing.T) {
	pool := newTestPostgres(t)
	custID := seedCustomer(t, pool, "replay-single@example.com")
	epID := seedEndpoint(t, pool, custID, "https://example.com/hook")
	code := 500
	before := time.Now()
	id := seedDelivery(t, pool, epID, "dead_letter", seedDeliveryOpts{attempts: 7, lastResponseCode: &code})

	if err := ReplayByID(context.Background(), pool, id); err != nil {
		t.Fatalf("ReplayByID: %v", err)
	}

	got := fetchDelivery(t, pool, id)
	if got.status != "pending" {
		t.Errorf("status: got %q, want pending", got.status)
	}
	if got.attempts != 0 {
		t.Errorf("attempts: got %d, want 0", got.attempts)
	}
	if got.claimedAt != nil {
		t.Errorf("claimed_at: got %v, want nil", got.claimedAt)
	}
	if got.lastResponseCode != nil {
		t.Errorf("last_response_code: got %v, want nil", got.lastResponseCode)
	}
	if got.nextAttemptAt.Before(before) {
		t.Errorf("next_attempt_at: got %v, want >= %v", got.nextAttemptAt, before)
	}
}

func TestReplayByID_NonDeadLetterRow_NoOp(t *testing.T) {
	pool := newTestPostgres(t)
	custID := seedCustomer(t, pool, "replay-noop@example.com")
	epID := seedEndpoint(t, pool, custID, "https://example.com/hook")

	for _, status := range []string{"pending", "delivering", "delivered"} {
		status := status
		t.Run(status, func(t *testing.T) {
			id := seedDelivery(t, pool, epID, status, seedDeliveryOpts{attempts: 1})
			before := fetchDelivery(t, pool, id)

			err := ReplayByID(context.Background(), pool, id)
			if err != pgx.ErrNoRows {
				t.Fatalf("ReplayByID(%s row): got err %v, want pgx.ErrNoRows", status, err)
			}

			after := fetchDelivery(t, pool, id)
			if after != before {
				t.Errorf("row was mutated by a no-op replay: before=%+v after=%+v", before, after)
			}
		})
	}
}

func TestReplayByID_InactiveEndpoint(t *testing.T) {
	pool := newTestPostgres(t)
	custID := seedCustomer(t, pool, "replay-inactive-single@example.com")
	epID := seedEndpointActive(t, pool, custID, "https://example.com/hook", false)
	id := seedDelivery(t, pool, epID, "dead_letter", seedDeliveryOpts{attempts: 7})

	err := ReplayByID(context.Background(), pool, id)
	if !errors.Is(err, ErrEndpointInactive) {
		t.Fatalf("ReplayByID: got err %v, want ErrEndpointInactive", err)
	}

	if s := fetchDelivery(t, pool, id); s.status != "dead_letter" {
		t.Errorf("row was mutated despite inactive endpoint: got status %q", s.status)
	}
}

func TestReplayByID_NotFound(t *testing.T) {
	pool := newTestPostgres(t)
	err := ReplayByID(context.Background(), pool, -1)
	if err != pgx.ErrNoRows {
		t.Fatalf("got err %v, want pgx.ErrNoRows", err)
	}
}

func TestReplayByEndpoint_RequeuesOnlyMatchingDeadLetterRows(t *testing.T) {
	pool := newTestPostgres(t)
	custID := seedCustomer(t, pool, "replay-bulk@example.com")
	targetEP := seedEndpoint(t, pool, custID, "https://example.com/target")
	otherEP := seedEndpoint(t, pool, custID, "https://example.com/other")

	deadOnTarget1 := seedDelivery(t, pool, targetEP, "dead_letter", seedDeliveryOpts{attempts: 7})
	deadOnTarget2 := seedDelivery(t, pool, targetEP, "dead_letter", seedDeliveryOpts{attempts: 7})
	pendingOnTarget := seedDelivery(t, pool, targetEP, "pending", seedDeliveryOpts{attempts: 1})
	deadOnOther := seedDelivery(t, pool, otherEP, "dead_letter", seedDeliveryOpts{attempts: 7})

	ids, err := ReplayByEndpoint(context.Background(), pool, targetEP)
	if err != nil {
		t.Fatalf("ReplayByEndpoint: %v", err)
	}

	if len(ids) != 2 {
		t.Fatalf("requeued ids: got %d, want 2 (%v)", len(ids), ids)
	}
	got := map[int64]bool{ids[0]: true, ids[1]: true}
	if !got[deadOnTarget1] || !got[deadOnTarget2] {
		t.Errorf("requeued ids %v do not match expected {%d, %d}", ids, deadOnTarget1, deadOnTarget2)
	}

	if s := fetchDelivery(t, pool, deadOnTarget1); s.status != "pending" {
		t.Errorf("deadOnTarget1 status: got %q, want pending", s.status)
	}
	if s := fetchDelivery(t, pool, deadOnTarget2); s.status != "pending" {
		t.Errorf("deadOnTarget2 status: got %q, want pending", s.status)
	}
	if s := fetchDelivery(t, pool, pendingOnTarget); s.status != "pending" || s.attempts != 1 {
		t.Errorf("pendingOnTarget was mutated: %+v", s)
	}
	if s := fetchDelivery(t, pool, deadOnOther); s.status != "dead_letter" {
		t.Errorf("deadOnOther leaked into a different endpoint's replay: got %q, want dead_letter", s.status)
	}
}

func TestReplayByEndpoint_NoMatches(t *testing.T) {
	pool := newTestPostgres(t)
	ids, err := ReplayByEndpoint(context.Background(), pool, uuid.New())
	if err != nil {
		t.Fatalf("ReplayByEndpoint: %v", err)
	}
	if len(ids) != 0 {
		t.Fatalf("got %d ids, want 0", len(ids))
	}
}

func TestReplayByEndpoint_InactiveEndpoint_NoRequeue(t *testing.T) {
	pool := newTestPostgres(t)
	custID := seedCustomer(t, pool, "replay-inactive-bulk@example.com")
	epID := seedEndpointActive(t, pool, custID, "https://example.com/hook", false)
	id1 := seedDelivery(t, pool, epID, "dead_letter", seedDeliveryOpts{attempts: 7})
	id2 := seedDelivery(t, pool, epID, "dead_letter", seedDeliveryOpts{attempts: 7})

	ids, err := ReplayByEndpoint(context.Background(), pool, epID)
	if err != nil {
		t.Fatalf("ReplayByEndpoint: %v", err)
	}
	if len(ids) != 0 {
		t.Fatalf("requeued ids for inactive endpoint: got %d, want 0 (%v)", len(ids), ids)
	}
	if s := fetchDelivery(t, pool, id1); s.status != "dead_letter" {
		t.Errorf("id1 was mutated despite inactive endpoint: got status %q", s.status)
	}
	if s := fetchDelivery(t, pool, id2); s.status != "dead_letter" {
		t.Errorf("id2 was mutated despite inactive endpoint: got status %q", s.status)
	}
}

func TestListDeadLetters_Pagination(t *testing.T) {
	pool := newTestPostgres(t)
	custID := seedCustomer(t, pool, "list-page@example.com")
	epID := seedEndpoint(t, pool, custID, "https://example.com/hook")

	seedDelivery(t, pool, epID, "dead_letter", seedDeliveryOpts{attempts: 7})
	seedDelivery(t, pool, epID, "dead_letter", seedDeliveryOpts{attempts: 7})
	seedDelivery(t, pool, epID, "dead_letter", seedDeliveryOpts{attempts: 7})
	// A non-dead_letter row must never appear in the listing.
	seedDelivery(t, pool, epID, "pending", seedDeliveryOpts{attempts: 1})

	page, err := ListDeadLetters(context.Background(), pool, DeadLettersFilter{Page: 1, PerPage: 2})
	if err != nil {
		t.Fatalf("ListDeadLetters: %v", err)
	}
	if page.Total < 3 {
		t.Fatalf("total: got %d, want >= 3", page.Total)
	}
	if len(page.Items) != 2 {
		t.Fatalf("items: got %d, want 2", len(page.Items))
	}
	for _, it := range page.Items {
		if it.EndpointURL != "https://example.com/hook" {
			t.Errorf("unexpected endpoint_url: %q", it.EndpointURL)
		}
	}
}

func TestListDeadLetters_OrderedMostRecentFirst(t *testing.T) {
	pool := newTestPostgres(t)
	custID := seedCustomer(t, pool, "list-order@example.com")
	epID := seedEndpoint(t, pool, custID, "https://example.com/hook")

	now := time.Now()
	older := seedDelivery(t, pool, epID, "dead_letter", seedDeliveryOpts{attempts: 7, createdAt: now.Add(-time.Hour)})
	newer := seedDelivery(t, pool, epID, "dead_letter", seedDeliveryOpts{attempts: 7, createdAt: now})

	page, err := ListDeadLetters(context.Background(), pool, DeadLettersFilter{Page: 1, PerPage: 100})
	if err != nil {
		t.Fatalf("ListDeadLetters: %v", err)
	}

	var newerIdx, olderIdx = -1, -1
	for i, it := range page.Items {
		if it.ID == newer {
			newerIdx = i
		}
		if it.ID == older {
			olderIdx = i
		}
	}
	if newerIdx == -1 || olderIdx == -1 {
		t.Fatalf("seeded rows missing from listing: newerIdx=%d olderIdx=%d", newerIdx, olderIdx)
	}
	if newerIdx > olderIdx {
		t.Errorf("expected newer row (id=%d) before older row (id=%d), got indices %d and %d", newer, older, newerIdx, olderIdx)
	}
}

func TestListDeadLetters_ReportsEndpointActive(t *testing.T) {
	pool := newTestPostgres(t)
	custID := seedCustomer(t, pool, "list-endpoint-active@example.com")
	activeEP := seedEndpointActive(t, pool, custID, "https://example.com/active", true)
	inactiveEP := seedEndpointActive(t, pool, custID, "https://example.com/inactive", false)

	activeID := seedDelivery(t, pool, activeEP, "dead_letter", seedDeliveryOpts{attempts: 7})
	inactiveID := seedDelivery(t, pool, inactiveEP, "dead_letter", seedDeliveryOpts{attempts: 7})

	page, err := ListDeadLetters(context.Background(), pool, DeadLettersFilter{Page: 1, PerPage: 100})
	if err != nil {
		t.Fatalf("ListDeadLetters: %v", err)
	}

	var sawActive, sawInactive bool
	for _, it := range page.Items {
		switch it.ID {
		case activeID:
			sawActive = true
			if !it.EndpointActive {
				t.Errorf("delivery %d: endpoint_active = false, want true", it.ID)
			}
		case inactiveID:
			sawInactive = true
			if it.EndpointActive {
				t.Errorf("delivery %d: endpoint_active = true, want false", it.ID)
			}
		}
	}
	if !sawActive || !sawInactive {
		t.Fatalf("seeded rows missing from listing: sawActive=%v sawInactive=%v", sawActive, sawInactive)
	}
}

// TestReplayByID_PickedUpByEmitter is the end-to-end acceptance check: a
// dead-lettered row, once replayed, is observed transitioning off dead_letter
// because the already-running Emitter worker delivers it — replay itself never
// calls the customer URL directly.
func TestReplayByID_PickedUpByEmitter(t *testing.T) {
	pool := newTestPostgres(t)

	called := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case called <- struct{}{}:
		default:
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	custID := seedCustomer(t, pool, "replay-e2e@example.com")
	epID := seedEndpoint(t, pool, custID, srv.URL)
	code := 500
	id := seedDelivery(t, pool, epID, "dead_letter", seedDeliveryOpts{attempts: 7, lastResponseCode: &code})

	if err := ReplayByID(context.Background(), pool, id); err != nil {
		t.Fatalf("ReplayByID: %v", err)
	}
	if s := fetchDelivery(t, pool, id); s.status != "pending" {
		t.Fatalf("row not requeued to pending before emitter tick: got %q", s.status)
	}

	// Use the same worker path production takes, minus the guarded transport
	// (which would block a loopback httptest server) — deliver() is exercised
	// identically to how the background worker calls it via processDue.
	e := &Emitter{db: pool, client: &http.Client{Timeout: deliveryTimeout}}
	if err := e.processDue(context.Background()); err != nil {
		t.Fatalf("processDue: %v", err)
	}

	select {
	case <-called:
	case <-time.After(5 * time.Second):
		t.Fatal("emitter did not call the endpoint after replay")
	}

	// deliver() writes its outcome via a background-derived context after the
	// HTTP call returns; poll briefly rather than asserting immediately after.
	deadline := time.Now().Add(2 * time.Second)
	for {
		s := fetchDelivery(t, pool, id)
		if s.status == "delivered" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("row never transitioned off dead_letter/pending: got %q", s.status)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
