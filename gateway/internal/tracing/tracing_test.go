package tracing_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/rs/zerolog"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	oteltrace "go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"

	"github.com/Unluckyathecking/crucible/gateway/internal/middleware"
	"github.com/Unluckyathecking/crucible/gateway/internal/tracing"
)

var okHandler = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
})

// newTestProvider builds a TracerProvider backed by a SpanRecorder for test assertions.
func newTestProvider(t *testing.T) (*sdktrace.TracerProvider, *tracetest.SpanRecorder) {
	t.Helper()
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := tp.Shutdown(ctx); err != nil {
			t.Errorf("tracer provider shutdown failed: %v", err)
		}
	})
	return tp, sr
}

// findSpan returns the first ended span with the given name, or (nil, false).
func findSpan(t *testing.T, spans []sdktrace.ReadOnlySpan, name string) (sdktrace.ReadOnlySpan, bool) {
	t.Helper()
	for _, s := range spans {
		if s.Name() == name {
			return s, true
		}
	}
	return nil, false
}

// TestInboundTraceparentContinuesTrace verifies that a valid W3C traceparent on the
// inbound request causes the gateway span to join the same remote trace (matching
// trace ID, with a valid parent reference).
func TestInboundTraceparentContinuesTrace(t *testing.T) {
	tp, sr := newTestProvider(t)

	// Create a parent span and encode it as a W3C traceparent header.
	// Keep the span active during injection so the span context is valid.
	parentCtx, parentSpan := tp.Tracer("test").Start(context.Background(), "parent")
	parentSC := parentSpan.SpanContext()

	inboundHeaders := make(http.Header)
	propagation.TraceContext{}.Inject(parentCtx, propagation.HeaderCarrier(inboundHeaders))
	parentSpan.End()

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	for k, v := range inboundHeaders {
		req.Header[k] = v
	}
	rec := httptest.NewRecorder()

	tracing.Middleware(tp)(okHandler).ServeHTTP(rec, req)

	// Without a chi router the span is renamed to gateway.unmatched (http.route always set).
	gwSpan, ok := findSpan(t, sr.Ended(), "gateway.unmatched")
	if !ok {
		t.Fatal("no gateway.unmatched span recorded")
	}
	if got := gwSpan.SpanContext().TraceID(); got != parentSC.TraceID() {
		t.Errorf("trace ID = %s, want %s (should continue parent trace)", got, parentSC.TraceID())
	}
	if !gwSpan.Parent().SpanID().IsValid() {
		t.Error("gateway span must have a valid parent when inbound traceparent is present")
	}
	if gwSpan.Parent().SpanID() != parentSC.SpanID() {
		t.Errorf("parent span ID = %s, want %s (must point to exact parent span)", gwSpan.Parent().SpanID(), parentSC.SpanID())
	}
}

// TestAbsentTraceparentStartsRootSpan verifies that a request without a traceparent
// header starts a fresh root span with a valid trace ID and no parent.
func TestAbsentTraceparentStartsRootSpan(t *testing.T) {
	tp, sr := newTestProvider(t)

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	rec := httptest.NewRecorder()

	tracing.Middleware(tp)(okHandler).ServeHTTP(rec, req)

	// Without a chi router the span is renamed to gateway.unmatched (http.route always set).
	gwSpan, ok := findSpan(t, sr.Ended(), "gateway.unmatched")
	if !ok {
		t.Fatal("no gateway.unmatched span recorded")
	}
	if !gwSpan.SpanContext().IsValid() {
		t.Error("gateway span must have a valid span context")
	}
	if gwSpan.Parent().SpanID().IsValid() {
		t.Errorf("gateway span must be a root span (no parent) when no traceparent header is present; got parent %s", gwSpan.Parent().SpanID())
	}
}

