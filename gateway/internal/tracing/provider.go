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

// NewProvider constructs a TracerProvider that exports spans via OTLP/HTTP to endpoint.
// insecure disables TLS — use true only for localhost/sidecar collectors.
// sampleRatio must be in [0.0, 1.0]; 1.0 samples every trace, 0.0 samples none.
// The returned shutdown function flushes pending spans; call it at process exit with
// a context whose deadline exceeds the export timeout (10 s) so in-flight exports complete.
//
// TLS limitation: custom CA certificates and mutual TLS (mTLS) are not
// supported — the exporter uses the system certificate pool when insecure=false.
// To use a private CA or mTLS, replace this constructor with one that calls
// otlptracehttp.WithTLSClientConfig(tlsCfg) directly.
func NewProvider(ctx context.Context, endpoint string, insecure bool, sampleRatio float64) (*sdktrace.TracerProvider, func(context.Context) error, error) {
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
	expCtx, expCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer expCancel()
	exp, err := otlptracehttp.New(expCtx, opts...)
	if err != nil {
		return nil, nil, fmt.Errorf("tracing: create OTLP exporter: %w", err)
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exp,
			sdktrace.WithBatchTimeout(5*time.Second),
			sdktrace.WithExportTimeout(10*time.Second),
		),
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.TraceIDRatioBased(sampleRatio))),
		sdktrace.WithResource(res),
	)
	// Return an explicit composite shutdown: tp.Shutdown flushes the batch processor
	// and calls exp.Shutdown internally via the BatchSpanProcessor chain. The explicit
	// exp.Shutdown call is idempotent; return its error when tp.Shutdown succeeds so
	// callers can detect HTTP-connection-drain failures.
	shutdown := func(ctx context.Context) error {
		tpErr := tp.Shutdown(ctx)
		expErr := exp.Shutdown(ctx) // idempotent: BSP already called this via tp.Shutdown
		if tpErr != nil {
			return tpErr
		}
		return expErr
	}
	return tp, shutdown, nil
}
