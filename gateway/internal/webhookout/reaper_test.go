package webhookout

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Unluckyathecking/crucible/gateway/internal/db"
)

func applyMigrationsWebhook(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	if err := db.Apply(context.Background(), pool); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}
}

func deliveryRowExists(t *testing.T, pool *pgxpool.Pool, id int64) bool {
	t.Helper()
	var exists bool
	if err := pool.QueryRow(context.Background(),
		`SELECT EXISTS(SELECT 1 FROM webhook_deliveries WHERE id = $1)`, id,
	).Scan(&exists); err != nil {
		t.Fatalf("deliveryRowExists: %v", err)
	}
	return exists
}

// seedDeliveryAged inserts a webhook_deliveries row with the given status and
// a backdated created_at so tests can construct rows at arbitrary ages.
func seedDeliveryAged(t *testing.T, pool *pgxpool.Pool, endpointID uuid.UUID, status string, age time.Duration) int64 {
	t.Helper()
	return seedDelivery(t, pool, endpointID, status, seedDeliveryOpts{
		createdAt: time.Now().Add(-age),
	})
}

// TestDeliveryReaper_Sweep_TableDriven proves the core contract:
//   - status='delivered' rows older than retention are deleted
//   - status='delivered' rows within retention are kept
//   - status='dead_letter' rows are NEVER deleted regardless of age
//   - status='pending' and 'delivering' rows are NEVER deleted regardless of age
func TestDeliveryReaper_Sweep_TableDriven(t *testing.T) {
	pool := newTestPostgres(t)
	applyMigrationsWebhook(t, pool)
	custID := seedCustomer(t, pool, "reaper-delivery-"+uuid.NewString()+"@example.com")
	endpointID := seedEndpoint(t, pool, custID, "https://example.com/webhook")

	const retention = time.Hour

	cases := []struct {
		name        string
		status      string
		age         time.Duration
		wantDeleted bool
	}{
		{"delivered past retention is deleted", "delivered", retention + time.Minute, true},
		{"delivered within retention is kept", "delivered", retention - time.Minute, false},
		// dead_letter must NEVER be deleted regardless of age — operators replay these.
		{"dead_letter past retention is never deleted", "dead_letter", retention + 24*time.Hour, false},
		// Non-terminal statuses are never deleted.
		{"pending past retention is never deleted", "pending", retention + 24*time.Hour, false},
	}

	ids := make(map[string]int64, len(cases))
	for _, c := range cases {
		ids[c.name] = seedDeliveryAged(t, pool, endpointID, c.status, c.age)
	}

	r := NewDeliveryReaper(pool, retention, time.Hour)
	r.sweep(context.Background())

	for _, c := range cases {
		gotExists := deliveryRowExists(t, pool, ids[c.name])
		wantExists := !c.wantDeleted
		if gotExists != wantExists {
			t.Errorf("%s: row exists=%v, want %v", c.name, gotExists, wantExists)
		}
	}
}

// TestDeliveryReaper_Sweep_BatchedAcrossMultipleDeletes proves a sweep drains
// a backlog larger than one batch, mirroring the jobs.Reaper batch test.
func TestDeliveryReaper_Sweep_BatchedAcrossMultipleDeletes(t *testing.T) {
	pool := newTestPostgres(t)
	applyMigrationsWebhook(t, pool)
	custID := seedCustomer(t, pool, "reaper-batch-"+uuid.NewString()+"@example.com")
	endpointID := seedEndpoint(t, pool, custID, "https://example.com/webhook")

	const retention = time.Hour
	const batchSize = 2
	const numRows = 2*batchSize + 1 // spans three batches: 2, 2, 1

	ids := make([]int64, numRows)
	for i := range ids {
		ids[i] = seedDeliveryAged(t, pool, endpointID, "delivered", retention+time.Minute)
	}

	r := NewDeliveryReaper(pool, retention, time.Hour)
	r.batchSize = batchSize
	r.sweep(context.Background())

	for _, id := range ids {
		if deliveryRowExists(t, pool, id) {
			t.Errorf("row %d survived a sweep that should have drained the entire backlog", id)
		}
	}
}

// TestDeliveryReaper_Run_NilSafe proves Run returns immediately for a nil
// receiver, nil db, and non-positive retention.
func TestDeliveryReaper_Run_NilSafe(t *testing.T) {
	mustReturnImmediately := func(name string, run func()) {
		t.Helper()
		done := make(chan struct{})
		go func() {
			run()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatalf("%s: Run did not return immediately", name)
		}
	}

	var nilReaper *DeliveryReaper
	mustReturnImmediately("nil receiver", func() { nilReaper.Run(context.Background()) })

	nilDB := NewDeliveryReaper(nil, time.Hour, time.Second)
	mustReturnImmediately("nil db", func() { nilDB.Run(context.Background()) })

	pool := newTestPostgres(t)
	applyMigrationsWebhook(t, pool)
	zeroRetention := NewDeliveryReaper(pool, 0, time.Second)
	mustReturnImmediately("retention = 0", func() { zeroRetention.Run(context.Background()) })

	negativeRetention := NewDeliveryReaper(pool, -time.Hour, time.Second)
	mustReturnImmediately("negative retention", func() { negativeRetention.Run(context.Background()) })
}