// TestPropagatorWritesOutboundTraceparent verifies that a well-formed W3C traceparent
// header is written when the propagator injects into an outbound request header.
func TestPropagatorWritesOutboundTraceparent(t *testing.T) {
	tp, _ := newTestProvider(t)

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	rec := httptest.NewRecorder()

	var outboundHeaders http.Header
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		outboundHeaders = make(http.Header)
		propagation.TraceContext{}.Inject(r.Context(), propagation.HeaderCarrier(outboundHeaders))
		w.WriteHeader(http.StatusOK)
	})

	tracing.Middleware(tp)(handler).ServeHTTP(rec, req)

	traceparent := outboundHeaders.Get("Traceparent")
	if traceparent == "" {
		t.Fatal("expected Traceparent header in outbound request")
	}
	// Well-formed W3C traceparent: "00-<32 hex>-<16 hex>-<2 hex>"
	parts := strings.Split(traceparent, "-")
	if len(parts) != 4 {
		t.Fatalf("malformed traceparent %q: got %d dash-separated parts, want 4", traceparent, len(parts))
	}
	if parts[0] != "00" {
		t.Errorf("traceparent version = %q, want 00", parts[0])
	}
	if len(parts[1]) != 32 {
		t.Errorf("traceparent trace ID = %q (%d chars), want 32 hex chars", parts[1], len(parts[1]))
	}
	if len(parts[2]) != 16 {
		t.Errorf("traceparent span ID = %q (%d chars), want 16 hex chars", parts[2], len(parts[2]))
	}
	if len(parts[3]) != 2 {
		t.Errorf("traceparent flags = %q (%d chars), want 2 hex chars", parts[3], len(parts[3]))
	}
	// Verify trace ID and span ID are not all-zero (would indicate a noop span context).
	if parts[1] == strings.Repeat("0", 32) {
		t.Error("traceparent trace ID is all zeros — span context is not valid")
	}
	if parts[2] == strings.Repeat("0", 16) {
		t.Error("traceparent span ID is all zeros — span context is not valid")
	}
}

// TestNoOpWhenDisabled verifies that a noop.TracerProvider (zero-config / default-off)
// produces no Traceparent header and the span context in the request context is invalid.
func TestNoOpWhenDisabled(t *testing.T) {
	tp := noop.NewTracerProvider()

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	rec := httptest.NewRecorder()

	var outboundHeaders http.Header
	var capturedCtx context.Context
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedCtx = r.Context()
		outboundHeaders = make(http.Header)
		propagation.TraceContext{}.Inject(r.Context(), propagation.HeaderCarrier(outboundHeaders))
		w.WriteHeader(http.StatusOK)
	})

	tracing.Middleware(tp)(handler).ServeHTTP(rec, req)

	if traceparent := outboundHeaders.Get("Traceparent"); traceparent != "" {
		t.Errorf("expected no Traceparent header for noop provider, got %q", traceparent)
	}

	// Confirm the span in context is invalid (noop).
	sc := oteltrace.SpanFromContext(capturedCtx).SpanContext()
	if sc.IsValid() {
		t.Errorf("expected invalid span context for noop provider, got %+v", sc)
	}
}

// TestLogLinesCarryTraceID verifies that handlers calling zerolog.Ctx(ctx) emit log
// events that include the trace_id field when a real TracerProvider is active.
// The test logger is injected via the request context so no global state is mutated.
func TestLogLinesCarryTraceID(t *testing.T) {
	tp, _ := newTestProvider(t)

	var buf bytes.Buffer
	testLogger := zerolog.New(&buf)

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	// Inject the test logger so the tracing middleware picks it up via zerolog.Ctx.
	req = req.WithContext(testLogger.WithContext(req.Context()))
	rec := httptest.NewRecorder()

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		zerolog.Ctx(r.Context()).Info().Msg("handler-log")
		w.WriteHeader(http.StatusOK)
	})

	tracing.Middleware(tp)(handler).ServeHTTP(rec, req)

	output := buf.String()
	if !strings.Contains(output, `"trace_id"`) {
		t.Errorf("expected trace_id field in log output, got:\n%s", output)
	}
	if !strings.Contains(output, `"span_id"`) {
		t.Errorf("expected span_id field in log output, got:\n%s", output)
	}
	if !strings.Contains(output, "handler-log") {
		t.Errorf("expected handler-log message in log output, got:\n%s", output)
	}

	// Parse the JSON log line for structured field validation.
	var logLine map[string]interface{}
	if err := json.Unmarshal([]byte(strings.TrimSpace(output)), &logLine); err != nil {
		t.Fatalf("log output is not valid JSON: %v\noutput: %s", err, output)
	}
	if tid, ok := logLine["trace_id"].(string); ok {
		if len(tid) != 32 {
			t.Errorf("trace_id = %q (%d chars), want 32 hex chars", tid, len(tid))
		}
		if tid == strings.Repeat("0", 32) {
			t.Error("trace_id is all zeros — span context is not valid")
		}
	}
	if sid, ok := logLine["span_id"].(string); ok {
		if len(sid) != 16 {
			t.Errorf("span_id = %q (%d chars), want 16 hex chars", sid, len(sid))
		}
		if sid == strings.Repeat("0", 16) {
			t.Error("span_id is all zeros — span context is not valid")
		}
	}
}

