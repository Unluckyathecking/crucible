package observability

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

func counterValue(c prometheus.Collector) float64 {
	ch := make(chan prometheus.Metric, 1)
	c.Collect(ch)
	m := <-ch
	metric := dto.Metric{}
	_ = m.Write(&metric)
	return metric.GetCounter().GetValue()
}

func TestNewMetricsForTest_NoCollision(t *testing.T) {
	reg := prometheus.NewRegistry()
	m1 := NewMetricsForTest(reg)

	m1.RateLimitedTotal.Inc()
	m1.RateLimitedTotal.Inc()
	m1.RateLimitedTotal.Inc()

	if v := counterValue(m1.RateLimitedTotal); v != 3 {
		t.Errorf("RateLimitedTotal = %v, want 3", v)
	}
}

func TestMetrics_RateLimitedTotal(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewMetricsForTest(reg)

	m.RateLimitedTotal.Inc()
	if v := counterValue(m.RateLimitedTotal); v != 1 {
		t.Errorf("after one Inc, got %v, want 1", v)
	}

	m.RateLimitedTotal.Inc()
	m.RateLimitedTotal.Inc()
	if v := counterValue(m.RateLimitedTotal); v != 3 {
		t.Errorf("after three Incs, got %v, want 3", v)
	}
}

func TestMetrics_RequestsTotal(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewMetricsForTest(reg)

	r := chi.NewRouter()
	r.Use(m.Middleware)
	r.Get("/hello/{name}", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	srv := httptest.NewServer(r)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/hello/world")
	if err != nil {
		t.Fatalf("GET failed: %v", err)
	}
	resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}

	var total float64
	for _, fam := range families {
		if fam.GetName() == "crucible_requests_total" {
			for _, metric := range fam.GetMetric() {
				labels := metric.GetLabel()
				method := ""
				path := ""
				status := ""
				for _, l := range labels {
					switch l.GetName() {
					case "method":
						method = l.GetValue()
					case "path":
						path = l.GetValue()
					case "status":
						status = l.GetValue()
					}
				}
				if method == "GET" && path == "/hello/{name}" && status == "200" {
					total += metric.GetCounter().GetValue()
				}
			}
		}
	}
	if total != 1 {
		t.Errorf("requests total for GET /hello/{name} 200 = %v, want 1", total)
	}
}

func TestMetrics_Middleware_Unmatched(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewMetricsForTest(reg)

	r := chi.NewRouter()
	r.Use(m.Middleware)
	r.Get("/only-route", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	srv := httptest.NewServer(r)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/nonexistent")
	if err != nil {
		t.Fatalf("GET failed: %v", err)
	}
	resp.Body.Close()

	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}

	var found bool
	for _, fam := range families {
		if fam.GetName() != "crucible_requests_total" {
			continue
		}
		for _, metric := range fam.GetMetric() {
			for _, l := range metric.GetLabel() {
				if l.GetName() == "path" && l.GetValue() == "unmatched" {
					found = true
				}
			}
		}
	}
	if !found {
		t.Error("expected 'unmatched' path label for unknown route")
	}
}

func TestMetrics_RequestDuration_Recorded(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewMetricsForTest(reg)

	r := chi.NewRouter()
	r.Use(m.Middleware)
	r.Get("/ping", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})

	srv := httptest.NewServer(r)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/ping")
	if err != nil {
		t.Fatalf("GET failed: %v", err)
	}
	resp.Body.Close()

	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}

	var durationObserved bool
	for _, fam := range families {
		if fam.GetName() != "crucible_request_duration_seconds" {
			continue
		}
		for _, metric := range fam.GetMetric() {
			if metric.GetHistogram().GetSampleCount() > 0 {
				durationObserved = true
				t.Logf("histogram samples: %d, sum: %f",
					metric.GetHistogram().GetSampleCount(),
					metric.GetHistogram().GetSampleSum())
			}
		}
	}
	if !durationObserved {
		t.Error("request_duration histogram has no samples")
	}
}
