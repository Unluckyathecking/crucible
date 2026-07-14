package jobs

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// seedJobWithStatusAge enqueues a job then force-sets its status and
// backdates updated_at (the column the reaper keys retention off of — see
// reaper.go's deleteBatch doc comment), so tests can construct rows the
// normal Store API can't reach directly (e.g. an aged 'succeeded' row).
func seedJobWithStatusAge(t *testing.T, pool *pgxpool.Pool, custID, keyID uuid.UUID, status string, age time.Duration) uuid.UUID {
	t.Helper()
	s := NewStore(pool)
	id, err := s.Enqueue(context.Background(), custID, keyID, "echo", "req-"+uuid.New().String(), "free", json.RawMessage(`{}`), 0, "")
	if err != nil {
		t.Fatalf("seedJobWithStatusAge: enqueue: %v", err)
	}
	if _, err := pool.Exec(context.Background(), `
		UPDATE async_jobs SET status = $2, updated_at = NOW() - $3 * INTERVAL '1 second'
		WHERE id = $1
	`, id, status, age.Seconds()); err != nil {
		t.Fatalf("seedJobWithStatusAge: backdate: %v", err)
	}
	return id
}

func jobRowExists(t *testing.T, pool *pgxpool.Pool, id uuid.UUID) bool {
	t.Helper()
	var exists bool
	if err := pool.QueryRow(context.Background(),
		`SELECT EXISTS(SELECT 1 FROM async_jobs WHERE id = $1)`, id,
	).Scan(&exists); err != nil {
		t.Fatalf("jobRowExists: %v", err)
	}
	return exists
}

// TestReaper_Sweep_TableDriven proves the reaper's core contract: terminal
// rows older than retention are deleted, terminal rows within retention are
// kept, and queued/running rows are NEVER deleted regardless of age.
func TestReaper_Sweep_TableDriven(t *testing.T) {
	pool := newTestPostgres(t)
	custA, keyA := seedCustomer(t, pool, "reaper-"+uuid.New().String()+"@example.com")

	const retention = time.Hour

	cases := []struct {
		name        string
		status      string
		age         time.Duration
		wantDeleted bool
	}{
		{"succeeded past retention is deleted", StatusSucceeded, retention + time.Minute, true},
		{"failed past retention is deleted", StatusFailed, retention + time.Minute, true},
		{"succeeded within retention is kept", StatusSucceeded, retention - time.Minute, false},
		{"failed within retention is kept", StatusFailed, retention - time.Minute, false},
		{"queued row is never deleted regardless of age", StatusQueued, retention + 24*time.Hour, false},
		{"running row is never deleted regardless of age", StatusRunning, retention + 24*time.Hour, false},
	}

	ids := make(map[string]uuid.UUID, len(cases))
	for _, c := range cases {
		ids[c.name] = seedJobWithStatusAge(t, pool, custA, keyA, c.status, c.age)
	}

	r := NewReaper(pool, retention, time.Hour)
	r.sweep(context.Background())

	for _, c := range cases {
		gotExists := jobRowExists(t, pool, ids[c.name])
		wantExists := !c.wantDeleted
		if gotExists != wantExists {
			t.Errorf("%s: row exists=%v, want %v", c.name, gotExists, wantExists)
		}
	}
}

// TestReaper_Sweep_KeysOffUpdatedAt_NotCreatedAt proves a job that sat
// queued/running long past retention but terminalized recently is kept —
// retention must measure age since the terminal transition (updated_at,
// stamped by Store.Complete/Fail/DeadLetter), not since enqueue (created_at).
// Keying off created_at would delete such a row the instant it terminalizes,
// sometimes before a customer ever observes the result via GET /v1/jobs/{id}.
func TestReaper_Sweep_KeysOffUpdatedAt_NotCreatedAt(t *testing.T) {
	pool := newTestPostgres(t)
	custA, keyA := seedCustomer(t, pool, "reaper-updated-at-"+uuid.New().String()+"@example.com")

	const retention = time.Hour

	s := NewStore(pool)
	id, err := s.Enqueue(context.Background(), custA, keyA, "echo", "req-old-enqueue", "free", json.RawMessage(`{}`), 0, "")
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	// Simulate a job that sat queued/running for much longer than retention,
	// then just terminalized: created_at is far in the past, but updated_at
	// (the terminal-transition stamp) is recent.
	if _, err := pool.Exec(context.Background(), `
		UPDATE async_jobs
		SET status = $2, created_at = NOW() - $3 * INTERVAL '1 second', updated_at = NOW()
		WHERE id = $1
	`, id, StatusSucceeded, (retention * 10).Seconds()); err != nil {
		t.Fatalf("backdate created_at only: %v", err)
	}

	r := NewReaper(pool, retention, time.Hour)
	r.sweep(context.Background())

	if !jobRowExists(t, pool, id) {
		t.Error("row with old created_at but recent updated_at was deleted; retention must key off updated_at (terminalization time), not created_at (enqueue time)")
	}
}

// TestReaper_Sweep_BatchedAcrossMultipleDeletes proves a single sweep call
// drains a backlog larger than one batch by looping until a batch comes back
// short — mirrors the usage flusher's batchPageSize-bounded phases.
func TestReaper_Sweep_BatchedAcrossMultipleDeletes(t *testing.T) {
	pool := newTestPostgres(t)
	custA, keyA := seedCustomer(t, pool, "reaper-batch-"+uuid.New().String()+"@example.com")

	const retention = time.Hour
	const batchSize = 2
	const numRows = 2*batchSize + 1 // spans three batches: 2, 2, 1

	ids := make([]uuid.UUID, 0, numRows)
	for i := 0; i < numRows; i++ {
		ids = append(ids, seedJobWithStatusAge(t, pool, custA, keyA, StatusSucceeded, retention+time.Minute))
	}

	r := NewReaper(pool, retention, time.Hour)
	r.batchSize = batchSize
	r.sweep(context.Background())

	for _, id := range ids {
		if jobRowExists(t, pool, id) {
			t.Errorf("row %s survived a sweep that should have drained the entire backlog across batches", id)
		}
	}
}

// TestReaper_Run_NilSafe proves Run is a safe no-op — returning immediately
// without starting a ticker or touching the database — for a nil receiver, a
// nil db, and a non-positive retention, mirroring usage.Flusher.Run and
// jobs.Executor.Run's nil-safe pattern.
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

	pool := newTestPostgres(t)
	zeroRetention := NewReaper(pool, 0, time.Second)
	mustReturnImmediately("retention <= 0", func() { zeroRetention.Run(context.Background()) })

	negativeRetention := NewReaper(pool, -time.Hour, time.Second)
	mustReturnImmediately("negative retention", func() { negativeRetention.Run(context.Background()) })
}
