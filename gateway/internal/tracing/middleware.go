package tracing

import (
	"net/http"

	"github.com/rs/zerolog/log"
	"go.opentelemetry.io/otel/propagation"
	oteltrace "go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
)

const tracerName = "crucible.gateway"

// propagator is the W3C TraceContext propagator used for header extraction/injection.
var propagator = propagation.TraceContext{}

// Middleware returns HTTP middleware that:
//  1. Extracts an inbound W3C traceparent header to continue a remote parent trace,
//     or starts a fresh root span when the header is absent.
//  2. Injects the active trace_id and span_id into the zerolog context so any
//     handler that calls zerolog.Ctx(ctx) carries them on every log event.
//
// When tp is nil or a noop.TracerProvider (default-off state), spans have invalid
// span contexts, no exporter is dialed, and the middleware is a transparent pass-through.
func Middleware(tp oteltrace.TracerProvider) func(http.Handler) http.Handler {
	if tp == nil {
		tp = noop.NewTracerProvider()
	}
	tracer := tp.Tracer(tracerName)

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Extract parent span from inbound W3C traceparent header (no-op if absent).
			ctx := propagator.Extract(r.Context(), propagation.HeaderCarrier(r.Header))

			// Start gateway.request span — child of remote parent or new root span.
			ctx, span := tracer.Start(ctx, "gateway.request")
			defer span.End()

			// Attach trace/span IDs to zerolog context. Using log.Logger as the base
			// ensures test overrides (log.Logger = log.Output(&buf)) are honoured here.
			sc := span.SpanContext()
			if sc.IsValid() {
				logger := log.Logger.With().
					Str("trace_id", sc.TraceID().String()).
					Str("span_id", sc.SpanID().String()).
					Logger()
				ctx = logger.WithContext(ctx)
			}

			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}
