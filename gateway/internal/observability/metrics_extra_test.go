package observability

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

// histogramSampleCount drains the collector and returns the total sample count
// across all label combinations.
func histogramSampleCount(h prometheus.Collector) uint64 {
	ch := make(chan prometheus.Metric, 10)
	h.Collect(ch)
	close(ch)
	var total uint64
	for m := range ch {
		metric := dto.Metric{}
		_ = m.Write(&metric)
		if hist := metric.GetHistogram(); hist != nil {
			total += hist.GetSampleCount()
		}
	}
	return total
}

// counterVecValue returns the value for the given label set from a CounterVec.
// It returns -1 if the label combination was not found.
func counterVecValue(cv *prometheus.CounterVec, lvs ...string) float64 {
	c, err := cv.GetMetricWithLabelValues(lvs...)
	if err != nil {
		return -1
	}
	ch := make(chan prometheus.Metric, 1)
	c.Collect(ch)
	m := <-ch
	metric := dto.Metric{}
	_ = m.Write(&metric)
	return metric.GetCounter().GetValue()
}

// TestHandler_ServesMetrics verifies that Handler() returns a valid /metrics
// endpoint that includes at least the standard Go runtime metrics from the
// default Prometheus registry.
func TestHandler_ServesMetrics(t *testing.T) {
	srv := httptest.NewServer(Handler())
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("GET /metrics failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading body: %v", err)
	}
	bodyStr := string(body)

	// The default registry always includes Go runtime metrics.
	for _, want := range []string{"go_goroutines", "go_gc_duration_seconds"} {
		if !strings.Contains(bodyStr, want) {
			t.Errorf("metrics output missing %q", want)
		}
	}

	ct := resp.Header.Get("Content-Type")
	if !strings.Contains(ct, "text/plain") {
		t.Errorf("Content-Type = %q, want text/plain", ct)
	}
}

// TestPackageMiddleware_RoutePattern verifies that the package-level Middleware
// uses the chi route pattern as the path label (bounded cardinality) and not
// the raw URL path.
func TestPackageMiddleware_RoutePattern(t *testing.T) {
	r := chi.NewRouter()
	r.Use(Middleware)
	r.Get("/items/{id}", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	srv := httptest.NewServer(r)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/items/42")
	if err != nil {
		t.Fatalf("GET failed: %v", err)
	}
	resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	// The label must be the route template, not the concrete path.
	// We verify by reading the counter from the global vec.
	v := counterVecValue(requestsTotal, "GET", "/items/{id}", "200")
	if v < 1 {
		t.Errorf("requestsTotal{GET, /items/{id}, 200} = %v, want >= 1", v)
	}
}

// TestPackageMiddleware_Unmatched verifies that a 404 request records the label
// "unmatched" instead of the raw path, preventing unbounded label cardinality.
func TestPackageMiddleware_Unmatched(t *testing.T) {
	r := chi.NewRouter()
	r.Use(Middleware)
	r.Get("/only", func(w http.ResponseWriter, _ *http.Request) {})

	srv := httptest.NewServer(r)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/does/not/exist/at/all")
	if err != nil {
		t.Fatalf("GET failed: %v", err)
	}
	resp.Body.Close()

	// The path label for unregistered routes must be "unmatched", not the raw URL.
	v := counterVecValue(requestsTotal, "GET", "unmatched", "404")
	if v < 1 {
		t.Errorf("requestsTotal{GET, unmatched, 404} = %v, want >= 1", v)
	}
}

// TestPackageMiddleware_DurationRecorded verifies that the package-level
// Middleware observes at least one sample in the duration histogram.
func TestPackageMiddleware_DurationRecorded(t *testing.T) {
	before := histogramSampleCount(requestDuration)

	r := chi.NewRouter()
	r.Use(Middleware)
	r.Get("/health", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	srv := httptest.NewServer(r)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/health")
	if err != nil {
		t.Fatalf("GET failed: %v", err)
	}
	resp.Body.Close()

	after := histogramSampleCount(requestDuration)
	if after <= before {
		t.Errorf("requestDuration sample count did not increase (before=%d, after=%d)", before, after)
	}
}

// TestMetrics_BillingFlushTotal verifies the BillingFlushTotal counter vec
// records increments per outcome label.
func TestMetrics_BillingFlushTotal(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewMetricsForTest(reg)

	m.BillingFlushTotal.WithLabelValues("ok").Inc()
	m.BillingFlushTotal.WithLabelValues("ok").Inc()
	m.BillingFlushTotal.WithLabelValues("error").Inc()

	if v := counterVecValue(m.BillingFlushTotal, "ok"); v != 2 {
		t.Errorf("BillingFlushTotal{ok} = %v, want 2", v)
	}
	if v := counterVecValue(m.BillingFlushTotal, "error"); v != 1 {
		t.Errorf("BillingFlushTotal{error} = %v, want 1", v)
	}
}

// TestMetrics_UsageRecordsTotal verifies the UsageRecordsTotal counter
// increments correctly.
func TestMetrics_UsageRecordsTotal(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewMetricsForTest(reg)

	for i := 0; i < 5; i++ {
		m.UsageRecordsTotal.Inc()
	}

	if v := counterValue(m.UsageRecordsTotal); v != 5 {
		t.Errorf("UsageRecordsTotal = %v, want 5", v)
	}
}

// TestMetrics_WorkerCallDuration verifies the WorkerCallDuration histogram
// records samples.
func TestMetrics_WorkerCallDuration(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewMetricsForTest(reg)

	m.WorkerCallDuration.Observe(0.05)
	m.WorkerCallDuration.Observe(0.15)
	m.WorkerCallDuration.Observe(1.5)

	ch := make(chan prometheus.Metric, 1)
	m.WorkerCallDuration.Collect(ch)
	metric := <-ch
	d := dto.Metric{}
	_ = metric.Write(&d)
	if d.GetHistogram().GetSampleCount() != 3 {
		t.Errorf("WorkerCallDuration sample count = %d, want 3", d.GetHistogram().GetSampleCount())
	}
}

// TestMetrics_RequestDuration_Labels verifies the Metrics.Middleware records
// duration with the correct path label from the chi route pattern.
func TestMetrics_RequestDuration_Labels(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewMetricsForTest(reg)

	r := chi.NewRouter()
	r.Use(m.Middleware)
	r.Post("/v1/run/{tool}", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusAccepted)
	})

	srv := httptest.NewServer(r)
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/v1/run/translate", "application/json", nil)
	if err != nil {
		t.Fatalf("POST failed: %v", err)
	}
	resp.Body.Close()

	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}

	var found bool
	for _, fam := range families {
		if fam.GetName() != "crucible_request_duration_seconds" {
			continue
		}
		for _, metric := range fam.GetMetric() {
			var method, path string
			for _, l := range metric.GetLabel() {
				switch l.GetName() {
				case "method":
					method = l.GetValue()
				case "path":
					path = l.GetValue()
				}
			}
			if method == "POST" && path == "/v1/run/{tool}" {
				if metric.GetHistogram().GetSampleCount() > 0 {
					found = true
				}
			}
		}
	}
	if !found {
		t.Error("no duration histogram sample found for POST /v1/run/{tool}")
	}
}