// TestSpanStatusErrorOn5xx verifies that a handler returning HTTP 500 causes the
// gateway span to be marked with codes.Error.
func TestSpanStatusErrorOn5xx(t *testing.T) {
	tp, sr := newTestProvider(t)

	errHandler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	rec := httptest.NewRecorder()

	tracing.Middleware(tp)(errHandler).ServeHTTP(rec, req)

	// Without a chi router the span is renamed to gateway.unmatched (http.route always set).
	gwSpan, ok := findSpan(t, sr.Ended(), "gateway.unmatched")
	if !ok {
		t.Fatal("no gateway.unmatched span recorded")
	}
	if got := gwSpan.Status().Code; got != codes.Error {
		t.Errorf("span status code = %v, want codes.Error for HTTP 500", got)
	}
}

// TestSpanNamedByChiRoutePattern verifies that after a chi router dispatches the
// request, the gateway span is renamed to the matched route pattern.
func TestSpanNamedByChiRoutePattern(t *testing.T) {
	tp, sr := newTestProvider(t)

	r := chi.NewRouter()
	r.Use(tracing.Middleware(tp))
	r.Get("/items/{id}", okHandler)

	req := httptest.NewRequest(http.MethodGet, "/items/42", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	_, plain := findSpan(t, sr.Ended(), "gateway.request")
	namedSpan, named := findSpan(t, sr.Ended(), "/items/{id}")
	if plain {
		t.Error("span was not renamed from gateway.request to the chi route pattern")
	}
	if !named {
		t.Error("expected span named /items/{id} after chi routing, not found")
	}
	if named {
		var httpRoute string
		for _, a := range namedSpan.Attributes() {
			if string(a.Key) == "http.route" {
				httpRoute = a.Value.AsString()
				break
			}
		}
		if httpRoute != "/items/{id}" {
			t.Errorf("http.route attribute = %q, want /items/{id}", httpRoute)
		}
	}
}

// TestSpanNamedGatewayUnmatchedForUnknownRoute verifies that when a chi router is
// active but no route matches the request (404), the gateway span is renamed to
// "gateway.unmatched" rather than keeping the "gateway.request" placeholder.
func TestSpanNamedGatewayUnmatchedForUnknownRoute(t *testing.T) {
	tp, sr := newTestProvider(t)

	r := chi.NewRouter()
	r.Use(tracing.Middleware(tp))
	r.Get("/items/{id}", okHandler)

	req := httptest.NewRequest(http.MethodGet, "/no-such-route", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 from chi, got %d", rec.Code)
	}
	_, plain := findSpan(t, sr.Ended(), "gateway.request")
	if plain {
		t.Error("span was not renamed from gateway.request for unmatched chi route")
	}
	unmatchedSpan, unmatched := findSpan(t, sr.Ended(), "gateway.unmatched")
	if !unmatched {
		t.Error("expected span named gateway.unmatched for unmatched chi route, not found")
	}
	if unmatched {
		var httpRoute string
		for _, a := range unmatchedSpan.Attributes() {
			if string(a.Key) == "http.route" {
				httpRoute = a.Value.AsString()
				break
			}
		}
		if httpRoute != "gateway.unmatched" {
			t.Errorf("http.route attribute = %q, want gateway.unmatched", httpRoute)
		}
	}
}

// TestNoOpWhenDisabledWithNilProvider verifies the Middleware(nil) code path —
// the production default when TracerProvider is not wired — produces no Traceparent.
func TestNoOpWhenDisabledWithNilProvider(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	rec := httptest.NewRecorder()

	var outboundHeaders http.Header
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		outboundHeaders = make(http.Header)
		propagation.TraceContext{}.Inject(r.Context(), propagation.HeaderCarrier(outboundHeaders))
		w.WriteHeader(http.StatusOK)
	})

	tracing.Middleware(nil)(handler).ServeHTTP(rec, req)

	if tp := outboundHeaders.Get("Traceparent"); tp != "" {
		t.Errorf("expected no Traceparent header for nil provider, got %q", tp)
	}
}

