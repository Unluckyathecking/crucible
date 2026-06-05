// Package tracing provides OpenTelemetry tracing primitives for the Crucible gateway.
//
// Two usage patterns:
//   - Default-off: pass nil TracerProvider to Middleware — zero overhead, no exporter dialed.
//   - Live traces: call NewProvider with an OTLP endpoint, pass the returned TracerProvider
//     to Middleware and server.Deps, and call the returned shutdown function at process exit.
package tracing

import (
	"context"
	"fmt"
	"net"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

const (
	// exporterCreationTimeout caps how long NewProvider blocks while opening the OTLP
	// connection during startup. A slow or unreachable collector must not stall boot.
	// Uses context.Background so a short-lived caller context cannot shorten the window.
	exporterCreationTimeout = 10 * time.Second

	// batchExportTimeout is the per-flush deadline passed to the OTLP exporter by the
	// BatchSpanProcessor. Set longer than batchFlushInterval so a full batch always
	// has time to drain before the flush interval fires again.
	batchExportTimeout = 10 * time.Second

	// batchFlushInterval is the maximum time a span sits in the BatchSpanProcessor
	// queue before the processor forces a flush. Increasing this value reduces HTTP
	// traffic at the cost of higher trace delivery latency.
	batchFlushInterval = 5 * time.Second
)

// NewProvider constructs a TracerProvider that exports spans via OTLP/HTTP to endpoint.
// endpoint must be in host:port format without a scheme, e.g. "localhost:4318" or
// "otel-collector.internal:4318" — matching the format validated by config.Load via
// OTEL_EXPORTER_ENDPOINT. insecure disables TLS; set true for localhost/sidecar
// collectors that do not serve TLS (mirrors config.OtelExporterInsecure).
// sampleRatio must be in [0.0, 1.0]; 1.0 samples every trace, 0.0 samples none.
// The returned shutdown function flushes pending spans; call it at process exit with
// a context whose deadline exceeds batchExportTimeout so in-flight exports complete.
//
// Note: exporter creation uses context.Background internally (not a caller-supplied
// context) so a short-lived caller context cannot abort startup. The exporterCreationTimeout
// constant is the startup bound.
func NewProvider(endpoint string, insecure bool, sampleRatio float64) (*sdktrace.TracerProvider, func(context.Context) error, error) {
	if endpoint == "" {
		return nil, nil, fmt.Errorf("tracing: endpoint cannot be empty")
	}
	host, _, err := net.SplitHostPort(endpoint)
	if err != nil {
		return nil, nil, fmt.Errorf("tracing: endpoint %q must be host:port (e.g. localhost:4318): %w", endpoint, err)
	}
	if host == "" {
		return nil, nil, fmt.Errorf("tracing: endpoint %q must have a non-empty host", endpoint)
	}

	// Build the resource first so that a merge error never leaks an already-opened exporter.
	// Re-use the default resource's schema URL so the merge result has a consistent schema
	// and downstream collectors can resolve attribute semantics correctly.
	// resource.Default() returns a non-nil resource in all known SDK versions, but guard
	// defensively so a future SDK change or stripped build tag can't produce a nil dereference.
	defaultRes := resource.Default()
	if defaultRes == nil {
		defaultRes = resource.Empty()
	}
	res, mergeErr := resource.Merge(
		defaultRes,
		resource.NewWithAttributes(defaultRes.SchemaURL(), attribute.String("service.name", "crucible-gateway")),
	)
	if mergeErr != nil {
		return nil, nil, fmt.Errorf("tracing: merge resource: %w", mergeErr)
	}

	// WithEndpoint takes host:port (no scheme). WithInsecure() skips TLS for
	// localhost or in-cluster collectors that don't serve TLS.
	opts := []otlptracehttp.Option{otlptracehttp.WithEndpoint(endpoint)}
	if insecure {
		opts = append(opts, otlptracehttp.WithInsecure())
	}

	// Bound exporter creation so a slow/unreachable collector doesn't block startup.
	// Use Background (not ctx) so a short-lived caller context cannot shorten the bound.
	expCtx, expCancel := context.WithTimeout(context.Background(), exporterCreationTimeout)
	exp, err := otlptracehttp.New(expCtx, opts...)
	expCancel() // Release timer resources immediately; otlptracehttp.New does not retain expCtx after returning.
	if err != nil {
		return nil, nil, fmt.Errorf("tracing: create OTLP exporter: %w", err)
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exp,
			sdktrace.WithBatchTimeout(batchFlushInterval),
			sdktrace.WithExportTimeout(batchExportTimeout),
		),
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.TraceIDRatioBased(sampleRatio))),
		sdktrace.WithResource(res),
	)
	// Shutdown flushes the BatchSpanProcessor and shuts down the exporter via
	// tp.Shutdown, which transitively calls exp.Shutdown through the BSP.
	shutdown := func(ctx context.Context) error {
		return tp.Shutdown(ctx)
	}
	return tp, shutdown, nil
}
