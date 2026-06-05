// Package proxy forwards Invoke() calls from the gateway to a worker over HTTP/JSON.
// The wire encoding matches gateway/proto/tool.proto. gRPC mode is a future swap;
// this layer is the seam where transport choice lives.
package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/Unluckyathecking/crucible/gateway/internal/observability"
	"github.com/Unluckyathecking/crucible/gateway/internal/resilience"
)

const (
	defaultTimeout = 30 * time.Second
	// defaultMaxConns caps total connections per worker host so a slow worker
	// can't pin gateway sockets/goroutines without bound. Used when maxConns <= 0.
	defaultMaxConns = 64

	// statusNone is the sentinel status returned by doOnce when the call failed
	// before an HTTP response was received for reasons other than a transport error
	// (e.g. request-build failure). It is not retryable.
	statusNone = -1
)

// InvokeRequest mirrors the proto for HTTP/JSON wire encoding.
type InvokeRequest struct {
	RequestID  string            `json:"request_id"`
	CustomerID string            `json:"customer_id"`
	Operation  string            `json:"operation"`
	Payload    json.RawMessage   `json:"payload"`
	Plan       string            `json:"plan"`
	Metadata   map[string]string `json:"metadata,omitempty"`
}

// InvokeResponse mirrors the proto. Either Payload or Error is set per call.
type InvokeResponse struct {
	Payload       json.RawMessage `json:"payload,omitempty"`
	Error         *InvokeError    `json:"error,omitempty"`
	BillableUnits uint64          `json:"billable_units"`
	UnitsLabel    string          `json:"units_label,omitempty"`
}

type InvokeError struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	Retryable bool   `json:"retryable"`
}

// ResiliencePolicy bundles the retry and circuit-breaker configuration for a Client.
// Zero value disables both (single-shot, no breaker), reproducing today's behaviour.
type ResiliencePolicy struct {
	Retry   resilience.Policy
	Breaker resilience.BreakerConfig
}

// Client forwards InvokeRequests to a worker.
type Client struct {
	workerURL string
	http      *http.Client
	retry     resilience.Policy
	breaker   *resilience.Breaker
}

// New creates a proxy client. If timeout is 0 or negative, it defaults
// to a 30s timeout to prevent infinite hangs from unresponsive workers.
// maxConns caps total connections per worker host; if 0 or negative it
// defaults to 64 so a slow worker can't exhaust gateway sockets/goroutines.
// An optional ResiliencePolicy enables retries and circuit-breaking; the zero
// value (omitted) disables both, preserving today's single-shot behaviour.
func New(workerURL string, timeout time.Duration, maxConns int, policies ...ResiliencePolicy) *Client {
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	if maxConns <= 0 {
		maxConns = defaultMaxConns
	}
	var pol ResiliencePolicy
	if len(policies) > 0 {
		pol = policies[0]
	}
	var b *resilience.Breaker
	if pol.Breaker.Threshold > 0 {
		b = resilience.NewBreaker(pol.Breaker, func(s resilience.State) {
			observability.WorkerBreakerState.Set(float64(s))
		})
	}
	return &Client{
		workerURL: workerURL,
		http: &http.Client{
			Timeout: timeout,
			Transport: &http.Transport{
				DialContext: (&net.Dialer{
					Timeout:   2 * time.Second,
					KeepAlive: 30 * time.Second,
				}).DialContext,
				MaxConnsPerHost:     maxConns,
				MaxIdleConns:        100,
				MaxIdleConnsPerHost: 50,
				IdleConnTimeout:     90 * time.Second,
				// No ResponseHeaderTimeout: workers write the whole response only after
				// their handler returns, so header latency == total handler latency. A
				// fixed ceiling here would silently cap legitimate workers below the
				// configured WORKER_TIMEOUT_MS. Total time is already bounded by the
				// per-request context deadline (and Client.Timeout); connection stalls
				// are bounded by the Dialer connect timeout above.
			},
		},
		retry:   pol.Retry,
		breaker: b,
	}
}

// WithBreakerClock injects a test clock into the circuit breaker. Intended for
// deterministic tests only; do not call after Invoke goroutines are running.
func (c *Client) WithBreakerClock(now func() time.Time) *Client {
	if c.breaker != nil {
		c.breaker.WithNow(now)
	}
	return c
}

