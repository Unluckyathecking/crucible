// Package tracing provides OpenTelemetry HTTP middleware for the Crucible gateway.
// See provider.go for tracer provider construction.
package tracing

import (
	"fmt"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	oteltrace "go.opentelemetry.io/otel/trace"

	"github.com/Unluckyathecking/crucible/gateway/internal/httputil"
)

// init sets DefaultContextLogger when this package is imported without the
// middleware package (e.g. isolated tracing tests), preventing zerolog.Ctx
// from returning a zero-value Logger with a nil writer.
func init() {
	if zerolog.DefaultContextLogger == nil {
		zerolog.DefaultContextLogger = &log.Logger
	}
}

const tracerName = "crucible.gateway"

// w3cTraceparentMinLen is the minimum valid byte length of a W3C Trace Context
// traceparent header. Version 00 encodes as "00-<32hex>-<16hex>-<2hex>" = 55 bytes.
// Future spec versions may be longer; maxTraceparentLen inside Middleware handles that.
const w3cTraceparentMinLen = 55

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
// When tp is nil the middleware is a zero-overhead transparent pass-through (no
// allocations, no span context derived). Pass a noop.TracerProvider explicitly for
// the low-overhead noop-span path.
func Middleware(tp oteltrace.TracerProvider) func(http.Handler) http.Handler {
	if tp == nil {
		return func(next http.Handler) http.Handler { return next }
	}
	tracer := tp.Tracer(tracerName)

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Read the base logger from the original request context before deriving
			// new contexts, so any logger stored by upstream middleware (e.g. RequestID)
			// is preserved. middleware.init() sets zerolog.DefaultContextLogger so Ctx
			// never returns the Nop sentinel when the full gateway stack is loaded.
			// Work with a value (not pointer) to avoid zerolog pointer aliasing.
			base := *zerolog.Ctx(r.Context())

			// Extract parent span from inbound W3C traceparent header.
			// Reject strings shorter than w3cTraceparentMinLen (55) — they can never be
			// valid and passing them to the propagator wastes parse work. The upper bound
			// (512) is a defense-in-depth limit to prevent header-stuffing DoS; W3C does
			// not specify a maximum traceparent length, but future spec versions are
			// unlikely to exceed this. propagator.Extract is a no-op when the header is
			// absent; both paths produce a fresh root span for absent or malformed input.
			const maxTraceparentLen = 512
			ctx := r.Context()
			if tv := r.Header.Get("traceparent"); len(tv) >= w3cTraceparentMinLen && len(tv) <= maxTraceparentLen {
				ctx = propagator.Extract(ctx, propagation.HeaderCarrier(r.Header))
			}

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
			}
			// Log enrichment is conditional on sc.IsValid(). When tp is a noop
			// provider, spans have invalid span contexts and the block above is
			// skipped; zerolog.DefaultContextLogger (set in middleware/middleware.go
			// init()) is the fallback for any zerolog.Ctx(ctx) call.

			// Reassign r so chi.RouteContext picks up the same context that has the span.
			r = r.WithContext(ctx)

			// Wrap the response writer to capture the HTTP status code for span annotation.
			ww := httputil.NewStatusRecorder(w)

			// span, r, and ww are all declared within this http.HandlerFunc invocation —
			// each concurrent request gets its own independent copies allocated on each
			// call. The defer below closes over only this invocation's span; there is no
			// shared span state across requests. All mutations and span.End() live in this
			// single closure so their ordering is unambiguous.
			defer func() {
				span.SetAttributes(
					attribute.String("http.method", r.Method),
					attribute.String("http.path", r.URL.Path),
					attribute.Int("http.status_code", ww.Status),
				)

				// Only server errors (5xx) indicate a gateway failure; 4xx are
				// client errors the server handled correctly per OTel conventions.
				if ww.Status >= 500 {
					span.SetStatus(codes.Error, fmt.Sprintf("HTTP %d", ww.Status))
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