// TestMetrics_TwoRegistries verifies that two separate registries do not
// share state — incrementing a counter in one does not affect the other.
func TestMetrics_TwoRegistries(t *testing.T) {
	reg1 := prometheus.NewRegistry()
	reg2 := prometheus.NewRegistry()
	m1 := NewMetricsForTest(reg1)
	m2 := NewMetricsForTest(reg2)

	m1.RateLimitedTotal.Inc()
	m1.RateLimitedTotal.Inc()

	if v := counterValue(m1.RateLimitedTotal); v != 2 {
		t.Errorf("m1.RateLimitedTotal = %v, want 2", v)
	}
	if v := counterValue(m2.RateLimitedTotal); v != 0 {
		t.Errorf("m2.RateLimitedTotal = %v, want 0 (independent registry)", v)
	}
}

// TestMetrics_RequestsTotal_MultipleStatuses verifies that different HTTP
// status codes produce separate label combinations.
func TestMetrics_RequestsTotal_MultipleStatuses(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewMetricsForTest(reg)

	r := chi.NewRouter()
	r.Use(m.Middleware)
	r.Get("/check", func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Query().Get("fail") == "1" {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	})

	srv := httptest.NewServer(r)
	defer srv.Close()

	resp200, err := http.Get(srv.URL + "/check")
	if err != nil {
		t.Fatalf("GET 200 failed: %v", err)
	}
	resp200.Body.Close()

	resp500, err := http.Get(srv.URL + "/check?fail=1")
	if err != nil {
		t.Fatalf("GET 500 failed: %v", err)
	}
	resp500.Body.Close()

	if v := counterVecValue(m.RequestsTotal, "GET", "/check", "200"); v != 1 {
		t.Errorf("RequestsTotal{200} = %v, want 1", v)
	}
	if v := counterVecValue(m.RequestsTotal, "GET", "/check", "500"); v != 1 {
		t.Errorf("RequestsTotal{500} = %v, want 1", v)
	}
}
