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
		// async_jobs.customer_id/api_key_id reference customers/api_keys
		// without ON DELETE CASCADE, so deleting the customer first would
		// silently fail (error discarded below) and leave queued/running
		// rows behind. Store.Claim claims globally, not scoped to a test's
		// customer, so a stale queued row from an earlier test run's failed
		// cleanup could be claimed by a later run and break its
		// distinct-claim-count assertions. Delete async_jobs first.
		ctx := context.Background()
		_, _ = pool.Exec(ctx, `DELETE FROM async_jobs WHERE customer_id = $1`, custID)
		_, _ = pool.Exec(ctx, `DELETE FROM customers WHERE id = $1`, custID)
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
	if jobs, err := s.Claim(context.Background(), 5, uuid.New(), 0); jobs != nil || err != nil {
		t.Errorf("nil Store.Claim: got (%v, %v), want (nil, nil)", jobs, err)
	}
	if n, err := s.ReleaseClaimed(context.Background(), uuid.New()); n != 0 || err != nil {
		t.Errorf("nil Store.ReleaseClaimed: got (%d, %v), want (0, nil)", n, err)
	}
	if _, err := s.Enqueue(context.Background(), uuid.New(), uuid.New(), "op", "rid", "free", json.RawMessage(`{}`), 0, ""); err == nil {
		t.Error("nil Store.Enqueue: want error")
	}
}

