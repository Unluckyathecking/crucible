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
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/Unluckyathecking/crucible/gateway/internal/observability"
	"github.com/Unluckyathecking/crucible/gateway/internal/resilience"
)

const (
	defaultTimeout = 30 * time.Second
	// defaultMaxConns is the fallback connection ceiling when New() is called with
	// maxConns <= 0. In production the value comes from GATEWAY_WORKER_MAX_CONNS;
	// this constant is the in-process guard for callers that omit the argument.
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

// clientMetrics groups Prometheus instruments that WithMetrics can hot-swap atomically.
// Storing a pointer in atomic.Value ensures the swap is indivisible — a concurrent
// Invoke either observes the full old set or the full new set, never a partial mix.
// Using a pointer avoids copying the struct's three interface fields on every Load.
type clientMetrics struct {
	retriesTotal       prometheus.Counter
	breakerState       prometheus.Gauge
	workerCallDuration prometheus.Histogram
}

// Client forwards InvokeRequests to a worker.
type Client struct {
	workerURL string
	http      *http.Client
	retry     resilience.Policy
	breaker   *resilience.Breaker
	metrics   atomic.Value // always stores *clientMetrics; swapped atomically by WithMetrics
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
	if len(policies) > 1 {
		// The variadic is for optional-parameter ergonomics, not multi-policy composition.
		// Silently ignoring extras is a footgun; fail loudly instead.
		panic("proxy.New: at most one ResiliencePolicy may be provided")
	}
	var pol ResiliencePolicy
	if len(policies) > 0 {
		pol = policies[0]
	}
	c := &Client{
		workerURL: workerURL,
		retry:     pol.Retry,
		http: &http.Client{
			Timeout: timeout, // per-request ceiling; enforces WORKER_TIMEOUT_MS
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
	}
	c.metrics.Store(&clientMetrics{
		retriesTotal:       observability.WorkerRetriesTotal,
		breakerState:       observability.WorkerBreakerState,
		workerCallDuration: observability.WorkerCallDuration,
	})
	if pol.Breaker.Threshold > 0 {
		// s is the state at transition time. The breaker lock is already released
		// when onState fires, so a concurrent transition may advance the state past
		// s before this callback runs — transient gauge staleness is acceptable and
		// self-corrects on the next transition. Using s avoids accessing c through
		// the closure after New() returns, which would require separate synchronization.
		// The metrics load is atomic; the nil guard is belt-and-suspenders for any
		// zero-value Client construction that bypassed New().
		c.breaker = resilience.NewBreaker(pol.Breaker, func(s resilience.State) {
			if mv := c.metrics.Load(); mv != nil {
				mv.(*clientMetrics).breakerState.Set(float64(s))
			}
		})
	}
	return c
}

// WithBreakerClock injects a test clock into the circuit breaker. The clock is
// stored through the breaker's own mutex, so this call is safe to make at any time;
// prefer calling before dispatching concurrent Invoke calls to avoid surprising
// time-ordering during the race window.
func (c *Client) WithBreakerClock(now func() time.Time) *Client {
	if now == nil || c.breaker == nil {
		return c
	}
	c.breaker.WithNow(now)
	return c
}

// WithMetrics replaces the default package-level Prometheus vars with the metrics
// from an isolated test registry. The swap is atomic: a concurrent Invoke either
// observes the old metrics set or the new one, never a partial mix. Safe to call
// at any time, including while Invoke goroutines are running.
// Panics if m is non-nil but any of the three worker instruments are nil —
// partial Metrics structs are a programmer error that should be caught early.
func (c *Client) WithMetrics(m *observability.Metrics) *Client {
	if m != nil {
		if m.WorkerRetriesTotal == nil || m.WorkerBreakerState == nil || m.WorkerCallDuration == nil {
			panic("proxy.Client.WithMetrics: Metrics has nil worker instruments")
		}
		c.metrics.Store(&clientMetrics{
			retriesTotal:       m.WorkerRetriesTotal,
			breakerState:       m.WorkerBreakerState,
			workerCallDuration: m.WorkerCallDuration,
		})
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
	if in == nil {
		return nil, fmt.Errorf("worker call: nil InvokeRequest")
	}
	body, err := json.Marshal(in)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	mv := c.metrics.Load()
	if mv == nil {
		// metrics is always set by New(). A nil here means Client was zero-valued
		// directly instead of constructed via New() — that is unsupported.
		panic("proxy.Client: must be constructed with proxy.New()")
	}
	m := mv.(*clientMetrics)
	maxAttempts := c.retry.MaxAttempts
	if maxAttempts <= 0 {
		maxAttempts = 1
	}

	for attempt := 0; ; attempt++ {
		if attempt > 0 {
			// n is 0-indexed: attempt-1 gives n=0 (base) for first retry,
			// n=1 (base*2) for second retry, etc.
			if err := c.retry.Sleep(ctx, attempt-1); err != nil {
				return nil, fmt.Errorf("worker call: %w", err)
			}
		}

		// Context check before Allow() — executes on EVERY attempt, including
		// attempt 0 (no preceding Sleep). Ensures the context is alive before
		// acquiring the breaker lock. A second check after Allow() (below) covers
		// the narrow race window between mutex release and the network call.
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("worker call: %w", err)
		}

		// Circuit-breaker admission: fast-fail without a network call when open.
		// Allow returns a generation token; pass it to every Record* call so stale
		// results from earlier breaker generations are silently ignored.
		// From StateClosed, Allow returns token=0. Since probeGen is incremented to
		// >= 1 before any HalfOpen transition, a stale 0 token will not match the
		// active probeGen and is silently ignored by Record*. This is a consequence
		// of Allow() incrementing probeGen before assignment, not a general invariant.
		var breakerToken uint64
		if c.breaker != nil {
			var berr error
			breakerToken, berr = c.breaker.Allow()
			if berr != nil {
				return nil, fmt.Errorf("worker call: %w", berr)
			}
		}

		// Check context after Allow and before the network call: if the caller
		// cancelled between Allow and doOnce, release any half-open probe slot
		// and skip the wasted HTTP call.
		// RecordAbort here (pre-call cancellation) differs from the post-call
		// DeadlineExceeded path below: the worker was never contacted, so no
		// health signal exists — releasing the slot is the right verdict.
		if err := ctx.Err(); err != nil {
			if c.breaker != nil {
				c.breaker.RecordAbort(breakerToken)
			}
			return nil, fmt.Errorf("worker call: %w", err)
		}

		// Count the retry after all pre-flight gates so the metric reflects
		// actual calls dispatched to doOnce, not ones aborted by ctx or breaker.
		if attempt > 0 {
			m.retriesTotal.Inc()
		}

		resp, status, err := c.doOnce(ctx, body, in.RequestID, m)

		// Update breaker state BEFORE the retry-exhaustion check so every attempt,
		// including the final one, is recorded. Skipping this on retry exhaustion
		// would blind the breaker to sustained failures that drain the retry budget.
		// err==nil is checked first: a successful HTTP 200 closes the breaker regardless
		// of ctx state, since the worker already did the work and we must record that.
		if c.breaker != nil {
			switch {
			case err == nil:
				c.breaker.RecordSuccess(breakerToken) // clean HTTP 200 — worker is healthy
			case status >= 500:
				// HTTP 5xx: record failure even if ctx is cancelled — the worker produced
				// a real error response and that health signal must reach the breaker.
				c.breaker.RecordFailure(breakerToken)
			case status == statusNone:
				// Pre-flight build error (bad URL, request-build failure): the worker was
				// never contacted, so no health signal exists. Release the probe slot without
				// recording a verdict — this is a local config error, not worker health.
				c.breaker.RecordAbort(breakerToken)
			case status == 0 && !errors.Is(err, context.Canceled):
				// Transport/network error or per-request timeout (DeadlineExceeded or nil
				// ctx error): the worker was unreachable or too slow. Record as failure so
				// a sustained pattern opens the breaker and subsequent calls fast-fail
				// instead of accumulating wasted round-trips.
				// context.Canceled falls through to the default (RecordAbort): the caller
				// abandoned the request, not a worker health signal.
				c.breaker.RecordFailure(breakerToken)
			default:
				// Covers: caller cancelled (context.Canceled) with no HTTP response, 4xx,
				// HTTP 200 decode error. Worker outcome is ambiguous or caller abandoned —
				// release the probe slot without recording a health verdict.
				c.breaker.RecordAbort(breakerToken)
			}
		}

		if err == nil {
			return resp, nil
		}

		if !resilience.IsRetryable(ctx, err, status) || attempt+1 >= maxAttempts {
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
func (c *Client) doOnce(ctx context.Context, body []byte, requestID string, m *clientMetrics) (*InvokeResponse, int, error) {
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
	m.workerCallDuration.Observe(time.Since(start).Seconds())
	if err != nil {
		// resp can be non-nil on redirect/policy errors — close any body before
		// returning. Return statusNone so retry/breaker logic treats this as a
		// persistent non-retryable error, not a transient worker health failure.
		if resp != nil && resp.Body != nil {
			resp.Body.Close()
		}
		if resp != nil {
			return nil, statusNone, fmt.Errorf("worker call: %w", err)
		}
		return nil, 0, fmt.Errorf("worker call: %w", err)
	}
	// net/http guarantees resp != nil and resp.Body != nil when err == nil.
	// Defer is registered here — after the err check — so it covers exactly
	// the success path. It fires once when doOnce returns.
	defer resp.Body.Close()
	if resp == nil {
		return nil, 0, errors.New("worker call: nil response without error")
	}

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
