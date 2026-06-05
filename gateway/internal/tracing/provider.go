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
// insecure disables TLS — use true only for localhost/sidecar collectors.
// sampleRatio must be in [0.0, 1.0]; 1.0 samples every trace, 0.0 samples none.
// The returned shutdown function flushes pending spans; call it at process exit with
// a context whose deadline exceeds batchExportTimeout so in-flight exports complete.
//
// Note: exporter creation uses context.Background internally (not a caller-supplied
// context) so a short-lived caller context cannot abort startup. The exporterCreationTimeout
// constant is the startup bound.
//
// TLS limitation: custom CA certificates and mutual TLS (mTLS) are not
// supported — the exporter uses the system certificate pool when insecure=false.
// To use a private CA or mTLS, replace this constructor with one that calls
// otlptracehttp.WithTLSClientConfig(tlsCfg) directly.
func NewProvider(endpoint string, insecure bool, sampleRatio float64) (*sdktrace.TracerProvider, func(context.Context) error, error) {
	if endpoint == "" {
		return nil, nil, fmt.Errorf("tracing: endpoint cannot be empty")
	}
	// Build the resource first so that a merge error never leaks an already-opened exporter.
	res, err := resource.Merge(
		resource.Default(),
		resource.NewSchemaless(attribute.String("service.name", "crucible-gateway")),
	)
	if err != nil {
		return nil, nil, fmt.Errorf("tracing: merge resource: %w", err)
	}

	opts := []otlptracehttp.Option{otlptracehttp.WithEndpoint(endpoint)}
	if insecure {
		opts = append(opts, otlptracehttp.WithInsecure())
	}

	// Bound exporter creation so a slow/unreachable collector doesn't block startup.
	// Use Background (not ctx) so a short-lived caller context cannot shorten the bound.
	expCtx, expCancel := context.WithTimeout(context.Background(), exporterCreationTimeout)
	defer expCancel()
	exp, err := otlptracehttp.New(expCtx, opts...)
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
	// Shutdown flushes the BatchSpanProcessor (tp.Shutdown) and then explicitly
	// shuts down the exporter (exp.Shutdown). The BSP calls exp.Shutdown internally
	// during a clean flush, but may skip it when the context is cancelled during
	// the drain phase — calling exp.Shutdown directly ensures background goroutines
	// in the OTLP client are always stopped. First error wins.
	shutdown := func(ctx context.Context) error {
		var firstErr error
		if err := tp.Shutdown(ctx); err != nil {
			firstErr = err
		}
		if err := exp.Shutdown(ctx); err != nil && firstErr == nil {
			firstErr = err
		}
		return firstErr
	}
	return tp, shutdown, nil
}
