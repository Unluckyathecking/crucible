package textkit

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"

	"github.com/Unluckyathecking/crucible/gateway/internal/openapi"
	"github.com/Unluckyathecking/crucible/gateway/internal/testdb"
	"github.com/Unluckyathecking/crucible/gateway/test/harness"
	crucible "github.com/Unluckyathecking/crucible/workers/sdk-go"
	"github.com/Unluckyathecking/crucible/workers/stubs/textkit/handler"
)

const (
	defaultTestRatePerMin = 100
	defaultTestMonthlyCap = 10_000

	testClientTimeout       = 25 * time.Second
	testDialTimeout         = 5 * time.Second
	testIdleConnTimeout     = 10 * time.Second
	testMaxIdleConns        = 10
	testMaxIdleConnsPerHost = 5
	testMaxConnsPerHost     = 10
	testRequestTimeout      = 10 * time.Second
)

// newTestHTTPClient returns an http.Client for a single test (one per test, not per request).
func newTestHTTPClient(t *testing.T) *http.Client {
	t.Helper()
	c := &http.Client{
		Timeout: testClientTimeout,
		Transport: &http.Transport{
			DialContext:           (&net.Dialer{Timeout: testDialTimeout}).DialContext,
			ResponseHeaderTimeout: testRequestTimeout,
			MaxIdleConns:          testMaxIdleConns,
			MaxIdleConnsPerHost:   testMaxIdleConnsPerHost,
			MaxConnsPerHost:       testMaxConnsPerHost,
			IdleConnTimeout:       testIdleConnTimeout,
		},
	}
	t.Cleanup(c.CloseIdleConnections)
	return c
}

func postgresDSN(t *testing.T) string {
	t.Helper()
	if v := os.Getenv("POSTGRES_DSN"); v != "" {
		return v
	}
	return testdb.DSN(t)
}

func redisURL(t *testing.T) string {
	t.Helper()
	if v := os.Getenv("REDIS_URL"); v != "" {
		return v
	}
	// Match the gateway/internal convention: fall back to the local default and
	// skip (not fail) when Redis is genuinely unreachable.
	const addr = "localhost:6379"
	c := redis.NewClient(&redis.Options{Addr: addr})
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := c.Ping(ctx).Err(); err != nil {
		t.Skipf("redis unavailable on %s, skipping: %v", addr, err)
	}
	return "redis://" + addr
}

// newTestServer boots the real gateway middleware chain with the textkit
// route table and an in-process handler.Handle worker, so every assertion
// below exercises auth, validate.Middleware, rate limit, quota, proxy, and
// usage recording exactly as production traffic would.
func newTestServer(t *testing.T) *harness.TestServer {
	t.Helper()
	h, err := crucible.HandlerWithConfig(handler.Handle, crucible.HandlerConfig{})
	if err != nil {
		t.Fatalf("build textkit handler: %v", err)
	}
	return harness.NewGatewayTestServer(t, harness.Options{
		Routes:        Routes,
		WorkerHandler: h,
		DSN:           postgresDSN(t),
		RedisURL:      redisURL(t),
	})
}

func drainBody(t *testing.T, r *http.Response) []byte {
	t.Helper()
	defer r.Body.Close()
	b, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatalf("drainBody: read body: %v", err)
	}
	return b
}

// errorCode extracts error.code from an apierror envelope; fatals if absent.
func errorCode(t *testing.T, body []byte) string {
	t.Helper()
	var env struct {
		Error *struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("decode apierror envelope: %v\nbody: %s", err, body)
	}
	if env.Error == nil || env.Error.Code == "" {
		t.Fatalf("apierror envelope missing error.code\nbody: %s", body)
	}
	return env.Error.Code
}

// invocationResponse is the JSON shape returned by every textkit operation.
type invocationResponse struct {
	Payload       json.RawMessage `json:"payload"`
	BillableUnits uint64          `json:"billable_units"`
	UnitsLabel    string          `json:"units_label"`
}

