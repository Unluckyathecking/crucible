package idempotency

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// seedKey inserts an idempotency_keys row with a backdated created_at so
// tests can construct rows at arbitrary ages without going through the normal
// Claim/Finalize flow.
func seedKey(t *testing.T, pool *pgxpool.Pool, customerID uuid.UUID, age time.Duration) int64 {
	t.Helper()
	var id int64
	err := pool.QueryRow(context.Background(), `
		INSERT INTO idempotency_keys (customer_id, idempotency_key, fingerprint, created_at)
		VALUES ($1, $2, '\x01'::bytea, NOW() - $3 * INTERVAL '1 second')
		RETURNING id
	`, customerID, uuid.NewString(), age.Seconds()).Scan(&id)
	if err != nil {
		t.Fatalf("seedKey: %v", err)
	}
	return id
}

func keyRowExists(t *testing.T, pool *pgxpool.Pool, id int64) bool {
	t.Helper()
	var exists bool
	if err := pool.QueryRow(context.Background(),
		`SELECT EXISTS(SELECT 1 FROM idempotency_keys WHERE id = $1)`, id,
	).Scan(&exists); err != nil {
		t.Fatalf("keyRowExists: %v", err)
	}
	return exists
}

// TestReaper_Sweep_TableDriven proves the core contract: rows older than the
// retention window are deleted; fresh rows survive; zero-value retention is inert.
func TestReaper_Sweep_TableDriven(t *testing.T) {
	pool := newTestPool(t)
	applyMigrations(t, pool)
	customerID := insertTestCustomer(t, pool)

	const retention = time.Hour

	cases := []struct {
		name        string
		age         time.Duration
		wantDeleted bool
	}{
		{"past retention is deleted", retention + time.Minute, true},
		{"within retention is kept", retention - time.Minute, false},
		// A row seeded at exactly NOW()-retention at insert time is strictly
		// older than NOW()-retention by the time the sweep transaction runs
		// (wall clock advances between the two), so the strict `<` predicate
		// deletes it. The boundary is not a stable "kept" case.
		{"at boundary is deleted", retention, true},
	}

	ids := make(map[string]int64, len(cases))
	for _, c := range cases {
		ids[c.name] = seedKey(t, pool, customerID, c.age)
	}

	r := NewReaper(pool, retention, time.Hour)
	r.sweep(context.Background())

	for _, c := range cases {
		gotExists := keyRowExists(t, pool, ids[c.name])
		wantExists := !c.wantDeleted
		if gotExists != wantExists {
			t.Errorf("%s: row exists=%v, want %v", c.name, gotExists, wantExists)
		}
	}
}

// TestReaper_Sweep_BatchedAcrossMultipleDeletes proves a single sweep drains a
// backlog larger than one batch, mirroring the jobs.Reaper batch test.
func TestReaper_Sweep_BatchedAcrossMultipleDeletes(t *testing.T) {
	pool := newTestPool(t)
	applyMigrations(t, pool)
	customerID := insertTestCustomer(t, pool)

	const retention = time.Hour
	const batchSize = 2
	const numRows = 2*batchSize + 1 // spans three batches: 2, 2, 1

	ids := make([]int64, numRows)
	for i := range ids {
		ids[i] = seedKey(t, pool, customerID, retention+time.Minute)
	}

	r := NewReaper(pool, retention, time.Hour)
	r.batchSize = batchSize
	r.sweep(context.Background())

	for _, id := range ids {
		if keyRowExists(t, pool, id) {
			t.Errorf("row %d survived a sweep that should have drained the entire backlog", id)
		}
	}
}

// TestReaper_Run_NilSafe proves Run returns immediately without touching the
// database for a nil receiver, nil db, and non-positive retention.
func TestReaper_Run_NilSafe(t *testing.T) {
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

	var nilReaper *Reaper
	mustReturnImmediately("nil receiver", func() { nilReaper.Run(context.Background()) })

	nilDB := NewReaper(nil, time.Hour, time.Second)
	mustReturnImmediately("nil db", func() { nilDB.Run(context.Background()) })

	pool := newTestPool(t)
	applyMigrations(t, pool)
	zeroRetention := NewReaper(pool, 0, time.Second)
	mustReturnImmediately("retention = 0", func() { zeroRetention.Run(context.Background()) })

	negativeRetention := NewReaper(pool, -time.Hour, time.Second)
	mustReturnImmediately("negative retention", func() { negativeRetention.Run(context.Background()) })
}
