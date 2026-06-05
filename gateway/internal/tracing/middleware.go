package tracing

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	oteltrace "go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"

	"github.com/Unluckyathecking/crucible/gateway/internal/httputil"
)

const tracerName = "crucible.gateway"

// propagator is the W3C TraceContext propagator used for header extraction/injection.
var propagator = propagation.TraceContext{}

// Middleware returns HTTP middleware that:
//  1. Extracts an inbound W3C traceparent header to continue a remote parent trace,
//     or starts a fresh root span when the header is absent.
//  2. Injects the active trace_id and span_id into the zerolog context so any
//     handler that calls zerolog.Ctx(ctx) carries them on every log event.
//  3. Renames the span to the matched chi route pattern after the handler returns
//     (the pattern is not resolved until the router has dispatched the request).
//  4. Records span status as Error for HTTP 5xx responses.
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
			// Deferred so the span is ended even if a panic escapes a middleware layer.
			// Annotations (attributes, status, name) execute inline before the return,
			// so they are always recorded before End fires.
			defer span.End()

			// Determine the base logger. zerolog.Ctx returns a disabled logger pointer
			// when no prior middleware has stored a logger in the context. Guard against
			// nil (paranoid: zerolog never returns nil, but the nil check costs nothing)
			// and against the disabled fallback — both indicate "no logger yet in context."
			// Use r.Context() for the logger lookup so that any logger already
			// stored by upstream middleware (e.g. RequestID) is preserved across
			// the propagator.Extract / tracer.Start context derivations.
			base := zerolog.Ctx(r.Context())
			if base == nil || base.GetLevel() == zerolog.Disabled {
				l := log.Logger
				base = &l
			}

			sc := span.SpanContext()
			if sc.IsValid() {
				// Extend the base logger with trace/span IDs.
				l := base.With().
					Str("trace_id", sc.TraceID().String()).
					Str("span_id", sc.SpanID().String()).
					Logger()
				ctx = l.WithContext(ctx)
			} else {
				// Noop provider — no span IDs to add, but still inject the logger so
				// AccessLog and handlers can call zerolog.Ctx(ctx) safely.
				ctx = base.WithContext(ctx)
			}

			// Reassign r so chi.RouteContext picks up the same context that has the span.
			r = r.WithContext(ctx)

			// Wrap the response writer to capture the HTTP status code for span annotation.
			ww := httputil.NewStatusRecorder(w)
			next.ServeHTTP(ww, r)

			// Record HTTP semantic attributes after the handler returns.
			span.SetAttributes(
				attribute.String("http.method", r.Method),
				attribute.String("http.path", r.URL.Path),
				attribute.Int("http.status_code", ww.Status),
			)

			// Only server errors (5xx) indicate a gateway failure; 4xx are
			// client errors the server handled correctly per OTel conventions.
			if ww.Status >= 500 {
				span.SetStatus(codes.Error, http.StatusText(ww.Status))
			}

			// Rename span and record http.route after routing has resolved.
			// "gateway.request" is only the initial placeholder — chi populates
			// RoutePattern during ServeHTTP, so it's available here but not at span start.
			if rctx := chi.RouteContext(r.Context()); rctx != nil && rctx.RoutePattern() != "" {
				span.SetAttributes(attribute.String("http.route", rctx.RoutePattern()))
				span.SetName(rctx.RoutePattern())
			}
		})
	}
}
