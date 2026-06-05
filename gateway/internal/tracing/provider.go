// Package tracing provides OpenTelemetry tracing primitives for the Crucible gateway.
//
// Default-disabled: the zero config (OTEL_TRACING_ENABLED=false / TracerProvider nil in
// server.Deps) uses a noop.TracerProvider that dials no exporter and adds no overhead.
// Call NewProvider to construct a live OTLP-exporting provider, then pass the result to
// server.Deps.TracerProvider.
package tracing

import (
	"context"
	"fmt"

	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// NewProvider constructs a TracerProvider that exports spans via OTLP/HTTP to endpoint.
// sampleRatio must be in [0.0, 1.0]; 1.0 samples every trace, 0.0 samples none.
// The returned shutdown function flushes pending spans; call it at process exit.
func NewProvider(ctx context.Context, endpoint string, sampleRatio float64) (*sdktrace.TracerProvider, func(context.Context) error, error) {
	exp, err := otlptracehttp.New(ctx,
		otlptracehttp.WithEndpoint(endpoint),
		otlptracehttp.WithInsecure(),
	)
	if err != nil {
		return nil, nil, fmt.Errorf("tracing: create OTLP exporter: %w", err)
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exp),
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.TraceIDRatioBased(sampleRatio))),
	)
	return tp, tp.Shutdown, nil
}