func postRoute(t *testing.T, client *http.Client, ts *harness.TestServer, apiKey, path, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, ts.Server.URL+"/v1"+path, strings.NewReader(body))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("send request: %v", err)
	}
	return resp
}

// TestTextkitRoutesDeclareResponseSchema asserts each of textkit's three
// routes declares a ResponseSchema, and that openapi.Build wraps it into the
// documented 200 body's payload property exactly as invokeResponseEnvelope
// does: {payload: <ResponseSchema>, billable_units: {type:"integer"},
// units_label: {type:"string"}}, matching each operation's handler response
// struct (workers/stubs/textkit/handler/handler.go). Pure/offline — unlike
// the rest of this file it needs no Postgres/Redis, since openapi.Build has
// no I/O.
func TestTextkitRoutesDeclareResponseSchema(t *testing.T) {
	wantPayloadProps := map[string][]string{
		handler.OpCountWords: {"words"},
		handler.OpTransform:  {"text"},
		handler.OpSlugify:    {"slug"},
	}

	doc := openapi.Build(Routes)
	for _, rt := range Routes {
		wantProps, ok := wantPayloadProps[rt.Operation]
		if !ok {
			t.Fatalf("operation %s: no expected payload properties declared in this test", rt.Operation)
		}
		if rt.ResponseSchema == nil {
			t.Fatalf("operation %s: RouteDescriptor.ResponseSchema is nil", rt.Operation)
		}

		item, ok := doc.Paths["/v1"+rt.Path]
		if !ok || item.Post == nil {
			t.Fatalf("operation %s: missing POST /v1%s in built document", rt.Operation, rt.Path)
		}
		media, ok := item.Post.Responses["200"].Content["application/json"]
		if !ok || media.Schema == nil {
			t.Fatalf("operation %s: missing 200 application/json schema", rt.Operation)
		}

		envelope := media.Schema
		if envelope.Type != "object" {
			t.Errorf("operation %s: 200 schema type = %q, want object", rt.Operation, envelope.Type)
		}
		if bu := envelope.Properties["billable_units"]; bu == nil || bu.Type != "integer" {
			t.Errorf("operation %s: 200 schema billable_units = %v, want {type:integer}", rt.Operation, bu)
		}
		if ul := envelope.Properties["units_label"]; ul == nil || ul.Type != "string" {
			t.Errorf("operation %s: 200 schema units_label = %v, want {type:string}", rt.Operation, ul)
		}

		payload := envelope.Properties["payload"]
		if payload == nil {
			t.Fatalf("operation %s: 200 schema missing payload property", rt.Operation)
		}
		if payload != rt.ResponseSchema {
			t.Errorf("operation %s: 200 schema payload is not the route's ResponseSchema", rt.Operation)
		}
		for _, prop := range wantProps {
			if _, ok := payload.Properties[prop]; !ok {
				t.Errorf("operation %s: payload schema missing property %q", rt.Operation, prop)
			}
		}
	}
}

// TestTextkitOperations drives every textkit route's declared SampleRequest
// through the real gateway middleware chain and asserts 200, billable_units
// >= 1, and exactly one usage_events row per operation — proving the
// multi-operation dispatch contract end-to-end for all three operations.
func TestTextkitOperations(t *testing.T) {
	t.Parallel()
	client := newTestHTTPClient(t)
	ts := newTestServer(t)
	ts.CreatePlan(t, "textkit-ops-plan", defaultTestRatePerMin, defaultTestMonthlyCap)

	for _, rt := range Routes {
		t.Run(rt.Operation, func(t *testing.T) {
			t.Parallel()
			customerID, apiKey := ts.CreateCustomer(t, "textkit-"+rt.Operation+"-"+uuid.New().String()+"@example.com", "textkit-ops-plan")

			resp := postRoute(t, client, ts, apiKey, rt.Path, string(rt.SampleRequest))
			body := drainBody(t, resp)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("operation %s: want 200, got %d: %s", rt.Operation, resp.StatusCode, body)
			}

			var inv invocationResponse
			if err := json.Unmarshal(body, &inv); err != nil {
				t.Fatalf("operation %s: decode response: %v\nbody: %s", rt.Operation, err, body)
			}
			if inv.BillableUnits < 1 {
				t.Errorf("operation %s: billable_units = %d, want >= 1", rt.Operation, inv.BillableUnits)
			}

			if n := ts.CountUsageEvents(t, customerID); n != 1 {
				t.Errorf("operation %s: usage_events = %d, want 1", rt.Operation, n)
			}
		})
	}
}

