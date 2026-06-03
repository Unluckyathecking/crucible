// Package observability registers Prometheus metrics and the middleware that increments them.
//
// Metrics exposed on /metrics (separate listener on METRICS_PORT, off the public API surface):
//
//	crucible_requests_total{method,path,status}
//	crucible_request_duration_seconds{method,path}
//	crucible_worker_call_duration_seconds
//	crucible_worker_errors_total{code}
//	crucible_usage_records_total
//	crucible_billing_flush_total{outcome}
//	crucible_rate_limited_total
//	crucible_ratelimit_failopen_total
//	crucible_quota_failopen_total
package observability

import (
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/Unluckyathecking/crucible/gateway/internal/httputil"
)

var (
	requestsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "crucible_requests_total",
		Help: "Total HTTP requests handled by the gateway.",
	}, []string{"method", "path", "status"})

	requestDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "crucible_request_duration_seconds",
		Help:    "End-to-end request latency at the gateway, including worker call.",
		Buckets: []float64{0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10},
	}, []string{"method", "path"})

	WorkerCallDuration = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "crucible_worker_call_duration_seconds",
		Help:    "Latency of gateway → worker HTTP calls.",
		Buckets: []float64{0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10},
	})

	// WorkerErrorsTotal counts worker error responses by their structured error code.
	// The label is the bounded enum-like Code returned by the worker (e.g. INVALID_VAT_FORMAT) —
	// never a free-form message or per-request value, so cardinality stays bounded.
	WorkerErrorsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "crucible_worker_errors_total",
		Help: "Number of worker error responses, by structured error code.",
	}, []string{"code"})

	UsageRecordsTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "crucible_usage_records_total",
		Help: "Number of usage_events rows recorded.",
	})

	BillingFlushTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "crucible_billing_flush_total",
		Help: "Number of billing flush attempts. outcome=ok|error",
	}, []string{"outcome"})

	RateLimitedTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "crucible_rate_limited_total",
		Help: "Number of requests rejected for exceeding rate limits.",
	})

	// Fail-open counters: incremented when Redis is unreachable and the request is
	// admitted anyway (correct behaviour, but otherwise silent). A non-zero rate here
	// means the limiter/quota is degraded and customers may be exceeding their caps.
	RateLimitFailOpenTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "crucible_ratelimit_failopen_total",
		Help: "Number of requests admitted because the rate-limit store (Redis) was unreachable.",
	})

	QuotaFailOpenTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "crucible_quota_failopen_total",
		Help: "Number of requests admitted because the quota store (Redis) was unreachable.",
	})
)

// Metrics is a test-friendly holder for all observability counters.
// Use NewMetricsForTest with prometheus.NewRegistry() to get an isolated copy.
type Metrics struct {
	RequestsTotal      *prometheus.CounterVec
	RequestDuration    *prometheus.HistogramVec
	WorkerCallDuration prometheus.Histogram
	WorkerErrorsTotal  *prometheus.CounterVec
	UsageRecordsTotal  prometheus.Counter
	BillingFlushTotal  *prometheus.CounterVec
	RateLimitedTotal   prometheus.Counter
	RateLimitFailOpen  prometheus.Counter
	QuotaFailOpen      prometheus.Counter
}

// NewMetricsForTest creates all metrics registered against the supplied Registerer.
// Callers should use prometheus.NewRegistry() to avoid collisions with the
// package-level promauto vars that target prometheus.DefaultRegisterer.
func NewMetricsForTest(reg prometheus.Registerer) *Metrics {
	m := &Metrics{
		RequestsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "crucible_requests_total",
			Help: "Total HTTP requests handled by the gateway.",
		}, []string{"method", "path", "status"}),
		RequestDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "crucible_request_duration_seconds",
			Help:    "End-to-end request latency at the gateway, including worker call.",
			Buckets: []float64{0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10},
		}, []string{"method", "path"}),
		WorkerCallDuration: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "crucible_worker_call_duration_seconds",
			Help:    "Latency of gateway → worker HTTP calls.",
			Buckets: []float64{0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10},
		}),
		WorkerErrorsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "crucible_worker_errors_total",
			Help: "Number of worker error responses, by structured error code.",
		}, []string{"code"}),
		UsageRecordsTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "crucible_usage_records_total",
			Help: "Number of usage_events rows recorded.",
		}),
		BillingFlushTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "crucible_billing_flush_total",
			Help: "Number of billing flush attempts. outcome=ok|error",
		}, []string{"outcome"}),
		RateLimitedTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "crucible_rate_limited_total",
			Help: "Number of requests rejected for exceeding rate limits.",
		}),
		RateLimitFailOpen: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "crucible_ratelimit_failopen_total",
			Help: "Number of requests admitted because the rate-limit store (Redis) was unreachable.",
		}),
		QuotaFailOpen: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "crucible_quota_failopen_total",
			Help: "Number of requests admitted because the quota store (Redis) was unreachable.",
		}),
	}
	reg.MustRegister(
		m.RequestsTotal,
		m.RequestDuration,
		m.WorkerCallDuration,
		m.WorkerErrorsTotal,
		m.UsageRecordsTotal,
		m.BillingFlushTotal,
		m.RateLimitedTotal,
		m.RateLimitFailOpen,
		m.QuotaFailOpen,
	)
	return m
}

// Middleware records request count + latency. Plug into the chi router after RequestID.
// IMPORTANT: must run INSIDE chi's router so RoutePattern() is populated — otherwise random
// 404 paths from attackers would explode metric label cardinality.
func Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := httputil.NewStatusRecorder(w)
		next.ServeHTTP(rec, r)

		// Use chi's matched route pattern when available — bounded cardinality.
		// Fall back to "unmatched" for 404s so a malicious client spamming random paths
		// can't blow up Prometheus's series count.
		path := chi.RouteContext(r.Context()).RoutePattern()
		if path == "" {
			path = "unmatched"
		}
		method := r.Method
		status := strconv.Itoa(rec.Status)
		requestsTotal.WithLabelValues(method, path, status).Inc()
		requestDuration.WithLabelValues(method, path).Observe(time.Since(start).Seconds())
	})
}

// Handler returns the /metrics HTTP handler.
func Handler() http.Handler {
	return promhttp.Handler()
}

// Middleware records request count + latency using the supplied Metrics.
// Equivalent to the package-level Middleware but allows injecting test registries.
func (m *Metrics) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := httputil.NewStatusRecorder(w)
		next.ServeHTTP(rec, r)

		path := chi.RouteContext(r.Context()).RoutePattern()
		if path == "" {
			path = "unmatched"
		}
		method := r.Method
		status := strconv.Itoa(rec.Status)
		m.RequestsTotal.WithLabelValues(method, path, status).Inc()
		m.RequestDuration.WithLabelValues(method, path).Observe(time.Since(start).Seconds())
	})
}