// Invoke POSTs an InvokeRequest to the worker's /invoke endpoint and decodes the response.
// Returns a transport error if the network call fails or the worker returns an unexpected shape.
// Returns a successful (*InvokeResponse, nil) if the worker returned a structured error envelope.
// With a ResiliencePolicy, retries transport errors and 5xx responses up to MaxAttempts times;
// HTTP 200 responses (including worker error envelopes) are never retried.
// All non-nil errors (transport failures, circuit-breaker rejection, 5xx) are equivalent at
// the caller boundary — the route handler maps every non-nil error to 502 WORKER_UNREACHABLE
// with retryable=true, so callers must not branch on the specific error value.
func (c *Client) Invoke(ctx context.Context, in *InvokeRequest) (*InvokeResponse, error) {
	body, err := json.Marshal(in)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	maxAttempts := c.retry.MaxAttempts
	if maxAttempts <= 0 {
		maxAttempts = 1
	}

	for attempt := 0; ; attempt++ {
		if attempt > 0 {
			// n is 0-indexed: attempt-1 gives n=0 (base) for first retry,
			// n=1 (base*2) for second retry, etc.
			if err := c.retry.Sleep(ctx, attempt-1); err != nil {
				return nil, err
			}
			// Confirm context is still live before proceeding.
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}

		// Belt-and-suspenders ctx guard: IsRetryable already rejects
		// context.DeadlineExceeded and context.Canceled, so in-flight context
		// errors stop retries via the IsRetryable check. This catches the narrow
		// window where a successful Sleep returns but ctx.Err() is already set.
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		// Circuit-breaker admission: fast-fail without a network call when open.
		if c.breaker != nil {
			if err := c.breaker.Allow(); err != nil {
				return nil, fmt.Errorf("worker call: %w", err)
			}
		}

		// Count only actual retry attempts dispatched past the breaker gate.
		if attempt > 0 {
			observability.WorkerRetriesTotal.Inc()
		}

		resp, status, err := c.doOnce(ctx, body, in.RequestID)

		// Update breaker state based on the outcome.
		// err==nil is checked first: a successful HTTP 200 closes the breaker regardless
		// of ctx state, since the worker already did the work and we must record that.
		if c.breaker != nil {
			switch {
			case err == nil:
				c.breaker.RecordSuccess() // clean HTTP 200 — worker is healthy
			case ctx.Err() != nil:
				// Caller cancelled before/during call: release probe slot without a verdict.
				c.breaker.RecordAbort()
			case status == statusNone:
				// Pre-flight build error: never reached the worker; release any probe slot.
				c.breaker.RecordAbort()
			case status >= 500:
				c.breaker.RecordFailure() // HTTP 5xx
			case status == 0:
				c.breaker.RecordFailure() // transport/network error, no HTTP response
			default:
				// 4xx or 200 decode error: worker responded but outcome is ambiguous.
				// Release any half-open probe slot without recording a health verdict.
				c.breaker.RecordAbort()
			}
		}

		if err == nil {
			return resp, nil
		}

		if !resilience.IsRetryable(err, status) || attempt+1 >= maxAttempts {
			return nil, err
		}
	}
}

// doOnce executes a single HTTP attempt against the worker.
// Returns (response, statusCode, error):
//   - error != nil, status == 0: transport/network error before HTTP response (retryable)
//   - error != nil, status == statusNone: pre-flight build error (not retryable)
//   - error != nil, status != 0: HTTP error (retryable if status >= 500)
//   - error == nil: HTTP 200, response decoded successfully
func (c *Client) doOnce(ctx context.Context, body []byte, requestID string) (*InvokeResponse, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.workerURL+"/invoke", bytes.NewReader(body))
	if err != nil {
		return nil, statusNone, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if requestID != "" {
		req.Header.Set("X-Request-ID", requestID)
	}

	start := time.Now()
	resp, err := c.http.Do(req)
	observability.WorkerCallDuration.Observe(time.Since(start).Seconds())
	if err != nil {
		if resp != nil && resp.Body != nil {
			resp.Body.Close()
		}
		return nil, 0, fmt.Errorf("worker call: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		// Surface up to 512 bytes of the body in the error — invaluable when triaging
		// a misbehaving worker. Customer-facing errors are still sanitised at the route
		// handler; this body only lands in the gateway's own structured logs.
		bodyPeek, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		// Drain anything past the peek so the connection can be reused from the pool.
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil, resp.StatusCode, fmt.Errorf("worker returned status %d: %s",
			resp.StatusCode, strings.TrimSpace(string(bodyPeek)))
	}

	// HTTP 200: decode. Errors here are NOT retryable — the worker already did the work.
	var out InvokeResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, http.StatusOK, fmt.Errorf("decode worker response: %w", err)
	}
	if out.Payload == nil && out.Error == nil {
		return nil, http.StatusOK, errors.New("worker returned neither payload nor error")
	}
	return &out, http.StatusOK, nil
}
