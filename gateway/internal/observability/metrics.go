// Package observability registers Prometheus metrics and the middleware that increments them.
//
// Metrics exposed on /metrics (separate listener on METRICS_PORT, off the public API surface):
//
//	crucible_requests_total{method,path,status}          — per-request counter (Middleware)
//	crucible_request_duration_seconds{method,path}       — end-to-end latency (Middleware)
//	crucible_worker_call_duration_seconds                — per-attempt worker latency (proxy.Client)
//	crucible_worker_errors_total{code}                   — structured worker error codes (route handler)
//	crucible_worker_retries_total                        — retry attempts past breaker gate (proxy.Client)
//	crucible_worker_breaker_state                        — circuit-breaker state 0/1/2 (proxy.Client)
//	crucible_usage_records_total
//	crucible_billing_flush_total{outcome}
//	crucible_billing_backlog_units                       — unflushed billable_units (label-free gauge, flusher tick)
//	crucible_billing_backlog_rows                        — unflushed row count (label-free gauge, flusher tick)
//	crucible_billing_backlog_oldest_age_seconds          — age of oldest unflushed row (label-free gauge, flusher tick)
//	crucible_billing_unbillable_units                    — unflushed units with no Stripe customer (label-free gauge, flusher tick)
//	crucible_billing_unbillable_rows                     — unflushed row count with no Stripe customer (label-free gauge, flusher tick)
//	crucible_billing_reconcile_errors_total              — flusher ticks where at least one reconcile query failed; non-zero means gauges may be stale
//	crucible_rate_limited_total
//	crucible_quota_exceeded_total
//	crucible_ratelimit_failopen_total
//	crucible_quota_failopen_total
//	crucible_respcache_hits_total{operation}             — requests served from respcache (worker skipped)
//	crucible_respcache_misses_total{operation}           — respcache lookups that missed (worker invoked)
//	crucible_respcache_failopen_total{operation}         — requests admitted because Redis store errored
//
// Note: worker_retries_total and worker_breaker_state are recorded by proxy.Client, not by
// Middleware — they are worker-call-scoped, not HTTP-request-scoped.
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
		Help:    "Per-attempt latency of gateway→worker HTTP calls (one observation per attempt, including retries).",
		Buckets: []float64{0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10},
	})

	// WorkerErrorsTotal counts worker error responses by their structured error code.
	// The label is the bounded enum-like Code returned by the worker (e.g. INVALID_VAT_FORMAT) —
	// never a free-form message or per-request value, so cardinality stays bounded.
	WorkerErrorsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "crucible_worker_errors_total",
		Help: "Number of worker error responses, by structured error code.",
	}, []string{"code"})

	// WorkerRetriesTotal counts retry attempts dispatched past the breaker gate and ctx check.
	WorkerRetriesTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "crucible_worker_retries_total",
		Help: "Number of worker call retry attempts dispatched past the breaker gate (attempt > 0).",
	})

	// WorkerBreakerState tracks the current circuit-breaker state:
	// 0 = closed (healthy), 1 = open (fast-failing), 2 = half-open (probing).
	WorkerBreakerState = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "crucible_worker_breaker_state",
		Help: "Current circuit-breaker state: 0=closed 1=open 2=half-open.",
	})

	UsageRecordsTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "crucible_usage_records_total",
		Help: "Number of usage_events rows recorded.",
	})

	BillingFlushTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "crucible_billing_flush_total",
		Help: "Number of billing flush attempts. outcome=ok|error",
	}, []string{"outcome"})

	// BillingBacklogUnits is the current count of unflushed billable_units across all customers.
	// Set by the flusher tick after both flush phases via the reconciler. Label-free.
	BillingBacklogUnits = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "crucible_billing_backlog_units",
		Help: "Current unflushed billable_units in usage_events (label-free; set each flusher tick).",
	})

	// BillingBacklogRows is the number of unflushed usage_events rows (not units).
	// Useful alongside BillingBacklogUnits: 1M units in 2 rows vs 1M rows indicates
	// very different operational profiles. Label-free.
	BillingBacklogRows = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "crucible_billing_backlog_rows",
		Help: "Number of unflushed usage_events rows (label-free; set each flusher tick).",
	})

	// BillingBacklogOldestAgeSeconds is the age in seconds of the oldest unflushed row.
	// Zero when the backlog is empty. Label-free.
	BillingBacklogOldestAgeSeconds = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "crucible_billing_backlog_oldest_age_seconds",
		Help: "Age in seconds of the oldest unflushed usage_events row (label-free; set each flusher tick).",
	})

	// BillingUnbillableUnits is the count of unflushed billable_units for customers that have
	// no stripe_customer_id. These rows are permanently excluded from the flush filter and
	// represent a silent revenue leak unless the customer is linked to Stripe. Label-free.
	BillingUnbillableUnits = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "crucible_billing_unbillable_units",
		Help: "Unflushed billable_units for customers without a stripe_customer_id (label-free; set each flusher tick).",
	})

	// BillingUnbillableRows is the number of unflushed usage_events rows for customers
	// without a stripe_customer_id. Paired with BillingUnbillableUnits for operator context.
	BillingUnbillableRows = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "crucible_billing_unbillable_rows",
		Help: "Number of unflushed usage_events rows for customers without a stripe_customer_id (label-free; set each flusher tick).",
	})

	// BillingReconcileErrorsTotal counts flusher ticks where at least one reconcile query failed.
	// Incremented at most once per tick regardless of how many queries fail. A non-zero rate means
	// the backlog/unbillable gauges are stale and billing alerts may be unreliable.
	BillingReconcileErrorsTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "crucible_billing_reconcile_errors_total",
		Help: "Number of flusher ticks where at least one reconcile query failed (backlog/unbillable gauges may be stale when non-zero).",
	})

	RateLimitedTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "crucible_rate_limited_total",
		Help: "Number of requests rejected for exceeding rate limits.",
	})

	QuotaExceededTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "crucible_quota_exceeded_total",
		Help: "Number of requests rejected for exceeding monthly quota.",
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

	// RespCacheHitsTotal counts requests served from the content-addressed respcache,
	// skipping the worker round-trip. operation is bounded (fixed V1Routes set).
	RespCacheHitsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "crucible_respcache_hits_total",
		Help: "Number of requests served from the respcache (worker round-trip skipped), by operation.",
	}, []string{"operation"})

	// RespCacheMissesTotal counts respcache lookups that fell through to the worker.
	RespCacheMissesTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "crucible_respcache_misses_total",
		Help: "Number of respcache lookups that missed (worker was invoked), by operation.",
	}, []string{"operation"})

	// RespCacheFailOpenTotal counts requests admitted after a Redis store error.
	// A non-zero rate here means the respcache is degraded and every request is
	// hitting the worker — mirrors the crucible_ratelimit_failopen_total pattern.
	RespCacheFailOpenTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "crucible_respcache_failopen_total",
		Help: "Number of requests admitted because the respcache store (Redis) returned an error, by operation.",
	}, []string{"operation"})
)

