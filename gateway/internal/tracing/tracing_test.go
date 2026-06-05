package tracing_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rs/zerolog"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	oteltrace "go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"

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
	t.Cleanup(func() { tp.Shutdown(context.Background()) })
	return tp, sr
}

// findSpan returns the first ended span with the given name, or (nil, false).
func findSpan(spans []sdktrace.ReadOnlySpan, name string) (sdktrace.ReadOnlySpan, bool) {
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
	parentCtx, parentSpan := tp.Tracer("test").Start(context.Background(), "parent")
	parentSpan.End()
	parentSC := parentSpan.SpanContext()

	inboundHeaders := make(http.Header)
	propagation.TraceContext{}.Inject(parentCtx, propagation.HeaderCarrier(inboundHeaders))

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.Header = inboundHeaders
	rec := httptest.NewRecorder()

	tracing.Middleware(tp)(okHandler).ServeHTTP(rec, req)

	gwSpan, ok := findSpan(sr.Ended(), "gateway.request")
	if !ok {
		t.Fatal("no gateway.request span recorded")
	}
	if got := gwSpan.SpanContext().TraceID(); got != parentSC.TraceID() {
		t.Errorf("trace ID = %s, want %s (should continue parent trace)", got, parentSC.TraceID())
	}
	if !gwSpan.Parent().SpanID().IsValid() {
		t.Error("gateway span must have a valid parent when inbound traceparent is present")
	}
}

// TestAbsentTraceparentStartsRootSpan verifies that a request without a traceparent
// header starts a fresh root span with a valid trace ID and no parent.
func TestAbsentTraceparentStartsRootSpan(t *testing.T) {
	tp, sr := newTestProvider(t)

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	rec := httptest.NewRecorder()

	tracing.Middleware(tp)(okHandler).ServeHTTP(rec, req)

	gwSpan, ok := findSpan(sr.Ended(), "gateway.request")
	if !ok {
		t.Fatal("no gateway.request span recorded")
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

	if tp := outboundHeaders.Get("Traceparent"); tp != "" {
		t.Errorf("expected no Traceparent header for noop provider, got %q", tp)
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

	var buf strings.Builder
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
}
