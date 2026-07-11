package jobs

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Unluckyathecking/crucible/gateway/internal/proxy"
	"github.com/Unluckyathecking/crucible/gateway/internal/usage"
)

func waitForStatus(t *testing.T, s *Store, id, customerID uuid.UUID, want string, timeout time.Duration) Job {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		job, ok, err := s.Get(context.Background(), id, customerID)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if ok && job.Status == want {
			return job
		}
		if time.Now().After(deadline) {
			if ok {
				t.Fatalf("job %s status = %q after %s, want %q", id, job.Status, timeout, want)
			}
			t.Fatalf("job %s not found after %s", id, timeout)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestExecutor_NilSafe(t *testing.T) {
	var e *Executor
	e.Run(context.Background()) // must not panic

	e2 := NewExecutor(nil, nil, nil, ExecutorConfig{})
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	e2.Run(ctx) // store/proxy nil: Run must return promptly without polling
}

func TestExecutor_Success_RecordsUsageAndCompletes(t *testing.T) {
	pool := newTestPostgres(t)
	store := NewStore(pool)
	custA, keyA := seedCustomer(t, pool, "jobs-exec-success-"+uuid.New().String()+"@example.com")

	worker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"payload":{"ok":true},"billable_units":3,"units_label":"pages"}`))
	}))
	t.Cleanup(worker.Close)
	p := proxy.New(worker.URL, 5*time.Second, 0)
	recorder := usage.NewRecorder(pool, nil)
	e := NewExecutor(store, p, recorder, ExecutorConfig{PoolSize: 2, PollInterval: 20 * time.Millisecond, JobTimeout: 5 * time.Second})

	id, err := store.Enqueue(context.Background(), custA, keyA, "echo", "req-exec-1", "free", json.RawMessage(`{"in":1}`), 0)
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); e.Run(ctx) }()

	job := waitForStatus(t, store, id, custA, StatusSucceeded, 2*time.Second)
	if job.BillableUnits != 3 {
		t.Errorf("billable_units = %d, want 3", job.BillableUnits)
	}
	if job.UnitsLabel != "pages" {
		t.Errorf("units_label = %q, want pages", job.UnitsLabel)
	}

	var count int64
	if err := pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM usage_events WHERE customer_id = $1 AND request_id = $2`, custA, "req-exec-1",
	).Scan(&count); err != nil {
		t.Fatalf("count usage_events: %v", err)
	}
	if count != 1 {
		t.Errorf("usage_events rows = %d, want 1", count)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Executor.Run did not stop after context cancellation")
	}
}

func TestExecutor_WorkerError_MarksFailed_NoUsageRecorded(t *testing.T) {
	pool := newTestPostgres(t)
	store := NewStore(pool)
	custA, keyA := seedCustomer(t, pool, "jobs-exec-fail-"+uuid.New().String()+"@example.com")

	worker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"error":{"code":"BAD_INPUT","message":"nope","retryable":false},"billable_units":0}`))
	}))
	t.Cleanup(worker.Close)
	p := proxy.New(worker.URL, 5*time.Second, 0)
	recorder := usage.NewRecorder(pool, nil)
	e := NewExecutor(store, p, recorder, ExecutorConfig{PoolSize: 2, PollInterval: 20 * time.Millisecond, JobTimeout: 5 * time.Second})

	id, err := store.Enqueue(context.Background(), custA, keyA, "echo", "req-exec-2", "free", json.RawMessage(`{}`), 0)
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go e.Run(ctx)

	job := waitForStatus(t, store, id, custA, StatusFailed, 2*time.Second)
	if job.ErrorCode != "BAD_INPUT" || job.ErrorMessage != "nope" {
		t.Errorf("error fields = %q/%q, want BAD_INPUT/nope", job.ErrorCode, job.ErrorMessage)
	}

	var count int64
	if err := pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM usage_events WHERE customer_id = $1 AND request_id = $2`, custA, "req-exec-2",
	).Scan(&count); err != nil {
		t.Fatalf("count usage_events: %v", err)
	}
	if count != 0 {
		t.Errorf("usage_events rows = %d, want 0 (a failed job must never bill)", count)
	}
}