// Metrics is a test-friendly holder for all observability counters.
// Use NewMetricsForTest with prometheus.NewRegistry() to get an isolated copy.
// Pass the result to the proxy client's metrics injection method for worker-call metrics in tests.
type Metrics struct {
	RequestsTotal                  *prometheus.CounterVec
	RequestDuration                *prometheus.HistogramVec
	WorkerCallDuration             prometheus.Histogram
	WorkerErrorsTotal              *prometheus.CounterVec
	WorkerRetriesTotal             prometheus.Counter
	WorkerBreakerState             prometheus.Gauge
	UsageRecordsTotal              prometheus.Counter
	BillingFlushTotal              *prometheus.CounterVec
	BillingBacklogUnits            prometheus.Gauge
	BillingBacklogRows             prometheus.Gauge
	BillingBacklogOldestAgeSeconds prometheus.Gauge
	BillingUnbillableUnits         prometheus.Gauge
	BillingUnbillableRows          prometheus.Gauge
	BillingReconcileErrorsTotal    prometheus.Counter
	RateLimitedTotal               prometheus.Counter
	QuotaExceededTotal             prometheus.Counter
	RateLimitFailOpen              prometheus.Counter
	QuotaFailOpen                  prometheus.Counter
	RespCacheHitsTotal             *prometheus.CounterVec
	RespCacheMissesTotal           *prometheus.CounterVec
	RespCacheFailOpenTotal         *prometheus.CounterVec
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
			Help:    "Per-attempt latency of gateway→worker HTTP calls (one observation per attempt, including retries).",
			Buckets: []float64{0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10},
		}),
		WorkerErrorsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "crucible_worker_errors_total",
			Help: "Number of worker error responses, by structured error code.",
		}, []string{"code"}),
		WorkerRetriesTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "crucible_worker_retries_total",
			Help: "Number of worker call retry attempts dispatched past the breaker gate (attempt > 0).",
		}),
		WorkerBreakerState: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "crucible_worker_breaker_state",
			Help: "Current circuit-breaker state: 0=closed 1=open 2=half-open.",
		}),
		UsageRecordsTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "crucible_usage_records_total",
			Help: "Number of usage_events rows recorded.",
		}),
		BillingFlushTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "crucible_billing_flush_total",
			Help: "Number of billing flush attempts. outcome=ok|error",
		}, []string{"outcome"}),
		BillingBacklogUnits: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "crucible_billing_backlog_units",
			Help: "Current unflushed billable_units in usage_events (label-free; set each flusher tick).",
		}),
		BillingBacklogRows: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "crucible_billing_backlog_rows",
			Help: "Number of unflushed usage_events rows (label-free; set each flusher tick).",
		}),
		BillingBacklogOldestAgeSeconds: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "crucible_billing_backlog_oldest_age_seconds",
			Help: "Age in seconds of the oldest unflushed usage_events row (label-free; set each flusher tick).",
		}),
		BillingUnbillableUnits: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "crucible_billing_unbillable_units",
			Help: "Unflushed billable_units for customers without a stripe_customer_id (label-free; set each flusher tick).",
		}),
		BillingUnbillableRows: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "crucible_billing_unbillable_rows",
			Help: "Number of unflushed usage_events rows for customers without a stripe_customer_id (label-free; set each flusher tick).",
		}),
		BillingReconcileErrorsTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "crucible_billing_reconcile_errors_total",
			Help: "Number of flusher ticks where at least one reconcile query failed (backlog/unbillable gauges may be stale when non-zero).",
		}),
		RateLimitedTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "crucible_rate_limited_total",
			Help: "Number of requests rejected for exceeding rate limits.",
		}),
		QuotaExceededTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "crucible_quota_exceeded_total",
			Help: "Number of requests rejected for exceeding monthly quota.",
		}),
		RateLimitFailOpen: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "crucible_ratelimit_failopen_total",
			Help: "Number of requests admitted because the rate-limit store (Redis) was unreachable.",
		}),
		QuotaFailOpen: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "crucible_quota_failopen_total",
			Help: "Number of requests admitted because the quota store (Redis) was unreachable.",
		}),
		RespCacheHitsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "crucible_respcache_hits_total",
			Help: "Number of requests served from the respcache (worker round-trip skipped), by operation.",
		}, []string{"operation"}),
		RespCacheMissesTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "crucible_respcache_misses_total",
			Help: "Number of respcache lookups that missed (worker was invoked), by operation.",
		}, []string{"operation"}),
		RespCacheFailOpenTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "crucible_respcache_failopen_total",
			Help: "Number of requests admitted because the respcache store (Redis) returned an error, by operation.",
		}, []string{"operation"}),
	}
	reg.MustRegister(
		m.RequestsTotal,
		m.RequestDuration,
		m.WorkerCallDuration,
		m.WorkerErrorsTotal,
		m.WorkerRetriesTotal,
		m.WorkerBreakerState,
		m.UsageRecordsTotal,
		m.BillingFlushTotal,
		m.BillingBacklogUnits,
		m.BillingBacklogRows,
		m.BillingBacklogOldestAgeSeconds,
		m.BillingUnbillableUnits,
		m.BillingUnbillableRows,
		m.BillingReconcileErrorsTotal,
		m.RateLimitedTotal,
		m.QuotaExceededTotal,
		m.RateLimitFailOpen,
		m.QuotaFailOpen,
		m.RespCacheHitsTotal,
		m.RespCacheMissesTotal,
		m.RespCacheFailOpenTotal,
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
