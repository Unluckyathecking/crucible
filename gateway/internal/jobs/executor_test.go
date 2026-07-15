package jobs

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/Unluckyathecking/crucible/gateway/internal/events"
	"github.com/Unluckyathecking/crucible/gateway/internal/observability"
	"github.com/Unluckyathecking/crucible/gateway/internal/proxy"
	"github.com/Unluckyathecking/crucible/gateway/internal/usage"
	"github.com/Unluckyathecking/crucible/gateway/internal/webhookout"
)

// hijackAndClose aborts the HTTP response by hijacking the connection and
// closing it without writing anything — the client sees a transport error
// (EOF), exactly the WORKER_UNREACHABLE class Executor is meant to retry,
// as opposed to a worker-returned structured error envelope (HTTP 200 body).
func hijackAndClose(t *testing.T, w http.ResponseWriter) {
	t.Helper()
	hj, ok := w.(http.Hijacker)
	if !ok {
		t.Fatal("ResponseWriter does not support hijacking")
	}
	conn, _, err := hj.Hijack()
	if err != nil {
		t.Fatalf("hijack: %v", err)
	}
	conn.Close()
}

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

// waitForUsageEventCount polls usage_events for (customerID, requestID) until
// it reaches want or timeout elapses. Complete (which waitForStatus observes)
// and Record are two separate sequential writes within the same process()
// call — see process's doc comment on why they can't share a transaction —
// so a concurrent Get can observe 'succeeded' in the narrow window before
// Record has run. Polling here, rather than asserting immediately after
// waitForStatus, avoids that race without weakening what's actually proven:
// Record always follows Complete in the same goroutine, so it lands within
// the poll window every time the job legitimately succeeds.
func waitForUsageEventCount(t *testing.T, pool *pgxpool.Pool, customerID uuid.UUID, requestID string, want int64, timeout time.Duration) int64 {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var count int64
	for {
		if err := pool.QueryRow(context.Background(),
			`SELECT COUNT(*) FROM usage_events WHERE customer_id = $1 AND request_id = $2`, customerID, requestID,
		).Scan(&count); err != nil {
			t.Fatalf("count usage_events: %v", err)
		}
		if count == want {
			return count
		}
		if time.Now().After(deadline) {
			return count
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// seedWebhookEndpoint inserts one active webhook_endpoints row for customerID
// subscribed to every event type (subscribed_events left NULL), so the
// emitter's Emit fans job.succeeded/job.failed out to it exactly like any
// other event type.
func seedWebhookEndpoint(t *testing.T, pool *pgxpool.Pool, customerID uuid.UUID) {
	t.Helper()
	secret, err := webhookout.GenerateSecret()
	if err != nil {
		t.Fatalf("generate secret: %v", err)
	}
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO webhook_endpoints (customer_id, url, secret, active) VALUES ($1, 'https://example.com/hook', $2, TRUE)`,
		customerID, secret,
	); err != nil {
		t.Fatalf("insert webhook endpoint: %v", err)
	}
}

// waitForWebhookDeliveryCount polls webhook_deliveries for (customerID,
// eventType) until it reaches want or timeout elapses, returning the last
// observed count and the most recently seen payload (if any row exists).
func waitForWebhookDeliveryCount(t *testing.T, pool *pgxpool.Pool, customerID uuid.UUID, eventType string, want int, timeout time.Duration) (int, json.RawMessage) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var count int
	var payload json.RawMessage
	for {
		rows, err := pool.Query(context.Background(), `
			SELECT d.payload
			FROM webhook_deliveries d
			JOIN webhook_endpoints we ON we.id = d.endpoint_id
			WHERE we.customer_id = $1 AND d.event_type = $2
		`, customerID, eventType)
		if err != nil {
			t.Fatalf("query webhook_deliveries: %v", err)
		}
		count = 0
		for rows.Next() {
			if err := rows.Scan(&payload); err != nil {
				rows.Close()
				t.Fatalf("scan webhook_deliveries: %v", err)
			}
			count++
		}
		rows.Close()
		if count == want {
			return count, payload
		}
		if time.Now().After(deadline) {
			return count, payload
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

	id, err := store.Enqueue(context.Background(), custA, keyA, "echo", "req-exec-1", "free", json.RawMessage(`{"in":1}`), 0, "")
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

	if count := waitForUsageEventCount(t, pool, custA, "req-exec-1", 1, time.Second); count != 1 {
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
	// ErrorExposure: "full" — this test asserts the worker's own error
	// reaches the job row verbatim; see
	// TestExecutor_WorkerError_SanitizedByDefault for the opposite (and
	// operationally default) case.
	e := NewExecutor(store, p, recorder, ExecutorConfig{PoolSize: 2, PollInterval: 20 * time.Millisecond, JobTimeout: 5 * time.Second, ErrorExposure: "full"})

	id, err := store.Enqueue(context.Background(), custA, keyA, "echo", "req-exec-2", "free", json.RawMessage(`{}`), 0, "")
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

// TestExecutor_WorkerError_SanitizedByDefault proves GET /v1/jobs/{id} can't
// leak a worker's internal error details when WORKER_ERROR_EXPOSURE is left
// at its sanitized default (ExecutorConfig.ErrorExposure zero value) — the
// same policy the synchronous /v1 invoke handler enforces (server/routes.go)
// applied identically to the async path via the shared SanitizeWorkerError.
func TestExecutor_WorkerError_SanitizedByDefault(t *testing.T) {
	pool := newTestPostgres(t)
	store := NewStore(pool)
	custA, keyA := seedCustomer(t, pool, "jobs-exec-sanitized-"+uuid.New().String()+"@example.com")

	worker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"error":{"code":"INTERNAL_STACK_TRACE_LEAK","message":"panic at db.go:42","retryable":false},"billable_units":0}`))
	}))
	t.Cleanup(worker.Close)
	p := proxy.New(worker.URL, 5*time.Second, 0)
	recorder := usage.NewRecorder(pool, nil)
	// ErrorExposure intentionally left unset (zero value) — proves the
	// sanitized default, not an explicitly-configured "sanitized" string.
	e := NewExecutor(store, p, recorder, ExecutorConfig{PoolSize: 2, PollInterval: 20 * time.Millisecond, JobTimeout: 5 * time.Second})

	id, err := store.Enqueue(context.Background(), custA, keyA, "echo", "req-exec-sanitized", "free", json.RawMessage(`{}`), 0, "")
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go e.Run(ctx)

	job := waitForStatus(t, store, id, custA, StatusFailed, 2*time.Second)
	if job.ErrorCode != "WORKER_UNREACHABLE" {
		t.Errorf("error_code = %q, want WORKER_UNREACHABLE (sanitized)", job.ErrorCode)
	}
	if job.ErrorMessage != "worker unavailable" {
		t.Errorf("error_message = %q, want %q (sanitized)", job.ErrorMessage, "worker unavailable")
	}
	if job.ErrorCode == "INTERNAL_STACK_TRACE_LEAK" || job.ErrorMessage == "panic at db.go:42" {
		t.Fatal("sanitized mode leaked worker internals")
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

	id, err := store.Enqueue(context.Background(), custA, keyA, "echo", "req-exec-3", "free", json.RawMessage(`{}`), 0, "")
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

// TestExecutor_GracefulShutdown_LeavesInFlightJobRunning proves Run does NOT
// eagerly requeue a job whose worker call was interrupted by shutdown: the
// worker SDK can't force product code to stop on context cancellation, so
// the original invocation may still genuinely be executing. Immediately
// marking the row 'queued' would let another claim start a second,
// concurrent execution of the same job. The row must stay 'running' —
// Store.Claim's crash-recovery sweep (timeout_seconds/DefaultJobTimeout +
// stuckJobGrace) is the only path back to 'queued', and only once enough
// time has passed that the original call has genuinely finished or expired.
func TestExecutor_GracefulShutdown_LeavesInFlightJobRunning(t *testing.T) {
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

	id, err := store.Enqueue(context.Background(), custA, keyA, "echo", "req-exec-4", "free", json.RawMessage(`{}`), 0, "")
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

	// Give any (incorrect) eager requeue a moment to land before asserting
	// the row is untouched — a flaky pass here would hide a regression.
	time.Sleep(100 * time.Millisecond)
	job, ok, err := store.Get(context.Background(), id, custA)
	if err != nil || !ok {
		t.Fatalf("Get: ok=%v err=%v", ok, err)
	}
	if job.Status != StatusRunning {
		t.Errorf("status = %q after shutdown, want %q (must not be eagerly requeued)", job.Status, StatusRunning)
	}
}

func TestStore_ReleaseClaimed_UsableAsOperatorPrimitive(t *testing.T) {
	pool := newTestPostgres(t)
	store := NewStore(pool)
	custA, keyA := seedCustomer(t, pool, "jobs-exec-release-"+uuid.New().String()+"@example.com")

	instanceID := uuid.New()
	id, err := store.Enqueue(context.Background(), custA, keyA, "echo", "req", "free", json.RawMessage(`{}`), 0, "")
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if _, err := store.Claim(context.Background(), 1, instanceID); err != nil {
		t.Fatalf("Claim: %v", err)
	}

	// Executor.Run never calls this itself (see its doc comment); it remains
	// a directly usable Store primitive, e.g. for an operator who can
	// positively confirm instanceID's process is gone.
	if _, err := store.ReleaseClaimed(context.Background(), instanceID); err != nil {
		t.Fatalf("ReleaseClaimed: %v", err)
	}

	waitForStatus(t, store, id, custA, StatusQueued, time.Second)
}

// TestExecutor_TransientFailure_RetriesThenSucceeds_RecordsUsageOnce proves
// the acceptance scenario at the heart of this module: a job whose worker
// call fails transiently (WORKER_UNREACHABLE / transport error, simulated
// here by hijacking and closing the connection before any HTTP response) is
// retried with backoff rather than failed on the first error, and once the
// worker recovers the job succeeds and usage is recorded exactly once —
// never once per attempt, and never zero times because an earlier attempt
// failed.
func TestExecutor_TransientFailure_RetriesThenSucceeds_RecordsUsageOnce(t *testing.T) {
	pool := newTestPostgres(t)
	store := NewStore(pool)
	custA, keyA := seedCustomer(t, pool, "jobs-exec-retry-succeed-"+uuid.New().String()+"@example.com")

	var calls int32
	worker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) <= 2 {
			hijackAndClose(t, w)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"payload":{"ok":true},"billable_units":2,"units_label":"pages"}`))
	}))
	t.Cleanup(worker.Close)
	p := proxy.New(worker.URL, 2*time.Second, 0)
	recorder := usage.NewRecorder(pool, nil)
	e := NewExecutor(store, p, recorder, ExecutorConfig{
		PoolSize: 2, PollInterval: 20 * time.Millisecond, JobTimeout: 5 * time.Second,
		MaxAttempts: 5, RetryBackoff: 50 * time.Millisecond,
	})

	retriedBefore := testutil.ToFloat64(observability.JobsRetriedTotal.WithLabelValues("echo"))

	id, err := store.Enqueue(context.Background(), custA, keyA, "echo", "req-retry-succeed", "free", json.RawMessage(`{}`), 0, "")
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); e.Run(ctx) }()

	job := waitForStatus(t, store, id, custA, StatusSucceeded, 5*time.Second)
	if job.BillableUnits != 2 {
		t.Errorf("billable_units = %d, want 2", job.BillableUnits)
	}
	if got := atomic.LoadInt32(&calls); got != 3 {
		t.Errorf("worker was called %d times, want 3 (2 transient failures + 1 success)", got)
	}

	if count := waitForUsageEventCount(t, pool, custA, "req-retry-succeed", 1, time.Second); count != 1 {
		t.Errorf("usage_events rows = %d, want 1 (billed exactly once despite 2 retries)", count)
	}

	if retried := testutil.ToFloat64(observability.JobsRetriedTotal.WithLabelValues("echo")) - retriedBefore; retried != 2 {
		t.Errorf("crucible_jobs_retried_total delta = %v, want 2", retried)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Executor.Run did not stop after context cancellation")
	}
}

// TestExecutor_TransientFailure_ExhaustsRetries_DeadLetters proves a
// sustained transient failure (the worker never recovers) still terminates:
// once attempts reaches MaxAttempts the job dead-letters to terminal
// 'failed' — the same customer-visible shape as an immediate deterministic
// failure (GET /v1/jobs/{id} gains no new status) — and is never billed.
func TestExecutor_TransientFailure_ExhaustsRetries_DeadLetters(t *testing.T) {
	pool := newTestPostgres(t)
	store := NewStore(pool)
	custA, keyA := seedCustomer(t, pool, "jobs-exec-retry-exhaust-"+uuid.New().String()+"@example.com")

	var calls int32
	worker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		hijackAndClose(t, w)
	}))
	t.Cleanup(worker.Close)
	p := proxy.New(worker.URL, 2*time.Second, 0)
	recorder := usage.NewRecorder(pool, nil)
	e := NewExecutor(store, p, recorder, ExecutorConfig{
		PoolSize: 2, PollInterval: 20 * time.Millisecond, JobTimeout: 5 * time.Second,
		MaxAttempts: 2, RetryBackoff: 20 * time.Millisecond,
	})

	retriedBefore := testutil.ToFloat64(observability.JobsRetriedTotal.WithLabelValues("echo"))

	id, err := store.Enqueue(context.Background(), custA, keyA, "echo", "req-retry-exhaust", "free", json.RawMessage(`{}`), 0, "")
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go e.Run(ctx)

	job := waitForStatus(t, store, id, custA, StatusFailed, 5*time.Second)
	if job.ErrorCode != "WORKER_UNREACHABLE" {
		t.Errorf("error_code = %q, want WORKER_UNREACHABLE", job.ErrorCode)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Errorf("worker was called %d times, want 2 (MaxAttempts)", got)
	}

	var count int64
	if err := pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM usage_events WHERE customer_id = $1 AND request_id = $2`, custA, "req-retry-exhaust",
	).Scan(&count); err != nil {
		t.Fatalf("count usage_events: %v", err)
	}
	if count != 0 {
		t.Errorf("usage_events rows = %d, want 0 — a dead-lettered job must never bill", count)
	}

	if retried := testutil.ToFloat64(observability.JobsRetriedTotal.WithLabelValues("echo")) - retriedBefore; retried != 1 {
		t.Errorf("crucible_jobs_retried_total delta = %v, want 1 (one retry before exhausting MaxAttempts=2)", retried)
	}
}

