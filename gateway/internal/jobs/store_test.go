package jobs

import (
	"context"
	"encoding/json"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Unluckyathecking/crucible/gateway/internal/db"
)

// newTestPostgres mirrors the pattern used across gateway/internal (e.g.
// webhookout/replay_test.go, operator/store_test.go): skip when Postgres is
// unreachable, unless the DSN was explicitly requested (CI), in which case
// failure is fatal. Also applies migrations (idempotent — invariant #8) so
// the suite is self-contained regardless of external setup.
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
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
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
	if err := db.Apply(ctx, pool); err != nil {
		pool.Close()
		t.Fatalf("apply migrations: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// seedCustomer inserts a minimal customers + api_keys row pair and registers
// cleanup. Returns (customerID, apiKeyID).
func seedCustomer(t *testing.T, pool *pgxpool.Pool, email string) (uuid.UUID, uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	var custID uuid.UUID
	if err := pool.QueryRow(ctx,
		`INSERT INTO customers (email, plan_id) VALUES ($1, 'free') RETURNING id`, email,
	).Scan(&custID); err != nil {
		t.Fatalf("seedCustomer: %v", err)
	}
	var keyID uuid.UUID
	prefix := "cru_test_" + uuid.New().String()[:8]
	if err := pool.QueryRow(ctx,
		`INSERT INTO api_keys (customer_id, prefix, hash) VALUES ($1, $2, '\x00') RETURNING id`, custID, prefix,
	).Scan(&keyID); err != nil {
		t.Fatalf("seedCustomer: insert api_key: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM customers WHERE id = $1`, custID)
	})
	return custID, keyID
}

func TestStore_NewStore_NilDB(t *testing.T) {
	if s := NewStore(nil); s != nil {
		t.Fatalf("NewStore(nil) = %v, want nil", s)
	}
}

func TestStore_NilReceiver_SafeNoop(t *testing.T) {
	var s *Store
	if _, ok, err := s.Get(context.Background(), uuid.New(), uuid.New()); ok || err != nil {
		t.Errorf("nil Store.Get: got (ok=%v, err=%v), want (false, nil)", ok, err)
	}
	if jobs, err := s.Claim(context.Background(), 5, uuid.New()); jobs != nil || err != nil {
		t.Errorf("nil Store.Claim: got (%v, %v), want (nil, nil)", jobs, err)
	}
	if n, err := s.ReleaseClaimed(context.Background(), uuid.New()); n != 0 || err != nil {
		t.Errorf("nil Store.ReleaseClaimed: got (%d, %v), want (0, nil)", n, err)
	}
	if _, err := s.Enqueue(context.Background(), uuid.New(), uuid.New(), "op", "rid", "free", json.RawMessage(`{}`), 0); err == nil {
		t.Error("nil Store.Enqueue: want error")
	}
}

func TestStore_EnqueueGet_CustomerScoped(t *testing.T) {
	pool := newTestPostgres(t)
	s := NewStore(pool)
	custA, keyA := seedCustomer(t, pool, "jobs-store-a-"+uuid.New().String()+"@example.com")
	custB, _ := seedCustomer(t, pool, "jobs-store-b-"+uuid.New().String()+"@example.com")

	id, err := s.Enqueue(context.Background(), custA, keyA, "echo", "req-1", "free", json.RawMessage(`{"in":1}`), 0)
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	job, ok, err := s.Get(context.Background(), id, custA)
	if err != nil || !ok {
		t.Fatalf("Get(owner): ok=%v err=%v", ok, err)
	}
	if job.Status != StatusQueued {
		t.Errorf("status = %q, want %q", job.Status, StatusQueued)
	}
	if job.Operation != "echo" {
		t.Errorf("operation = %q, want echo", job.Operation)
	}

	// IDOR: a different customer's Get for the same job id must report not-found,
	// not the job. This is the SQL-level scoping (AND customer_id = $2), not a
	// post-fetch check.
	if _, ok, err := s.Get(context.Background(), id, custB); ok || err != nil {
		t.Fatalf("Get(other customer): ok=%v err=%v, want (false, nil)", ok, err)
	}

	// Nonexistent id is indistinguishable from another customer's id.
	if _, ok, err := s.Get(context.Background(), uuid.New(), custA); ok || err != nil {
		t.Fatalf("Get(nonexistent): ok=%v err=%v, want (false, nil)", ok, err)
	}
}

func TestStore_Claim_MarksRunningAndScansFields(t *testing.T) {
	pool := newTestPostgres(t)
	s := NewStore(pool)
	custA, keyA := seedCustomer(t, pool, "jobs-claim-"+uuid.New().String()+"@example.com")

	id, err := s.Enqueue(context.Background(), custA, keyA, "echo", "req-1", "free", json.RawMessage(`{"in":1}`), 30)
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	instance := uuid.New()
	claimed, err := s.Claim(context.Background(), 10, instance)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	var found *Job
	for i := range claimed {
		if claimed[i].ID == id {
			found = &claimed[i]
		}
	}
	if found == nil {
		t.Fatalf("Claim did not return enqueued job %s (got %d jobs)", id, len(claimed))
	}
	if found.Status != StatusRunning {
		t.Errorf("status = %q, want %q", found.Status, StatusRunning)
	}
	if found.Operation != "echo" || found.RequestID != "req-1" || found.Plan != "free" {
		t.Errorf("claimed job fields mismatch: %+v", found)
	}
	if found.TimeoutSeconds != 30 {
		t.Errorf("timeout_seconds = %d, want 30", found.TimeoutSeconds)
	}
	// JSONB round-trips through Postgres's canonical text form (e.g. added
	// whitespace), so compare decoded values rather than raw bytes.
	var payload map[string]int
	if err := json.Unmarshal(found.Payload, &payload); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if payload["in"] != 1 {
		t.Errorf("payload = %s, want {\"in\":1}", found.Payload)
	}

	// A second claim must not return the same job again — it's already 'running'.
	claimed2, err := s.Claim(context.Background(), 10, instance)
	if err != nil {
		t.Fatalf("Claim (2nd): %v", err)
	}
	for _, j := range claimed2 {
		if j.ID == id {
			t.Fatalf("job %s claimed twice", id)
		}
	}
}

// TestStore_Claim_SkipLocked_NoDoubleClaim is the race-sensitive test: N
// concurrent Claim(limit=1) callers against M queued jobs must together claim
// exactly M distinct jobs, never the same job twice — the FOR UPDATE SKIP
// LOCKED contract this Store depends on for multi-replica safety.
func TestStore_Claim_SkipLocked_NoDoubleClaim(t *testing.T) {
	pool := newTestPostgres(t)
	s := NewStore(pool)
	custA, keyA := seedCustomer(t, pool, "jobs-race-"+uuid.New().String()+"@example.com")

	const numJobs = 12
	ids := make(map[uuid.UUID]bool, numJobs)
	var idsMu sync.Mutex
	for i := 0; i < numJobs; i++ {
		id, err := s.Enqueue(context.Background(), custA, keyA, "echo", "req", "free", json.RawMessage(`{}`), 0)
		if err != nil {
			t.Fatalf("Enqueue: %v", err)
		}
		idsMu.Lock()
		ids[id] = true
		idsMu.Unlock()
	}

	var (
		wg          sync.WaitGroup
		mu          sync.Mutex
		claimedByID = make(map[uuid.UUID]int)
		totalClaims int64
	)
	for w := 0; w < numJobs; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			claimed, err := s.Claim(context.Background(), 1, uuid.New())
			if err != nil {
				t.Errorf("Claim: %v", err)
				return
			}
			if len(claimed) == 0 {
				return
			}
			atomic.AddInt64(&totalClaims, int64(len(claimed)))
			mu.Lock()
			for _, j := range claimed {
				claimedByID[j.ID]++
			}
			mu.Unlock()
		}()
	}
	wg.Wait()

	if totalClaims != numJobs {
		t.Errorf("total claims = %d, want %d", totalClaims, numJobs)
	}
	for id, count := range claimedByID {
		if !ids[id] {
			t.Errorf("claimed unexpected job id %s", id)
		}
		if count != 1 {
			t.Errorf("job %s claimed %d times, want 1 (SKIP LOCKED violated)", id, count)
		}
	}
	if len(claimedByID) != numJobs {
		t.Errorf("distinct claimed jobs = %d, want %d", len(claimedByID), numJobs)
	}
}

