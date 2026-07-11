package acceptance

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Unluckyathecking/crucible/gateway/internal/jobs"
	"github.com/Unluckyathecking/crucible/gateway/internal/proxy"
	"github.com/Unluckyathecking/crucible/gateway/internal/quota"
	"github.com/Unluckyathecking/crucible/gateway/internal/server"
	"github.com/Unluckyathecking/crucible/gateway/internal/usage"
	"github.com/Unluckyathecking/crucible/gateway/test/harness"
)

const asyncTestPlanID = "async-acceptance"

// asyncTestPostgresDSN and asyncTestRedisURL deliberately do NOT reuse this
// package's postgresDSN(t)/redisURL(t) helpers (acceptance_test.go), which
// are fatal-if-unset by design: TestClonedTreeRuntimeAcceptance only ever
// reaches them after workerURL(t) has already skipped the whole opt-in
// acceptance flow when WORKER_URL is absent. These async tests use an
// in-process worker (no WORKER_URL needed) and so, unlike that test, would
// be the first to actually call the fatal helpers during an ordinary
// `go test ./...` run with no services configured — breaking a plain local
// or CI run instead of skipping it. Skip gracefully instead, mirroring the
// newTestPostgres pattern used throughout gateway/internal (e.g.
// webhookout, jobs, operator).
func asyncTestPostgresDSN(t *testing.T) string {
	t.Helper()
	if v := os.Getenv("POSTGRES_DSN"); v != "" {
		return v
	}
	t.Skip("POSTGRES_DSN not set; skipping async acceptance test")
	return ""
}

func asyncTestRedisURL(t *testing.T) string {
	t.Helper()
	if v := os.Getenv("REDIS_URL"); v != "" {
		return v
	}
	t.Skip("REDIS_URL not set; skipping async acceptance test")
	return ""
}

// withAsyncEcho opts server.V1Routes[0] ("/echo") into AsyncRoutes for the
// duration of the calling test, restoring the previous map on cleanup.
// Must be called before harness.NewGatewayTestServer: NewRouter reads
// AsyncRoutes while building routes, but does not re-read it per request —
// mutating the package var after the router is built has no effect on the
// already-constructed router, so cleanup can safely restore immediately
// after the test's httptest server usage is done, mirroring how
// harness.go's own V1Routes mutation is scoped.
func withAsyncEcho(t *testing.T, timeoutSeconds int) {
	t.Helper()
	if len(server.V1Routes) == 0 {
		t.Fatal("server.V1Routes is empty; routes_table.go must declare at least one /v1 endpoint")
	}
	orig := server.AsyncRoutes
	server.AsyncRoutes = map[string]int{server.V1Routes[0].Path: timeoutSeconds}
	t.Cleanup(func() { server.AsyncRoutes = orig })
}

// asyncWorkerHandler is an in-process worker stub for the /invoke contract:
// any operation succeeds with a fixed billable_units, so these tests exercise
// the gateway's async plumbing rather than a specific product's business logic.
func asyncWorkerHandler(t *testing.T) http.Handler {
	t.Helper()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req proxy.InvokeRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("worker: decode request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"payload":{"echoed":true},"billable_units":2,"units_label":"calls"}`))
	})
}

type enqueueResponse struct {
	JobID string `json:"job_id"`
}