func TestStore_EnqueueGet_CustomerScoped(t *testing.T) {
	pool := newTestPostgres(t)
	s := NewStore(pool)
	custA, keyA := seedCustomer(t, pool, "jobs-store-a-"+uuid.New().String()+"@example.com")
	custB, _ := seedCustomer(t, pool, "jobs-store-b-"+uuid.New().String()+"@example.com")

	id, err := s.Enqueue(context.Background(), custA, keyA, "echo", "req-1", "free", json.RawMessage(`{"in":1}`), 0, "")
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

// TestStore_Enqueue_IdempotencyKey_ReturnsExistingJob proves the scenario
// idempotency.Middleware's finalize failure can trigger: a client retry
// with the same Idempotency-Key reaching enqueueAsync a second time must
// get back the FIRST job's id, not create a second job (which would let
// the worker run — and bill — twice for what the client intended as one
// request).
func TestStore_Enqueue_IdempotencyKey_ReturnsExistingJob(t *testing.T) {
	pool := newTestPostgres(t)
	s := NewStore(pool)
	custA, keyA := seedCustomer(t, pool, "jobs-idem-"+uuid.New().String()+"@example.com")

	id1, err := s.Enqueue(context.Background(), custA, keyA, "echo", "req-1", "free", json.RawMessage(`{"in":1}`), 0, "idem-key-1")
	if err != nil {
		t.Fatalf("Enqueue (1st): %v", err)
	}

	// Simulate a client retry with the same Idempotency-Key: request_id and
	// payload may differ slightly (a real retry re-sends the same logical
	// request, but Enqueue must dedupe on (customer, key) regardless).
	id2, err := s.Enqueue(context.Background(), custA, keyA, "echo", "req-2", "free", json.RawMessage(`{"in":1}`), 0, "idem-key-1")
	if err != nil {
		t.Fatalf("Enqueue (retry): %v", err)
	}
	if id2 != id1 {
		t.Fatalf("Enqueue retry with same idempotency key returned a different job: %s != %s", id2, id1)
	}

	var count int64
	if err := pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM async_jobs WHERE customer_id = $1 AND operation = 'echo'`, custA,
	).Scan(&count); err != nil {
		t.Fatalf("count async_jobs: %v", err)
	}
	if count != 1 {
		t.Errorf("async_jobs rows for customer = %d, want 1 (retry must not create a second job)", count)
	}

	// A different customer using the SAME key string must NOT collide —
	// the unique index is scoped to (customer_id, idempotency_key).
	custB, keyB := seedCustomer(t, pool, "jobs-idem-b-"+uuid.New().String()+"@example.com")
	id3, err := s.Enqueue(context.Background(), custB, keyB, "echo", "req-3", "free", json.RawMessage(`{}`), 0, "idem-key-1")
	if err != nil {
		t.Fatalf("Enqueue (other customer, same key): %v", err)
	}
	if id3 == id1 {
		t.Fatal("Enqueue with the same idempotency key but a different customer returned the same job")
	}

	// Multiple enqueues with NO idempotency key must never collide with
	// each other (they'd all store NULL, which the partial unique index
	// deliberately excludes).
	idNoKeyA, err := s.Enqueue(context.Background(), custA, keyA, "echo", "req-4", "free", json.RawMessage(`{}`), 0, "")
	if err != nil {
		t.Fatalf("Enqueue (no key, 1st): %v", err)
	}
	idNoKeyB, err := s.Enqueue(context.Background(), custA, keyA, "echo", "req-5", "free", json.RawMessage(`{}`), 0, "")
	if err != nil {
		t.Fatalf("Enqueue (no key, 2nd): %v", err)
	}
	if idNoKeyA == idNoKeyB {
		t.Fatal("two key-less enqueues collided with each other")
	}
}

func TestStore_List_CustomerScopedAndOrdered(t *testing.T) {
	pool := newTestPostgres(t)
	s := NewStore(pool)
	custA, keyA := seedCustomer(t, pool, "jobs-list-a-"+uuid.New().String()+"@example.com")
	custB, keyB := seedCustomer(t, pool, "jobs-list-b-"+uuid.New().String()+"@example.com")

	idOld, err := s.Enqueue(context.Background(), custA, keyA, "echo", "req-old", "free", json.RawMessage(`{}`), 0, "")
	if err != nil {
		t.Fatalf("Enqueue (old): %v", err)
	}
	idNew, err := s.Enqueue(context.Background(), custA, keyA, "echo", "req-new", "free", json.RawMessage(`{}`), 0, "")
	if err != nil {
		t.Fatalf("Enqueue (new): %v", err)
	}
	// Back-date the first row so ordering isn't dependent on same-timestamp ties.
	if _, err := pool.Exec(context.Background(),
		`UPDATE async_jobs SET created_at = created_at - INTERVAL '1 hour' WHERE id = $1`, idOld,
	); err != nil {
		t.Fatalf("backdate: %v", err)
	}

	// Another customer's job must never appear in custA's list — IDOR scope,
	// matching Get's SQL-level customer_id filter.
	if _, err := s.Enqueue(context.Background(), custB, keyB, "echo", "req-other", "free", json.RawMessage(`{}`), 0, ""); err != nil {
		t.Fatalf("Enqueue (other customer): %v", err)
	}

	jobsList, total, err := s.List(context.Background(), custA, nil, nil, 50, 0)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if total != 2 {
		t.Fatalf("total = %d, want 2", total)
	}
	if len(jobsList) != 2 {
		t.Fatalf("len(jobsList) = %d, want 2", len(jobsList))
	}
	if jobsList[0].ID != idNew || jobsList[1].ID != idOld {
		t.Errorf("List order = [%s, %s], want newest-first [%s, %s]", jobsList[0].ID, jobsList[1].ID, idNew, idOld)
	}
	for _, j := range jobsList {
		if j.CustomerID != custA {
			t.Errorf("List(custA) returned a job owned by %s", j.CustomerID)
		}
	}
}

func TestStore_List_StatusFilter(t *testing.T) {
	pool := newTestPostgres(t)
	s := NewStore(pool)
	custA, keyA := seedCustomer(t, pool, "jobs-list-status-"+uuid.New().String()+"@example.com")

	queuedID, err := s.Enqueue(context.Background(), custA, keyA, "echo", "req-queued", "free", json.RawMessage(`{}`), 0, "")
	if err != nil {
		t.Fatalf("Enqueue (queued): %v", err)
	}
	failedID, err := s.Enqueue(context.Background(), custA, keyA, "echo", "req-failed", "free", json.RawMessage(`{}`), 0, "")
	if err != nil {
		t.Fatalf("Enqueue (failed): %v", err)
	}
	if err := s.Fail(context.Background(), failedID, "WORKER_BAD_RESPONSE", "worker contract violation"); err != nil {
		t.Fatalf("Fail: %v", err)
	}

	failedStatus := StatusFailed
	jobsList, total, err := s.List(context.Background(), custA, &failedStatus, nil, 50, 0)
	if err != nil {
		t.Fatalf("List(status=failed): %v", err)
	}
	if total != 1 || len(jobsList) != 1 {
		t.Fatalf("List(status=failed) = %d rows (total=%d), want 1", len(jobsList), total)
	}
	if jobsList[0].ID != failedID {
		t.Errorf("List(status=failed) returned job %s, want %s", jobsList[0].ID, failedID)
	}
	if jobsList[0].ID == queuedID {
		t.Error("List(status=failed) returned the still-queued job")
	}
}

func TestStore_List_Pagination(t *testing.T) {
	pool := newTestPostgres(t)
	s := NewStore(pool)
	custA, keyA := seedCustomer(t, pool, "jobs-list-page-"+uuid.New().String()+"@example.com")

	const numJobs = 5
	for i := 0; i < numJobs; i++ {
		if _, err := s.Enqueue(context.Background(), custA, keyA, "echo", "req", "free", json.RawMessage(`{}`), 0, ""); err != nil {
			t.Fatalf("Enqueue: %v", err)
		}
	}

	page1, total, err := s.List(context.Background(), custA, nil, nil, 2, 0)
	if err != nil {
		t.Fatalf("List(page1): %v", err)
	}
	if total != numJobs {
		t.Fatalf("total = %d, want %d", total, numJobs)
	}
	if len(page1) != 2 {
		t.Fatalf("len(page1) = %d, want 2", len(page1))
	}

	page2, _, err := s.List(context.Background(), custA, nil, nil, 2, 2)
	if err != nil {
		t.Fatalf("List(page2): %v", err)
	}
	if len(page2) != 2 {
		t.Fatalf("len(page2) = %d, want 2", len(page2))
	}
	if page1[0].ID == page2[0].ID || page1[1].ID == page2[0].ID {
		t.Error("List pages overlap: same job returned on page1 and page2")
	}
}

func TestStore_List_NilReceiver(t *testing.T) {
	var s *Store
	jobsList, total, err := s.List(context.Background(), uuid.New(), nil, nil, 10, 0)
	if jobsList != nil || total != 0 || err != nil {
		t.Errorf("nil Store.List: got (%v, %d, %v), want (nil, 0, nil)", jobsList, total, err)
	}
}

func TestStore_Claim_MarksRunningAndScansFields(t *testing.T) {
	pool := newTestPostgres(t)
	s := NewStore(pool)
	custA, keyA := seedCustomer(t, pool, "jobs-claim-"+uuid.New().String()+"@example.com")

	id, err := s.Enqueue(context.Background(), custA, keyA, "echo", "req-1", "free", json.RawMessage(`{"in":1}`), 30, "")
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	instance := uuid.New()
	claimed, err := s.Claim(context.Background(), 10, instance, 0)
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
	claimed2, err := s.Claim(context.Background(), 10, instance, 0)
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
		id, err := s.Enqueue(context.Background(), custA, keyA, "echo", "req", "free", json.RawMessage(`{}`), 0, "")
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
			claimed, err := s.Claim(context.Background(), 1, uuid.New(), 0)
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

// TestStore_Claim_FairnessPreventsBacklogStarvation is the acceptance test for
// the fair-claim path: with maxInflightPerCustomer disabled (0), a customer B
// job enqueued after customer A's deep backlog only gets claimed once A's
// backlog has drained below the pool's free-slot count. With the cap enabled,
// the exact same queue state claims B within the very first Claim call.
func TestStore_Claim_FairnessPreventsBacklogStarvation(t *testing.T) {
	pool := newTestPostgres(t)
	s := NewStore(pool)

	run := func(t *testing.T, maxInflightPerCustomer int) bool {
		custA, keyA := seedCustomer(t, pool, "jobs-fair-a-"+uuid.New().String()+"@example.com")
		custB, keyB := seedCustomer(t, pool, "jobs-fair-b-"+uuid.New().String()+"@example.com")

		const backlogSize = 5
		const poolFreeSlots = 3
		for i := 0; i < backlogSize; i++ {
			if _, err := s.Enqueue(context.Background(), custA, keyA, "echo", "req-a", "free", json.RawMessage(`{}`), 0, ""); err != nil {
				t.Fatalf("Enqueue (A): %v", err)
			}
		}
		idB, err := s.Enqueue(context.Background(), custB, keyB, "echo", "req-b", "free", json.RawMessage(`{}`), 0, "")
		if err != nil {
			t.Fatalf("Enqueue (B): %v", err)
		}

		claimed, err := s.Claim(context.Background(), poolFreeSlots, uuid.New(), maxInflightPerCustomer)
		if err != nil {
			t.Fatalf("Claim: %v", err)
		}
		for _, j := range claimed {
			if j.ID == idB {
				return true
			}
		}
		return false
	}

	t.Run("disabled_starves_B", func(t *testing.T) {
		if run(t, 0) {
			t.Fatalf("customer B claimed within the first cycle even though the fairness cap is disabled — pure-FIFO should have exhausted the pool on A's backlog first")
		}
	})

	t.Run("enabled_claims_B_within_first_cycle", func(t *testing.T) {
		if !run(t, 1) {
			t.Fatalf("customer B was not claimed within the first cycle despite maxInflightPerCustomer=1 — fairness cap did not protect against A's backlog")
		}
	})
}

// TestStore_Claim_MaxInflightPerCustomer_RaceEnforced is the -race test for
// the per-customer cap itself: many concurrent Claim callers (simulating
// concurrent gateway replicas, mirroring TestStore_Claim_SkipLocked_NoDoubleClaim's
// shape) against one customer's deep backlog must never let that customer
// exceed maxInflightPerCustomer simultaneously 'running' rows, while a second
// customer's jobs still make progress concurrently.
func TestStore_Claim_MaxInflightPerCustomer_RaceEnforced(t *testing.T) {
	pool := newTestPostgres(t)
	s := NewStore(pool)
	custA, keyA := seedCustomer(t, pool, "jobs-fair-race-a-"+uuid.New().String()+"@example.com")
	custB, keyB := seedCustomer(t, pool, "jobs-fair-race-b-"+uuid.New().String()+"@example.com")

	const (
		numJobsA               = 30
		numJobsB               = 10
		maxInflightPerCustomer = 3
		numWorkers             = 16
	)
	for i := 0; i < numJobsA; i++ {
		if _, err := s.Enqueue(context.Background(), custA, keyA, "echo", "req-a", "free", json.RawMessage(`{}`), 0, ""); err != nil {
			t.Fatalf("Enqueue (A): %v", err)
		}
	}
	for i := 0; i < numJobsB; i++ {
		if _, err := s.Enqueue(context.Background(), custB, keyB, "echo", "req-b", "free", json.RawMessage(`{}`), 0, ""); err != nil {
			t.Fatalf("Enqueue (B): %v", err)
		}
	}

	var (
		wg          sync.WaitGroup
		mu          sync.Mutex
		maxSeenA    int
		claimedIDs  = make(map[uuid.UUID]bool)
		claimedForB int
	)
	checkRunningCap := func() {
		var n int
		if err := pool.QueryRow(context.Background(),
			`SELECT COUNT(*) FROM async_jobs WHERE customer_id = $1 AND status = 'running'`, custA,
		).Scan(&n); err != nil {
			t.Errorf("count running: %v", err)
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
			for i := 0; i < 8; i++ {
				claimed, err := s.Claim(context.Background(), 2, uuid.New(), maxInflightPerCustomer)
				if err != nil {
					t.Errorf("Claim: %v", err)
					return
				}
				checkRunningCap()
				mu.Lock()
				for _, j := range claimed {
					claimedIDs[j.ID] = true
					if j.CustomerID == custB {
						claimedForB++
					}
				}
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	if maxSeenA > maxInflightPerCustomer {
		t.Errorf("customer A had %d rows simultaneously running, want <= %d (maxInflightPerCustomer)", maxSeenA, maxInflightPerCustomer)
	}
	if claimedForB == 0 {
		t.Error("customer B's jobs never progressed while A's backlog was being claimed — fairness cap did not protect B")
	}
	if len(claimedIDs) == 0 {
		t.Error("no jobs claimed at all")
	}
}

// TestStore_Claim_NeverClaimsCancelled proves a cancelled job can never be
// claimed/executed by jobs.Executor and so never bills any units — Claim's
// scan is WHERE status = 'queued', which a cancelled row no longer matches
// the instant CancelQueued transitions it.
func TestStore_Claim_NeverClaimsCancelled(t *testing.T) {
	pool := newTestPostgres(t)
	s := NewStore(pool)
	custA, keyA := seedCustomer(t, pool, "jobs-cancel-claim-"+uuid.New().String()+"@example.com")

	id, err := s.Enqueue(context.Background(), custA, keyA, "echo", "req", "free", json.RawMessage(`{}`), 0, "")
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	ok, err := s.CancelQueued(context.Background(), id, custA)
	if err != nil || !ok {
		t.Fatalf("CancelQueued: ok=%v err=%v", ok, err)
	}

	claimed, err := s.Claim(context.Background(), 10, uuid.New(), 0)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	for _, j := range claimed {
		if j.ID == id {
			t.Fatalf("Claim returned cancelled job %s", id)
		}
	}

	job, found, err := s.Get(context.Background(), id, custA)
	if err != nil || !found {
		t.Fatalf("Get: found=%v err=%v", found, err)
	}
	if job.Status != StatusCancelled {
		t.Errorf("status = %q, want %q", job.Status, StatusCancelled)
	}
	if job.BillableUnits != 0 {
		t.Errorf("billable_units = %d, want 0 (a cancelled job must never bill)", job.BillableUnits)
	}
}

func TestStore_Claim_StuckJobRecovery(t *testing.T) {
	pool := newTestPostgres(t)
	s := NewStore(pool)
	custA, keyA := seedCustomer(t, pool, "jobs-stuck-"+uuid.New().String()+"@example.com")

	id, err := s.Enqueue(context.Background(), custA, keyA, "echo", "req", "free", json.RawMessage(`{}`), 0, "")
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

	claimed, err := s.Claim(context.Background(), 10, uuid.New(), 0)
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
	id, err := s.Enqueue(context.Background(), custA, keyA, "echo", "req", "free", json.RawMessage(`{}`), 0, "")
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if _, err := s.Claim(context.Background(), 10, uuid.New(), 0); err != nil {
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
	id, err := s.Enqueue(context.Background(), custA, keyA, "echo", "req", "free", json.RawMessage(`{}`), 0, "")
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

	// Fail is the deterministic-failure path (a worker structured business
	// error, or a billable_units<1 contract violation) — neither is retried,
	// so attempts must stay exactly as it was, unlike DeadLetter which
	// records the exhausted retry count.
	var attempts int
	if err := pool.QueryRow(context.Background(),
		`SELECT attempts FROM async_jobs WHERE id = $1`, id,
	).Scan(&attempts); err != nil {
		t.Fatalf("query attempts: %v", err)
	}
	if attempts != 0 {
		t.Errorf("attempts = %d, want 0 (Fail must not touch attempts)", attempts)
	}
}

func TestStore_ReleaseClaimed_ScopedToInstance(t *testing.T) {
	pool := newTestPostgres(t)
	s := NewStore(pool)
	custA, keyA := seedCustomer(t, pool, "jobs-release-"+uuid.New().String()+"@example.com")

	idA, err := s.Enqueue(context.Background(), custA, keyA, "echo", "req", "free", json.RawMessage(`{}`), 0, "")
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	idB, err := s.Enqueue(context.Background(), custA, keyA, "echo", "req", "free", json.RawMessage(`{}`), 0, "")
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	instanceA := uuid.New()
	instanceB := uuid.New()
	// Claim each job under a distinct instance by claiming one at a time —
	// Claim assigns the same instanceID to every row it claims in one call,
	// so two separate calls (different instance ids) are used here.
	if _, err := s.Claim(context.Background(), 1, instanceA, 0); err != nil {
		t.Fatalf("Claim A: %v", err)
	}
	if _, err := s.Claim(context.Background(), 1, instanceB, 0); err != nil {
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

// TestStore_CancelQueued_TableDriven proves CancelQueued's full contract:
// a queued job transitions to cancelled; a running/succeeded/failed job is
// left untouched (ok=false); an unowned or nonexistent id is also ok=false,
// indistinguishable from each other by design (mirrors Get's IDOR-safe 404).
func TestStore_CancelQueued_TableDriven(t *testing.T) {
	pool := newTestPostgres(t)
	s := NewStore(pool)
	custA, keyA := seedCustomer(t, pool, "jobs-cancel-"+uuid.New().String()+"@example.com")
	custB, _ := seedCustomer(t, pool, "jobs-cancel-other-"+uuid.New().String()+"@example.com")

	newJob := func(t *testing.T) uuid.UUID {
		t.Helper()
		id, err := s.Enqueue(context.Background(), custA, keyA, "echo", "req-"+uuid.New().String(), "free", json.RawMessage(`{}`), 0, "")
		if err != nil {
			t.Fatalf("Enqueue: %v", err)
		}
		return id
	}

	t.Run("queued job is cancelled", func(t *testing.T) {
		id := newJob(t)
		ok, err := s.CancelQueued(context.Background(), id, custA)
		if err != nil || !ok {
			t.Fatalf("CancelQueued: ok=%v err=%v, want (true, nil)", ok, err)
		}
		job, found, err := s.Get(context.Background(), id, custA)
		if err != nil || !found {
			t.Fatalf("Get: found=%v err=%v", found, err)
		}
		if job.Status != StatusCancelled {
			t.Errorf("status = %q, want %q", job.Status, StatusCancelled)
		}
	})

	t.Run("running job is rejected", func(t *testing.T) {
		id := newJob(t)
		if _, err := s.Claim(context.Background(), 10, uuid.New(), 0); err != nil {
			t.Fatalf("Claim: %v", err)
		}
		ok, err := s.CancelQueued(context.Background(), id, custA)
		if err != nil || ok {
			t.Fatalf("CancelQueued(running): ok=%v err=%v, want (false, nil)", ok, err)
		}
		job, found, err := s.Get(context.Background(), id, custA)
		if err != nil || !found {
			t.Fatalf("Get: found=%v err=%v", found, err)
		}
		if job.Status != StatusRunning {
			t.Errorf("status = %q, want %q (must be left untouched)", job.Status, StatusRunning)
		}
	})

	t.Run("succeeded job is rejected", func(t *testing.T) {
		id := newJob(t)
		if _, err := s.Claim(context.Background(), 10, uuid.New(), 0); err != nil {
			t.Fatalf("Claim: %v", err)
		}
		if err := s.Complete(context.Background(), id, json.RawMessage(`{}`), 1, "units"); err != nil {
			t.Fatalf("Complete: %v", err)
		}
		ok, err := s.CancelQueued(context.Background(), id, custA)
		if err != nil || ok {
			t.Fatalf("CancelQueued(succeeded): ok=%v err=%v, want (false, nil)", ok, err)
		}
	})

	t.Run("failed job is rejected", func(t *testing.T) {
		id := newJob(t)
		if err := s.Fail(context.Background(), id, "WORKER_BAD_RESPONSE", "worker contract violation"); err != nil {
			t.Fatalf("Fail: %v", err)
		}
		ok, err := s.CancelQueued(context.Background(), id, custA)
		if err != nil || ok {
			t.Fatalf("CancelQueued(failed): ok=%v err=%v, want (false, nil)", ok, err)
		}
	})

	t.Run("other customer's job is not cancelled (IDOR)", func(t *testing.T) {
		id := newJob(t)
		ok, err := s.CancelQueued(context.Background(), id, custB)
		if err != nil || ok {
			t.Fatalf("CancelQueued(other customer): ok=%v err=%v, want (false, nil)", ok, err)
		}
		job, found, err := s.Get(context.Background(), id, custA)
		if err != nil || !found {
			t.Fatalf("Get: found=%v err=%v", found, err)
		}
		if job.Status != StatusQueued {
			t.Errorf("status = %q, want %q (must be left untouched)", job.Status, StatusQueued)
		}
	})

	t.Run("nonexistent id", func(t *testing.T) {
		ok, err := s.CancelQueued(context.Background(), uuid.New(), custA)
		if err != nil || ok {
			t.Fatalf("CancelQueued(nonexistent): ok=%v err=%v, want (false, nil)", ok, err)
		}
	})
}

func TestStore_CancelQueued_NilReceiver(t *testing.T) {
	var s *Store
	if _, err := s.CancelQueued(context.Background(), uuid.New(), uuid.New()); err == nil {
		t.Error("nil Store.CancelQueued: want error")
	}
}

func TestStore_Requeue(t *testing.T) {
	pool := newTestPostgres(t)
	s := NewStore(pool)
	custA, keyA := seedCustomer(t, pool, "jobs-requeue-"+uuid.New().String()+"@example.com")
	id, err := s.Enqueue(context.Background(), custA, keyA, "echo", "req", "free", json.RawMessage(`{}`), 0, "")
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if _, err := s.Claim(context.Background(), 10, uuid.New(), 0); err != nil {
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

func TestStore_RequeueRetry_SetsAttemptsAndNextAttemptAt(t *testing.T) {
	pool := newTestPostgres(t)
	s := NewStore(pool)
	custA, keyA := seedCustomer(t, pool, "jobs-requeue-retry-"+uuid.New().String()+"@example.com")
	id, err := s.Enqueue(context.Background(), custA, keyA, "echo", "req", "free", json.RawMessage(`{}`), 0, "")
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if _, err := s.Claim(context.Background(), 10, uuid.New(), 0); err != nil {
		t.Fatalf("Claim: %v", err)
	}

	nextAt := time.Now().Add(time.Hour)
	if err := s.RequeueRetry(context.Background(), id, 2, nextAt); err != nil {
		t.Fatalf("RequeueRetry: %v", err)
	}

	job, ok, err := s.Get(context.Background(), id, custA)
	if err != nil || !ok {
		t.Fatalf("Get: ok=%v err=%v", ok, err)
	}
	if job.Status != StatusQueued {
		t.Errorf("status = %q, want %q", job.Status, StatusQueued)
	}

	// Attempts isn't in Get's SELECT list (only Claim's — see Job.Attempts's
	// doc comment), so assert the persisted value directly.
	var attempts int
	var storedNextAt time.Time
	if err := pool.QueryRow(context.Background(),
		`SELECT attempts, next_attempt_at FROM async_jobs WHERE id = $1`, id,
	).Scan(&attempts, &storedNextAt); err != nil {
		t.Fatalf("query attempts/next_attempt_at: %v", err)
	}
	if attempts != 2 {
		t.Errorf("attempts = %d, want 2", attempts)
	}
	if storedNextAt.Sub(nextAt).Abs() > time.Second {
		t.Errorf("next_attempt_at = %s, want ~%s", storedNextAt, nextAt)
	}
}

// TestStore_Claim_SkipsFutureNextAttemptAt proves a row scheduled for retry
// is not claimable again before its backoff delay elapses, while a genuinely
// eligible queued row is claimed in the same call — oldest-eligible-first
// ordering (by created_at) among the eligible rows.
func TestStore_Claim_SkipsFutureNextAttemptAt(t *testing.T) {
	pool := newTestPostgres(t)
	s := NewStore(pool)
	custA, keyA := seedCustomer(t, pool, "jobs-claim-future-"+uuid.New().String()+"@example.com")

	scheduled, err := s.Enqueue(context.Background(), custA, keyA, "echo", "req-scheduled", "free", json.RawMessage(`{}`), 0, "")
	if err != nil {
		t.Fatalf("Enqueue (scheduled): %v", err)
	}
	if _, err := pool.Exec(context.Background(),
		`UPDATE async_jobs SET next_attempt_at = NOW() + INTERVAL '1 hour' WHERE id = $1`, scheduled,
	); err != nil {
		t.Fatalf("seed future next_attempt_at: %v", err)
	}

	eligible, err := s.Enqueue(context.Background(), custA, keyA, "echo", "req-eligible", "free", json.RawMessage(`{}`), 0, "")
	if err != nil {
		t.Fatalf("Enqueue (eligible): %v", err)
	}

	claimed, err := s.Claim(context.Background(), 10, uuid.New(), 0)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	var gotScheduled, gotEligible bool
	for _, j := range claimed {
		if j.ID == scheduled {
			gotScheduled = true
		}
		if j.ID == eligible {
			gotEligible = true
		}
	}
	if gotScheduled {
		t.Error("Claim returned a row whose next_attempt_at is in the future")
	}
	if !gotEligible {
		t.Error("Claim did not return the eligible row")
	}

	job, ok, err := s.Get(context.Background(), scheduled, custA)
	if err != nil || !ok {
		t.Fatalf("Get(scheduled): ok=%v err=%v", ok, err)
	}
	if job.Status != StatusQueued {
		t.Errorf("scheduled job status = %q, want %q (must remain queued, not claimed)", job.Status, StatusQueued)
	}
}

func TestStore_DeadLetter_SetsAttemptsAndTerminal(t *testing.T) {
	pool := newTestPostgres(t)
	s := NewStore(pool)
	custA, keyA := seedCustomer(t, pool, "jobs-deadletter-"+uuid.New().String()+"@example.com")
	id, err := s.Enqueue(context.Background(), custA, keyA, "echo", "req", "free", json.RawMessage(`{}`), 0, "")
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if _, err := s.Claim(context.Background(), 10, uuid.New(), 0); err != nil {
		t.Fatalf("Claim: %v", err)
	}

	if err := s.DeadLetter(context.Background(), id, 3, "WORKER_UNREACHABLE", "worker unavailable"); err != nil {
		t.Fatalf("DeadLetter: %v", err)
	}

	job, ok, err := s.Get(context.Background(), id, custA)
	if err != nil || !ok {
		t.Fatalf("Get: ok=%v err=%v", ok, err)
	}
	if job.Status != StatusFailed {
		t.Errorf("status = %q, want %q", job.Status, StatusFailed)
	}
	if job.ErrorCode != "WORKER_UNREACHABLE" || job.ErrorMessage != "worker unavailable" {
		t.Errorf("error fields mismatch: code=%q message=%q", job.ErrorCode, job.ErrorMessage)
	}
	var attempts int
	if err := pool.QueryRow(context.Background(),
		`SELECT attempts FROM async_jobs WHERE id = $1`, id,
	).Scan(&attempts); err != nil {
		t.Fatalf("query attempts: %v", err)
	}
	if attempts != 3 {
		t.Errorf("attempts = %d, want 3", attempts)
	}
}