func TestStore_Claim_StuckJobRecovery(t *testing.T) {
	pool := newTestPostgres(t)
	s := NewStore(pool)
	custA, keyA := seedCustomer(t, pool, "jobs-stuck-"+uuid.New().String()+"@example.com")

	id, err := s.Enqueue(context.Background(), custA, keyA, "echo", "req", "free", json.RawMessage(`{}`), 0)
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	// Simulate a crashed process: mark the row 'running' with a claimed_at far
	// enough in the past that Claim's crash-recovery sweep treats it as abandoned.
	staleInstance := uuid.New()
	if _, err := pool.Exec(context.Background(), `
		UPDATE async_jobs SET status = 'running', claimed_at = NOW() - INTERVAL '10 minutes', claimed_by = $2
		WHERE id = $1
	`, id, staleInstance); err != nil {
		t.Fatalf("seed stuck row: %v", err)
	}

	claimed, err := s.Claim(context.Background(), 10, uuid.New())
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	var recovered bool
	for _, j := range claimed {
		if j.ID == id {
			recovered = true
		}
	}
	if !recovered {
		t.Fatalf("stuck job %s was not recovered and re-claimed", id)
	}
}

func TestStore_Complete(t *testing.T) {
	pool := newTestPostgres(t)
	s := NewStore(pool)
	custA, keyA := seedCustomer(t, pool, "jobs-complete-"+uuid.New().String()+"@example.com")
	id, err := s.Enqueue(context.Background(), custA, keyA, "echo", "req", "free", json.RawMessage(`{}`), 0)
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if _, err := s.Claim(context.Background(), 10, uuid.New()); err != nil {
		t.Fatalf("Claim: %v", err)
	}

	if err := s.Complete(context.Background(), id, json.RawMessage(`{"out":true}`), 3, "pages"); err != nil {
		t.Fatalf("Complete: %v", err)
	}

	job, ok, err := s.Get(context.Background(), id, custA)
	if err != nil || !ok {
		t.Fatalf("Get: ok=%v err=%v", ok, err)
	}
	if job.Status != StatusSucceeded {
		t.Errorf("status = %q, want %q", job.Status, StatusSucceeded)
	}
	if job.BillableUnits != 3 {
		t.Errorf("billable_units = %d, want 3", job.BillableUnits)
	}
	if job.UnitsLabel != "pages" {
		t.Errorf("units_label = %q, want pages", job.UnitsLabel)
	}
	var result map[string]bool
	if err := json.Unmarshal(job.Result, &result); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if !result["out"] {
		t.Errorf("result = %s, want {\"out\":true}", job.Result)
	}
}

