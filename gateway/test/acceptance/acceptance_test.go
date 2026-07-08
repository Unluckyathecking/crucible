// Package acceptance proves that a cloned tree's shipped worker (workers/active)
// satisfies the frozen HTTP/JSON contract end-to-end: a real metered /v1/<op>
// request flows through the real gateway middleware chain (via
// gateway/test/harness) to the real worker binary and into billing.
//
// Unlike gateway/test/scenarios, which drives the pipeline against an
// in-process http.Handler mock, this test targets an already-running external
// worker process supplied via harness.Options.WorkerURL. scripts/acceptance-run.sh
// builds and starts that process (the workers/active binary) before running
// this test and exports its address as WORKER_URL.
package acceptance

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Unluckyathecking/crucible/gateway/test/harness"
)

const (
	testPlanID            = "acceptance"
	testRatePerMin        = 100
	testMonthlyCap        = 10_000
	testRequestTimeout    = 10 * time.Second
	testHTTPClientTimeout = 25 * time.Second
)

func postgresDSN(t *testing.T) string {
	t.Helper()
	v := os.Getenv("POSTGRES_DSN")
	if v == "" {
		t.Fatal("POSTGRES_DSN not set; required for acceptance tests")
	}
	return v
}

func redisURL(t *testing.T) string {
	t.Helper()
	v := os.Getenv("REDIS_URL")
	if v == "" {
		t.Fatal("REDIS_URL not set; required for acceptance tests")
	}
	return v
}

// workerURL returns the address of the already-running external worker
// process started by scripts/acceptance-run.sh (e.g. the workers/active binary).
// Unlike postgresDSN/redisURL, an absent WORKER_URL skips rather than fails:
// this package is opt-in (driven by scripts/acceptance-run.sh) and must not
// break the blanket `go test ./test/...` run in ci.yml, which does not start
// a worker process.
func workerURL(t *testing.T) string {
	t.Helper()
	v := os.Getenv("WORKER_URL")
	if v == "" {
		t.Skip("WORKER_URL not set; skipping — run via scripts/acceptance-run.sh, which starts the workers/active binary")
	}
	return v
}

// invocationResponse is the JSON shape the gateway returns for a successful /v1 invoke.
type invocationResponse struct {
	Payload       json.RawMessage `json:"payload"`
	BillableUnits uint64          `json:"billable_units"`
}

// TestClonedTreeRuntimeAcceptance drives one authenticated, metered /v1/echo
// request through the real gateway middleware chain into the real
// workers/active worker binary, and asserts the frozen contract holds:
// HTTP 200, billable_units >= 1 (both in the response body and the
// X-Billable-Units header the gateway sets from the worker's response), and
// exactly one usage_events row recorded for the customer.
func TestClonedTreeRuntimeAcceptance(t *testing.T) {
	client := &http.Client{Timeout: testHTTPClientTimeout}
	t.Cleanup(client.CloseIdleConnections)

	ts := harness.NewGatewayTestServer(t, harness.Options{
		WorkerURL: workerURL(t),
		DSN:       postgresDSN(t),
		RedisURL:  redisURL(t),
	})
	ts.CreatePlan(t, testPlanID, testRatePerMin, testMonthlyCap)
	customerID, apiKey := ts.CreateCustomer(t, "acceptance-"+uuid.New().String()+"@example.com", testPlanID)

	ctx, cancel := context.WithTimeout(context.Background(), testRequestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		ts.Server.URL+"/v1/echo",
		strings.NewReader(`{"x":"hi"}`),
	)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("send request: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response body: %v", err)
	}

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", resp.StatusCode, body)
	}

	var inv invocationResponse
	if err := json.Unmarshal(body, &inv); err != nil {
		t.Fatalf("decode response: %v\nbody: %s", err, body)
	}
	if inv.BillableUnits < 1 {
		t.Errorf("billable_units: got %d, want >= 1", inv.BillableUnits)
	}

	headerUnits := resp.Header.Get("X-Billable-Units")
	n, err := strconv.ParseUint(headerUnits, 10, 64)
	if err != nil || n < 1 {
		t.Errorf("X-Billable-Units header: got %q, want a value >= 1", headerUnits)
	}

	if got := ts.CountUsageEvents(t, customerID); got != 1 {
		t.Errorf("usage_events: got %d, want 1", got)
	}
}