// TestExecutor_BillableUnitsBelowOne_RejectsAsTrustBoundaryViolation proves
// the async path enforces the exact same invariant #2 contract as the
// synchronous /v1 invoke handler: a worker reporting success with
// billable_units < 1 must never be treated as billable success.
func TestExecutor_BillableUnitsBelowOne_RejectsAsTrustBoundaryViolation(t *testing.T) {
	pool := newTestPostgres(t)
	store := NewStore(pool)
	custA, keyA := seedCustomer(t, pool, "jobs-exec-badunits-"+uuid.New().String()+"@example.com")

	worker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"payload":{"ok":true},"billable_units":0}`))
	}))
	t.Cleanup(worker.Close)
	p := proxy.New(worker.URL, 5*time.Second, 0)
	recorder := usage.NewRecorder(pool, nil)
	e := NewExecutor(store, p, recorder, ExecutorConfig{PoolSize: 2, PollInterval: 20 * time.Millisecond, JobTimeout: 5 * time.Second})

	id, err := store.Enqueue(context.Background(), custA, keyA, "echo", "req-exec-3", "free", json.RawMessage(`{}`), 0)
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go e.Run(ctx)

	job := waitForStatus(t, store, id, custA, StatusFailed, 2*time.Second)
	if job.ErrorCode != "WORKER_BAD_RESPONSE" {
		t.Errorf("error_code = %q, want WORKER_BAD_RESPONSE", job.ErrorCode)
	}

	var count int64
	if err := pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM usage_events WHERE customer_id = $1 AND request_id = $2`, custA, "req-exec-3",
	).Scan(&count); err != nil {
		t.Fatalf("count usage_events: %v", err)
	}
	if count != 0 {
		t.Errorf("usage_events rows = %d, want 0 — a billable_units<1 response must never bill", count)
	}
}

// TestExecutor_GracefulShutdown_RequeuesInFlightJob proves the "no lost
// work" acceptance criterion: a job whose worker call is interrupted by
// context cancellation (graceful shutdown) is returned to 'queued', not
// permanently failed.
func TestExecutor_GracefulShutdown_RequeuesInFlightJob(t *testing.T) {
	pool := newTestPostgres(t)
	store := NewStore(pool)
	custA, keyA := seedCustomer(t, pool, "jobs-exec-shutdown-"+uuid.New().String()+"@example.com")

	started := make(chan struct{})
	release := make(chan struct{})
	worker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-release
		// By the time we'd respond, the client's context is already
		// cancelled and http.Client has abandoned the request; nothing
		// further to write.
	}))
	t.Cleanup(worker.Close)
	t.Cleanup(func() { close(release) })
	p := proxy.New(worker.URL, 30*time.Second, 0)
	e := NewExecutor(store, p, nil, ExecutorConfig{PoolSize: 1, PollInterval: 20 * time.Millisecond, JobTimeout: 30 * time.Second})

	id, err := store.Enqueue(context.Background(), custA, keyA, "echo", "req-exec-4", "free", json.RawMessage(`{}`), 0)
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); e.Run(ctx) }()

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("worker was never invoked")
	}

	cancel() // graceful shutdown while the worker call is in flight

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Executor.Run did not stop after context cancellation")
	}

	job := waitForStatus(t, store, id, custA, StatusQueued, 2*time.Second)
	if job.ErrorCode != "" {
		t.Errorf("requeued job has error_code = %q, want empty (not a permanent failure)", job.ErrorCode)
	}
}

func TestExecutor_ReleaseClaimed_NoLostWorkOnShutdown(t *testing.T) {
	pool := newTestPostgres(t)
	store := NewStore(pool)
	custA, keyA := seedCustomer(t, pool, "jobs-exec-release-"+uuid.New().String()+"@example.com")

	instanceID := uuid.New()
	id, err := store.Enqueue(context.Background(), custA, keyA, "echo", "req", "free", json.RawMessage(`{}`), 0)
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if _, err := store.Claim(context.Background(), 1, instanceID); err != nil {
		t.Fatalf("Claim: %v", err)
	}

	e := &Executor{store: store, instanceID: instanceID}
	e.releaseClaimed()

	waitForStatus(t, store, id, custA, StatusQueued, time.Second)
}
