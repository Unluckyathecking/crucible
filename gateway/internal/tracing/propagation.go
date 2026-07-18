// Package tracing (see provider.go, middleware.go) also provides the
// capture-at-enqueue / restore-at-execute primitive used by the framework's
// async outbox subsystems (jobs.Store/Executor, webhookout.Emitter) to carry
// a W3C trace context across the durable-row boundary that Middleware's
// synchronous HTTP propagation never crosses.
package tracing

import (
	"context"

	"go.opentelemetry.io/otel/propagation"
)

// CaptureTraceparent serializes the W3C traceparent of the span active in
// ctx, for persisting alongside a durable outbox row (async_jobs,
// webhook_deliveries) at enqueue time. Returns "" when ctx carries no valid
// span — tracing disabled (Middleware(nil) never starts a span) or the call
// happened outside any traced request — so callers persist NULL rather than
// a meaningless empty string, and RestoreTraceparent's later no-op on ""
// is exact, not approximate.
func CaptureTraceparent(ctx context.Context) string {
	carrier := propagation.MapCarrier{}
	propagator.Inject(ctx, carrier)
	return carrier.Get("traceparent")
}

// RestoreTraceparent parses a W3C traceparent captured by CaptureTraceparent
// and returns a context carrying it as the remote parent span context — the
// execution-time counterpart used by jobs.Executor and webhookout.Emitter's
// delivery loop to continue the trace that started at enqueue, so the span
// they start nests under the original request instead of orphaning. An
// empty or malformed traceparent is a no-op: ctx is returned unchanged
// rather than panicking or fabricating an invalid parent, mirroring
// Middleware's tolerance of a malformed inbound header.
func RestoreTraceparent(ctx context.Context, traceparent string) context.Context {
	if traceparent == "" {
		return ctx
	}
	carrier := propagation.MapCarrier{"traceparent": traceparent}
	return propagator.Extract(ctx, carrier)
}
