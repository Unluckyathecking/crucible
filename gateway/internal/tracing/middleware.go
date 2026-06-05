// Package tracing provides OpenTelemetry HTTP middleware and tracer provider
// construction for the Crucible gateway.
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
			// Read the base logger from the original request context before deriving
			// new contexts, so any logger stored by upstream middleware (e.g. RequestID)
			// is preserved. zerolog.Ctx returns a disabled sentinel (never nil) when no
			// logger is stored; fall back to the global logger in that case.
			base := zerolog.Ctx(r.Context())
			if base.GetLevel() == zerolog.Disabled {
				base = &log.Logger
			}

			// Extract parent span from inbound W3C traceparent header (no-op if absent).
			// Reject oversized traceparent headers without mutating the original request:
			// the W3C spec fixes the format at 55 chars; 256 is generous for future versions.
			extractHeaders := r.Header
			if traceparentVal := r.Header.Get("Traceparent"); len(traceparentVal) > 256 {
				extractHeaders = r.Header.Clone()
				extractHeaders.Del("Traceparent")
			}
			ctx := propagator.Extract(r.Context(), propagation.HeaderCarrier(extractHeaders))

			// Start gateway.request span — child of remote parent or new root span.
			ctx, span := tracer.Start(ctx, "gateway.request")

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

			// A single deferred closure records all span attributes, updates status,
			// resolves the span name from the chi route pattern, and ends the span.
			// All mutations and span.End() are in the same closure so their ordering
			// is unambiguous.
			defer func() {
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
				// When chi is active but no route matched (404), or when the middleware is
				// used outside chi, use "gateway.unmatched" so every span carries http.route
				// and dashboards can group uniformly.
				routePattern := ""
				if rctx := chi.RouteContext(r.Context()); rctx != nil {
					routePattern = rctx.RoutePattern()
				}
				if routePattern != "" {
					span.SetAttributes(attribute.String("http.route", routePattern))
					span.SetName(routePattern)
				} else {
					span.SetAttributes(attribute.String("http.route", "gateway.unmatched"))
					span.SetName("gateway.unmatched")
				}

				span.End()
			}()

			next.ServeHTTP(ww, r)
		})
	}
}