// TestTextkitCountWordsVariableBilling proves count-words meters a computed
// quantity (word count), not a flat 1, with a non-empty units_label.
func TestTextkitCountWordsVariableBilling(t *testing.T) {
	t.Parallel()
	client := newTestHTTPClient(t)
	ts := newTestServer(t)
	ts.CreatePlan(t, "textkit-count-plan", defaultTestRatePerMin, defaultTestMonthlyCap)
	_, apiKey := ts.CreateCustomer(t, "textkit-count-"+uuid.New().String()+"@example.com", "textkit-count-plan")

	resp := postRoute(t, client, ts, apiKey, "/textkit/count-words", `{"text":"one two three four five"}`)
	body := drainBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", resp.StatusCode, body)
	}
	var inv invocationResponse
	if err := json.Unmarshal(body, &inv); err != nil {
		t.Fatalf("decode response: %v\nbody: %s", err, body)
	}
	if inv.BillableUnits != 5 {
		t.Errorf("billable_units: got %d, want 5 (word count, not a flat 1)", inv.BillableUnits)
	}
	if inv.UnitsLabel != "words" {
		t.Errorf("units_label: got %q, want %q", inv.UnitsLabel, "words")
	}
}

// TestTextkitInvalidPayloadRejectedBySchema proves RequestSchema is enforced
// by validate.Middleware before the worker is ever invoked: an out-of-enum
// mode on /textkit/transform must fail with 400 BAD_REQUEST, and the
// rejected request must not bill.
func TestTextkitInvalidPayloadRejectedBySchema(t *testing.T) {
	t.Parallel()
	client := newTestHTTPClient(t)
	ts := newTestServer(t)
	ts.CreatePlan(t, "textkit-invalid-plan", defaultTestRatePerMin, defaultTestMonthlyCap)
	customerID, apiKey := ts.CreateCustomer(t, "textkit-invalid-"+uuid.New().String()+"@example.com", "textkit-invalid-plan")

	resp := postRoute(t, client, ts, apiKey, "/textkit/transform", `{"text":"hi","mode":"sideways"}`)
	body := drainBody(t, resp)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("want 400, got %d: %s", resp.StatusCode, body)
	}
	if code := errorCode(t, body); code != "BAD_REQUEST" {
		t.Errorf("error.code: got %q, want BAD_REQUEST", code)
	}
	if n := ts.CountUsageEvents(t, customerID); n != 0 {
		t.Errorf("usage_events after schema rejection: got %d, want 0", n)
	}
}

// TestTextkitCountWordsRejectsWhitespaceOnly proves whitespace-only text is
// rejected by the count-words route's Pattern constraint rather than being
// accepted and billed 1 unit for a response that reports words:0.
func TestTextkitCountWordsRejectsWhitespaceOnly(t *testing.T) {
	t.Parallel()
	client := newTestHTTPClient(t)
	ts := newTestServer(t)
	ts.CreatePlan(t, "textkit-ws-plan", defaultTestRatePerMin, defaultTestMonthlyCap)
	customerID, apiKey := ts.CreateCustomer(t, "textkit-ws-"+uuid.New().String()+"@example.com", "textkit-ws-plan")

	resp := postRoute(t, client, ts, apiKey, "/textkit/count-words", `{"text":"   "}`)
	body := drainBody(t, resp)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("want 400, got %d: %s", resp.StatusCode, body)
	}
	if code := errorCode(t, body); code != "BAD_REQUEST" {
		t.Errorf("error.code: got %q, want BAD_REQUEST", code)
	}
	if n := ts.CountUsageEvents(t, customerID); n != 0 {
		t.Errorf("usage_events after whitespace-only rejection: got %d, want 0", n)
	}
}