// TestExecutor_Success_EmitsJobSucceededWebhook proves a job that reaches the
// terminal 'succeeded' state fires exactly one job.succeeded webhook, with a
// payload carrying job_id/operation/status — never the worker's raw result
// body.
func TestExecutor_Success_EmitsJobSucceededWebhook(t *testing.T) {
	pool := newTestPostgres(t)
	store := NewStore(pool)
	custA, keyA := seedCustomer(t, pool, "jobs-exec-emit-success-"+uuid.New().String()+"@example.com")
	seedWebhookEndpoint(t, pool, custA)

	worker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"payload":{"ok":true},"billable_units":1}`))
	}))
	t.Cleanup(worker.Close)
	p := proxy.New(worker.URL, 5*time.Second, 0)
	recorder := usage.NewRecorder(pool, nil)
	emitter := webhookout.NewEmitter(context.Background(), pool)
	t.Cleanup(emitter.Stop)
	e := NewExecutor(store, p, recorder, ExecutorConfig{PoolSize: 2, PollInterval: 20 * time.Millisecond, JobTimeout: 5 * time.Second})
	e.SetEmitter(emitter)

	id, err := store.Enqueue(context.Background(), custA, keyA, "echo", "req-emit-success", "free", json.RawMessage(`{}`), 0, "")
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go e.Run(ctx)

	waitForStatus(t, store, id, custA, StatusSucceeded, 2*time.Second)

	count, payload := waitForWebhookDeliveryCount(t, pool, custA, events.JobSucceeded, 1, 2*time.Second)
	if count != 1 {
		t.Fatalf("job.succeeded webhook_deliveries rows = %d, want 1", count)
	}
	var decoded events.JobSucceededPayload
	if err := json.Unmarshal(payload, &decoded); err != nil {
		t.Fatalf("payload is not valid JobSucceededPayload JSON: %v", err)
	}
	if decoded.JobID != id.String() {
		t.Errorf("payload.job_id = %q, want %q", decoded.JobID, id.String())
	}
	if decoded.Operation != "echo" {
		t.Errorf("payload.operation = %q, want echo", decoded.Operation)
	}
	if decoded.Status != StatusSucceeded {
		t.Errorf("payload.status = %q, want %q", decoded.Status, StatusSucceeded)
	}

	// The failure-side event type must never fire for a successful job.
	if failCount, _ := waitForWebhookDeliveryCount(t, pool, custA, events.JobFailed, 0, 100*time.Millisecond); failCount != 0 {
		t.Errorf("job.failed webhook_deliveries rows = %d, want 0", failCount)
	}
}

// TestExecutor_WorkerError_EmitsJobFailedWebhook proves a worker-reported
// structured error (a deterministic terminal failure, never retried) fires
// exactly one job.failed webhook carrying the sanitized error_code.
func TestExecutor_WorkerError_EmitsJobFailedWebhook(t *testing.T) {
	pool := newTestPostgres(t)
	store := NewStore(pool)
	custA, keyA := seedCustomer(t, pool, "jobs-exec-emit-workererr-"+uuid.New().String()+"@example.com")
	seedWebhookEndpoint(t, pool, custA)

	worker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"error":{"code":"BAD_INPUT","message":"nope","retryable":false},"billable_units":0}`))
	}))
	t.Cleanup(worker.Close)
	p := proxy.New(worker.URL, 5*time.Second, 0)
	recorder := usage.NewRecorder(pool, nil)
	emitter := webhookout.NewEmitter(context.Background(), pool)
	t.Cleanup(emitter.Stop)
	e := NewExecutor(store, p, recorder, ExecutorConfig{PoolSize: 2, PollInterval: 20 * time.Millisecond, JobTimeout: 5 * time.Second, ErrorExposure: "full"})
	e.SetEmitter(emitter)

	id, err := store.Enqueue(context.Background(), custA, keyA, "echo", "req-emit-workererr", "free", json.RawMessage(`{}`), 0, "")
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go e.Run(ctx)

	waitForStatus(t, store, id, custA, StatusFailed, 2*time.Second)

	count, payload := waitForWebhookDeliveryCount(t, pool, custA, events.JobFailed, 1, 2*time.Second)
	if count != 1 {
		t.Fatalf("job.failed webhook_deliveries rows = %d, want 1", count)
	}
	var decoded events.JobFailedPayload
	if err := json.Unmarshal(payload, &decoded); err != nil {
		t.Fatalf("payload is not valid JobFailedPayload JSON: %v", err)
	}
	if decoded.ErrorCode != "BAD_INPUT" {
		t.Errorf("payload.error_code = %q, want BAD_INPUT", decoded.ErrorCode)
	}
	if decoded.Status != StatusFailed {
		t.Errorf("payload.status = %q, want %q", decoded.Status, StatusFailed)
	}
}