type jobStatusResponse struct {
	JobID         string          `json:"job_id"`
	Status        string          `json:"status"`
	Result        json.RawMessage `json:"result"`
	BillableUnits uint64          `json:"billable_units"`
	UnitsLabel    string          `json:"units_label"`
	Error         *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// TestAsyncInvoke_EndToEnd_SuccessAndPoll drives the full durable-async
// contract: POST /v1/<op> on a route opted into AsyncRoutes returns 202
// {job_id} instead of the synchronous 200; a bounded jobs.Executor (started
// here, pointed at the same Postgres/worker the harness wired) claims and
// executes the job through the unchanged proxy.Client/usage.Recorder paths;
// GET /v1/jobs/{id} eventually reports status=succeeded with the worker's
// billable_units; exactly one usage_events row is recorded.
func TestAsyncInvoke_EndToEnd_SuccessAndPoll(t *testing.T) {
	withAsyncEcho(t, 0)

	ts := harness.NewGatewayTestServer(t, harness.Options{
		WorkerHandler: asyncWorkerHandler(t),
		DSN:           asyncTestPostgresDSN(t),
		RedisURL:      asyncTestRedisURL(t),
	})
	ts.CreatePlan(t, asyncTestPlanID, testRatePerMin, testMonthlyCap)
	customerID, apiKey := ts.CreateCustomer(t, "async-e2e-"+uuid.New().String()+"@example.com", asyncTestPlanID)
	// async_jobs rows reference customers/api_keys by FK; the harness's own
	// CreateCustomer cleanup doesn't know about this package-scoped table
	// (out of harness.go's scope), so delete them here first. t.Cleanup runs
	// LIFO, so registering this after CreateCustomer means it runs before
	// CreateCustomer's cleanup, avoiding an FK-violation cleanup failure.
	t.Cleanup(func() {
		_, _ = ts.DB.Exec(context.Background(), `DELETE FROM async_jobs WHERE customer_id = $1`, customerID)
	})

	// jobStore := jobs.NewStore(ts.DB) is already wired into ts.Server's router
	// internally by server.NewRouter (built from ts.DB) for the enqueue/poll
	// HTTP handlers. The harness does not start a background executor, so one
	// is constructed here against the same Postgres pool and worker the
	// harness already stood up — this is the only piece not already exercised
	// by harness.NewGatewayTestServer.
	quotaTracker := quota.New(ts.Redis)
	recorder := usage.NewRecorder(ts.DB, quotaTracker)
	proxyClient := proxy.New(ts.Worker.URL, testRequestTimeout, 8)
	store := jobs.NewStore(ts.DB)
	executor := jobs.NewExecutor(store, proxyClient, recorder, jobs.ExecutorConfig{
		PoolSize:     2,
		PollInterval: 50 * time.Millisecond,
		JobTimeout:   testRequestTimeout,
	})
	execCtx, execCancel := context.WithCancel(context.Background())
	t.Cleanup(execCancel)
	go executor.Run(execCtx)

	client := &http.Client{Timeout: testHTTPClientTimeout}
	t.Cleanup(client.CloseIdleConnections)

	route := server.V1Routes[0]
	sampleBody := route.SampleRequest
	if sampleBody == nil {
		sampleBody = json.RawMessage(`{}`)
	}

	enqCtx, enqCancel := context.WithTimeout(context.Background(), testRequestTimeout)
	defer enqCancel()
	enqReq, err := http.NewRequestWithContext(enqCtx, http.MethodPost, ts.Server.URL+"/v1"+route.Path, bytes.NewReader(sampleBody))
	if err != nil {
		t.Fatalf("build enqueue request: %v", err)
	}
	enqReq.Header.Set("Authorization", "Bearer "+apiKey)
	enqReq.Header.Set("Content-Type", "application/json")

	enqResp, err := client.Do(enqReq)
	if err != nil {
		t.Fatalf("send enqueue request: %v", err)
	}
	enqBody, err := io.ReadAll(enqResp.Body)
	enqResp.Body.Close()
	if err != nil {
		t.Fatalf("read enqueue response: %v", err)
	}
	if enqResp.StatusCode != http.StatusAccepted {
		t.Fatalf("enqueue: want 202, got %d: %s", enqResp.StatusCode, enqBody)
	}
	var enq enqueueResponse
	if err := json.Unmarshal(enqBody, &enq); err != nil {
		t.Fatalf("decode enqueue response: %v\nbody: %s", err, enqBody)
	}
	if enq.JobID == "" {
		t.Fatal("enqueue response has empty job_id")
	}

	deadline := time.Now().Add(testRequestTimeout)
	var final jobStatusResponse
	for {
		getCtx, getCancel := context.WithTimeout(context.Background(), testRequestTimeout)
		getReq, err := http.NewRequestWithContext(getCtx, http.MethodGet, ts.Server.URL+"/v1/jobs/"+enq.JobID, nil)
		if err != nil {
			getCancel()
			t.Fatalf("build poll request: %v", err)
		}
		getReq.Header.Set("Authorization", "Bearer "+apiKey)
		getResp, err := client.Do(getReq)
		getCancel()
		if err != nil {
			t.Fatalf("send poll request: %v", err)
		}
		getBody, err := io.ReadAll(getResp.Body)
		getResp.Body.Close()
		if err != nil {
			t.Fatalf("read poll response: %v", err)
		}
		if getResp.StatusCode != http.StatusOK {
			t.Fatalf("poll: want 200, got %d: %s", getResp.StatusCode, getBody)
		}
		if err := json.Unmarshal(getBody, &final); err != nil {
			t.Fatalf("decode poll response: %v\nbody: %s", err, getBody)
		}
		if final.Status == jobs.StatusSucceeded || final.Status == jobs.StatusFailed {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("job %s did not reach a terminal state within %s; last status: %s", enq.JobID, testRequestTimeout, getBody)
		}
		time.Sleep(50 * time.Millisecond)
	}

	if final.Status != jobs.StatusSucceeded {
		t.Fatalf("job status = %q, want %q (error: %+v)", final.Status, jobs.StatusSucceeded, final.Error)
	}
	if final.BillableUnits < 1 {
		t.Errorf("billable_units = %d, want >= 1", final.BillableUnits)
	}

	if got := ts.CountUsageEvents(t, customerID); got != 1 {
		t.Errorf("usage_events: got %d, want 1", got)
	}
}

// TestAsyncInvoke_JobStatus_IDORSafe proves GET /v1/jobs/{id} is scoped to
// the caller's own customer: a job id owned by customer A returns 404 when
// polled with customer B's API key, mirroring the existing
// webhookout customer-scoping tests (endpoints_test.go
// TestDeleteEndpointHandler_OwnedByOtherCustomer_404 and siblings). No
// executor is started — the job stays queued, which is irrelevant to the
// scoping check.
func TestAsyncInvoke_JobStatus_IDORSafe(t *testing.T) {
	withAsyncEcho(t, 0)

	ts := harness.NewGatewayTestServer(t, harness.Options{
		WorkerHandler: asyncWorkerHandler(t),
		DSN:           asyncTestPostgresDSN(t),
		RedisURL:      asyncTestRedisURL(t),
	})
	ts.CreatePlan(t, asyncTestPlanID, testRatePerMin, testMonthlyCap)
	ownerID, apiKeyOwner := ts.CreateCustomer(t, "async-idor-owner-"+uuid.New().String()+"@example.com", asyncTestPlanID)
	// See the identical comment in TestAsyncInvoke_EndToEnd_SuccessAndPoll:
	// async_jobs isn't known to harness.go's own cleanup.
	t.Cleanup(func() {
		_, _ = ts.DB.Exec(context.Background(), `DELETE FROM async_jobs WHERE customer_id = $1`, ownerID)
	})
	_, apiKeyAttacker := ts.CreateCustomer(t, "async-idor-attacker-"+uuid.New().String()+"@example.com", asyncTestPlanID)

	client := &http.Client{Timeout: testHTTPClientTimeout}
	t.Cleanup(client.CloseIdleConnections)

	route := server.V1Routes[0]
	sampleBody := route.SampleRequest
	if sampleBody == nil {
		sampleBody = json.RawMessage(`{}`)
	}

	enqCtx, enqCancel := context.WithTimeout(context.Background(), testRequestTimeout)
	defer enqCancel()
	enqReq, err := http.NewRequestWithContext(enqCtx, http.MethodPost, ts.Server.URL+"/v1"+route.Path, bytes.NewReader(sampleBody))
	if err != nil {
		t.Fatalf("build enqueue request: %v", err)
	}
	enqReq.Header.Set("Authorization", "Bearer "+apiKeyOwner)
	enqReq.Header.Set("Content-Type", "application/json")
	enqResp, err := client.Do(enqReq)
	if err != nil {
		t.Fatalf("send enqueue request: %v", err)
	}
	enqBody, err := io.ReadAll(enqResp.Body)
	enqResp.Body.Close()
	if err != nil {
		t.Fatalf("read enqueue response: %v", err)
	}
	if enqResp.StatusCode != http.StatusAccepted {
		t.Fatalf("enqueue: want 202, got %d: %s", enqResp.StatusCode, enqBody)
	}
	var enq enqueueResponse
	if err := json.Unmarshal(enqBody, &enq); err != nil {
		t.Fatalf("decode enqueue response: %v\nbody: %s", err, enqBody)
	}

	// Attacker polls the owner's job id.
	attackCtx, attackCancel := context.WithTimeout(context.Background(), testRequestTimeout)
	defer attackCancel()
	attackReq, err := http.NewRequestWithContext(attackCtx, http.MethodGet, ts.Server.URL+"/v1/jobs/"+enq.JobID, nil)
	if err != nil {
		t.Fatalf("build attacker poll request: %v", err)
	}
	attackReq.Header.Set("Authorization", "Bearer "+apiKeyAttacker)
	attackResp, err := client.Do(attackReq)
	if err != nil {
		t.Fatalf("send attacker poll request: %v", err)
	}
	attackBody, _ := io.ReadAll(attackResp.Body)
	attackResp.Body.Close()
	if attackResp.StatusCode != http.StatusNotFound {
		t.Fatalf("attacker poll: want 404 (IDOR-safe), got %d: %s", attackResp.StatusCode, attackBody)
	}

	// The owner must still be able to see their own job, proving the 404
	// above is scoping, not a broken job id.
	ownerCtx, ownerCancel := context.WithTimeout(context.Background(), testRequestTimeout)
	defer ownerCancel()
	ownerReq, err := http.NewRequestWithContext(ownerCtx, http.MethodGet, ts.Server.URL+"/v1/jobs/"+enq.JobID, nil)
	if err != nil {
		t.Fatalf("build owner poll request: %v", err)
	}
	ownerReq.Header.Set("Authorization", "Bearer "+apiKeyOwner)
	ownerResp, err := client.Do(ownerReq)
	if err != nil {
		t.Fatalf("send owner poll request: %v", err)
	}
	ownerBody, err := io.ReadAll(ownerResp.Body)
	ownerResp.Body.Close()
	if err != nil {
		t.Fatalf("read owner poll response: %v", err)
	}
	if ownerResp.StatusCode != http.StatusOK {
		t.Fatalf("owner poll: want 200, got %d: %s", ownerResp.StatusCode, ownerBody)
	}
	var ownerStatus jobStatusResponse
	if err := json.Unmarshal(ownerBody, &ownerStatus); err != nil {
		t.Fatalf("decode owner poll response: %v\nbody: %s", err, ownerBody)
	}
	if ownerStatus.JobID != enq.JobID {
		t.Errorf("owner poll job_id = %q, want %q", ownerStatus.JobID, enq.JobID)
	}
}