// TestFullMiddlewareStackLogCarriesRequestAndTraceIDs verifies that when RequestID,
// tracing.Middleware, and AccessLog are stacked together (as in the production route),
// the access log line carries both request_id (from RequestID middleware) and
// trace_id/span_id (injected by tracing middleware). This covers the integration
// between tracing's context-logger enrichment and AccessLog's consumption of it.
func TestFullMiddlewareStackLogCarriesRequestAndTraceIDs(t *testing.T) {
	tp, _ := newTestProvider(t)

	var buf strings.Builder
	testLogger := zerolog.New(&buf)

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	// Inject test logger so the tracing middleware enriches it with trace_id/span_id.
	req = req.WithContext(testLogger.WithContext(req.Context()))
	rec := httptest.NewRecorder()

	// Stack mirrors the production chain: RequestID → tracing → AccessLog → handler.
	stack := middleware.RequestID(tracing.Middleware(tp)(middleware.AccessLog(okHandler)))
	stack.ServeHTTP(rec, req)

	output := buf.String()
	if !strings.Contains(output, `"request_id"`) {
		t.Errorf("expected request_id field in access log, got:\n%s", output)
	}
	if !strings.Contains(output, `"trace_id"`) {
		t.Errorf("expected trace_id field in access log, got:\n%s", output)
	}
	if !strings.Contains(output, `"span_id"`) {
		t.Errorf("expected span_id field in access log, got:\n%s", output)
	}
}

// TestConcurrentRequestsGetDistinctTraceIDs verifies that concurrent requests through
// the same middleware instance each receive a unique trace ID and do not share context.
func TestConcurrentRequestsGetDistinctTraceIDs(t *testing.T) {
	tp, sr := newTestProvider(t)

	const n = 20
	var wg sync.WaitGroup
	traceIDs := make([]string, n)
	statusCodes := make([]int, n)

	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			req := httptest.NewRequest(http.MethodGet, "/test", nil)
			rec := httptest.NewRecorder()
			handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				traceIDs[idx] = oteltrace.SpanFromContext(r.Context()).SpanContext().TraceID().String()
				w.WriteHeader(http.StatusOK)
			})
			tracing.Middleware(tp)(handler).ServeHTTP(rec, req)
			statusCodes[idx] = rec.Code
		}(i)
	}
	wg.Wait()

	// All assertions run after wg.Wait() so t.Errorf is called from the test goroutine only.
	for i, code := range statusCodes {
		if code != http.StatusOK {
			t.Errorf("goroutine %d: unexpected status %d", i, code)
		}
	}

	if got := len(sr.Ended()); got != n {
		t.Errorf("ended span count = %d, want %d (each goroutine must produce exactly one span)", got, n)
	}

	// Build a set of trace IDs from the recorded spans for cross-reference.
	recordedIDs := make(map[string]bool, n)
	for _, s := range sr.Ended() {
		recordedIDs[s.SpanContext().TraceID().String()] = true
	}

	seen := make(map[string]bool)
	for i, id := range traceIDs {
		if id == "" || id == strings.Repeat("0", 32) {
			t.Errorf("goroutine %d: got empty or zero trace ID", i)
			continue
		}
		if seen[id] {
			t.Errorf("goroutine %d: duplicate trace ID %s", i, id)
		}
		seen[id] = true
		// Verify the trace ID seen inside the handler matches a recorded span.
		if !recordedIDs[id] {
			t.Errorf("goroutine %d: trace ID %s not found in recorded spans (context/recorder mismatch)", i, id)
		}
	}
}