func TestStore_Fail(t *testing.T) {
	pool := newTestPostgres(t)
	s := NewStore(pool)
	custA, keyA := seedCustomer(t, pool, "jobs-fail-"+uuid.New().String()+"@example.com")
	id, err := s.Enqueue(context.Background(), custA, keyA, "echo", "req", "free", json.RawMessage(`{}`), 0)
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	if err := s.Fail(context.Background(), id, "WORKER_BAD_RESPONSE", "worker contract violation"); err != nil {
		t.Fatalf("Fail: %v", err)
	}

	job, ok, err := s.Get(context.Background(), id, custA)
	if err != nil || !ok {
		t.Fatalf("Get: ok=%v err=%v", ok, err)
	}
	if job.Status != StatusFailed {
		t.Errorf("status = %q, want %q", job.Status, StatusFailed)
	}
	if job.ErrorCode != "WORKER_BAD_RESPONSE" || job.ErrorMessage != "worker contract violation" {
		t.Errorf("error fields mismatch: code=%q message=%q", job.ErrorCode, job.ErrorMessage)
	}
}

func TestStore_ReleaseClaimed_ScopedToInstance(t *testing.T) {
	pool := newTestPostgres(t)
	s := NewStore(pool)
	custA, keyA := seedCustomer(t, pool, "jobs-release-"+uuid.New().String()+"@example.com")

	idA, err := s.Enqueue(context.Background(), custA, keyA, "echo", "req", "free", json.RawMessage(`{}`), 0)
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	idB, err := s.Enqueue(context.Background(), custA, keyA, "echo", "req", "free", json.RawMessage(`{}`), 0)
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	instanceA := uuid.New()
	instanceB := uuid.New()
	// Claim each job under a distinct instance by claiming one at a time —
	// Claim assigns the same instanceID to every row it claims in one call,
	// so two separate calls (different instance ids) are used here.
	if _, err := s.Claim(context.Background(), 1, instanceA); err != nil {
		t.Fatalf("Claim A: %v", err)
	}
	if _, err := s.Claim(context.Background(), 1, instanceB); err != nil {
		t.Fatalf("Claim B: %v", err)
	}

	n, err := s.ReleaseClaimed(context.Background(), instanceA)
	if err != nil {
		t.Fatalf("ReleaseClaimed: %v", err)
	}
	if n != 1 {
		t.Errorf("released = %d, want 1", n)
	}

	// Whichever job instanceA claimed is back to queued; the other (claimed by
	// instanceB) must be untouched — ReleaseClaimed must never release another
	// instance's in-flight work.
	var queuedCount, runningCount int
	for _, id := range []uuid.UUID{idA, idB} {
		job, ok, err := s.Get(context.Background(), id, custA)
		if err != nil || !ok {
			t.Fatalf("Get(%s): ok=%v err=%v", id, ok, err)
		}
		switch job.Status {
		case StatusQueued:
			queuedCount++
		case StatusRunning:
			runningCount++
		default:
			t.Errorf("job %s has unexpected status %q", id, job.Status)
		}
	}
	if queuedCount != 1 || runningCount != 1 {
		t.Errorf("queued=%d running=%d, want 1 and 1", queuedCount, runningCount)
	}
}

func TestStore_Requeue(t *testing.T) {
	pool := newTestPostgres(t)
	s := NewStore(pool)
	custA, keyA := seedCustomer(t, pool, "jobs-requeue-"+uuid.New().String()+"@example.com")
	id, err := s.Enqueue(context.Background(), custA, keyA, "echo", "req", "free", json.RawMessage(`{}`), 0)
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if _, err := s.Claim(context.Background(), 10, uuid.New()); err != nil {
		t.Fatalf("Claim: %v", err)
	}

	if err := s.Requeue(context.Background(), id); err != nil {
		t.Fatalf("Requeue: %v", err)
	}

	job, ok, err := s.Get(context.Background(), id, custA)
	if err != nil || !ok {
		t.Fatalf("Get: ok=%v err=%v", ok, err)
	}
	if job.Status != StatusQueued {
		t.Errorf("status = %q, want %q", job.Status, StatusQueued)
	}
}
