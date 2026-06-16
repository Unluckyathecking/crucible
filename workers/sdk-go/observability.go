package crucible

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/rs/zerolog/log"
)

// Metric name constants — byte-identical across Go/Rust/TS SDKs (parity contract).
const (
	metricRequestsTotal = "crucible_worker_requests_total"
	metricErrorsTotal   = "crucible_worker_errors_total"
	metricDurationSecs  = "crucible_worker_request_duration_seconds"
)

// workerMetrics holds the three Prometheus instruments for a worker process.
// Label set is exactly {operation, outcome}; outcome ∈ {ok, error}.
// Cardinality is bounded so long as the set of operation strings is bounded —
// the same invariant as the gateway's RoutePattern() label.
type workerMetrics struct {
	registry *prometheus.Registry
	requests *prometheus.CounterVec
	errors   *prometheus.CounterVec
	duration *prometheus.HistogramVec
}

func newWorkerMetrics() *workerMetrics {
	reg := prometheus.NewRegistry()

	requests := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: metricRequestsTotal,
		Help: "Total /invoke requests handled by the worker.",
	}, []string{"operation", "outcome"})

	errs := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: metricErrorsTotal,
		Help: "Total /invoke requests that returned an error envelope.",
	}, []string{"operation", "outcome"})

	dur := prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    metricDurationSecs,
		Help:    "Latency of /invoke handler calls in seconds.",
		Buckets: prometheus.DefBuckets,
	}, []string{"operation", "outcome"})

	reg.MustRegister(requests, errs, dur)

	return &workerMetrics{
		registry: reg,
		requests: requests,
		errors:   errs,
		duration: dur,
	}
}

// observe records one /invoke call. Call only after a successful Request decode so
// operation is always the product-defined operation string — never a URL, request-id,
// payload fragment, or other unbounded per-request value.
func (m *workerMetrics) observe(operation, outcome string, elapsed time.Duration) {
	labels := prometheus.Labels{"operation": operation, "outcome": outcome}
	m.requests.With(labels).Inc()
	if outcome == "error" {
		m.errors.With(labels).Inc()
	}
	m.duration.With(labels).Observe(elapsed.Seconds())
}

func (m *workerMetrics) httpHandler() http.Handler {
	return promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{})
}

// startMetricsListener binds port synchronously, then serves /metrics in a goroutine.
// Returns the running *http.Server and nil on success, or nil and an error if the port
// can't be bound — the caller can then return nil from initMetrics rather than handing
// back a metrics handle that is never accessible.
// The separate listener ensures /metrics is never accidentally exposed through the
// same load-balancer rule as /invoke.
func startMetricsListener(port int, m *workerMetrics) (*http.Server, error) {
	ln, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		return nil, err
	}
	mux := http.NewServeMux()
	mux.Handle("/metrics", m.httpHandler())
	srv := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		if serveErr := srv.Serve(ln); serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
			log.Error().Err(serveErr).Int("metrics_port", port).Msg("worker metrics listener failed")
		}
	}()
	return srv, nil
}