// TestExecutor_BillableUnitsBelowOne_EmitsJobFailedWebhook proves the
// billable_units<1 trust-boundary rejection (invariant #2) — the second
// deterministic terminal-failure kind — also fires exactly one job.failed
// webhook.
func TestExecutor_BillableUnitsBelowOne_EmitsJobFailedWebhook(t *testing.T) {
	pool := newTestPostgres(t)
	store := NewStore(pool)
	custA, keyA := seedCustomer(t, pool, "jobs-exec-emit-badunits-"+uuid.New().String()+"@example.com")
	seedWebhookEndpoint(t, pool, custA)

	worker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"payload":{"ok":true},"billable_units":0}`))
	}))
	t.Cleanup(worker.Close)
	p := proxy.New(worker.URL, 5*time.Second, 0)
	recorder := usage.NewRecorder(pool, nil)
	emitter := webhookout.NewEmitter(context.Background(), pool)
	t.Cleanup(emitter.Stop)
	e := NewExecutor(store, p, recorder, ExecutorConfig{PoolSize: 2, PollInterval: 20 * time.Millisecond, JobTimeout: 5 * time.Second})
	e.SetEmitter(emitter)

	id, err := store.Enqueue(context.Background(), custA, keyA, "echo", "req-emit-badunits", "free", json.RawMessage(`{}`), 0, "")
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go e.Run(ctx)

	waitForStatus(t, store, id, custA, StatusFailed, 2*time.Second)

	count, payload := waitForWebhookDeliveryCount(t, pool, custA, events.JobFailed, 1, 2*time.Second)
	if count != 1 {
		t.Fatalf("job.failed webhook_deliveries rows = %d, want 1", count)
	}
	var decoded events.JobFailedPayload
	if err := json.Unmarshal(payload, &decoded); err != nil {
		t.Fatalf("payload is not valid JobFailedPayload JSON: %v", err)
	}
	if decoded.ErrorCode != "WORKER_BAD_RESPONSE" {
		t.Errorf("payload.error_code = %q, want WORKER_BAD_RESPONSE", decoded.ErrorCode)
	}
}

// TestExecutor_TransientFailure_RetryEmitsNothing_DeadLetterEmitsExactlyOne
// proves the third terminal-failure kind (retry-exhausted dead-letter) fires
// exactly one job.failed webhook, and — critically — that the non-terminal
// retry attempt in between fires none: if retryOrDeadLetter's requeue branch
// incorrectly notified on every attempt, this job (MaxAttempts=2, so one
// retry before dead-lettering) would show two job.failed rows instead of one.
func TestExecutor_TransientFailure_RetryEmitsNothing_DeadLetterEmitsExactlyOne(t *testing.T) {
	pool := newTestPostgres(t)
	store := NewStore(pool)
	custA, keyA := seedCustomer(t, pool, "jobs-exec-emit-deadletter-"+uuid.New().String()+"@example.com")
	seedWebhookEndpoint(t, pool, custA)

	deadletteredBefore := testutil.ToFloat64(observability.JobsDeadletteredTotal.WithLabelValues("echo"))
	retriedBefore := testutil.ToFloat64(observability.JobsRetriedTotal.WithLabelValues("echo"))

	var calls int32
	worker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		hijackAndClose(t, w)
	}))
	t.Cleanup(worker.Close)
	p := proxy.New(worker.URL, 2*time.Second, 0)
	recorder := usage.NewRecorder(pool, nil)
	emitter := webhookout.NewEmitter(context.Background(), pool)
	t.Cleanup(emitter.Stop)
	e := NewExecutor(store, p, recorder, ExecutorConfig{
		PoolSize: 2, PollInterval: 20 * time.Millisecond, JobTimeout: 5 * time.Second,
		MaxAttempts: 2, RetryBackoff: 20 * time.Millisecond,
	})
	e.SetEmitter(emitter)

	id, err := store.Enqueue(context.Background(), custA, keyA, "echo", "req-emit-deadletter", "free", json.RawMessage(`{}`), 0, "")
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go e.Run(ctx)

	waitForStatus(t, store, id, custA, StatusFailed, 5*time.Second)
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Errorf("worker was called %d times, want 2 (MaxAttempts)", got)
	}

	count, _ := waitForWebhookDeliveryCount(t, pool, custA, events.JobFailed, 1, 2*time.Second)
	if count != 1 {
		t.Fatalf("job.failed webhook_deliveries rows = %d, want exactly 1 (never one per retry attempt)", count)
	}

	// The retry attempt (attempt 1) must not increment deadlettered; the
	// dead-letter path (attempt 2 = MaxAttempts) must increment it exactly once.
	if dl := testutil.ToFloat64(observability.JobsDeadletteredTotal.WithLabelValues("echo")) - deadletteredBefore; dl != 1 {
		t.Errorf("crucible_jobs_deadlettered_total{operation=echo} delta = %.0f, want 1", dl)
	}
	if retried := testutil.ToFloat64(observability.JobsRetriedTotal.WithLabelValues("echo")) - retriedBefore; retried != 1 {
		t.Errorf("crucible_jobs_retried_total{operation=echo} delta = %.0f, want 1 (one non-terminal retry)", retried)
	}
}
