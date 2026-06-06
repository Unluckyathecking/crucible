package runtime

import (
	"context"
	"errors"
	"sync"

	oteltrace "go.opentelemetry.io/otel/trace"

	"github.com/Unluckyathecking/crucible/gateway/internal/config"
	"github.com/Unluckyathecking/crucible/gateway/internal/proxy"
	"github.com/Unluckyathecking/crucible/gateway/internal/tracing"
)

// Components holds the assembled runtime dependencies ready for injection into
// proxy.New and server.Deps. Zero values are safe: a zero ResiliencePolicy means
// single-shot (no retry, no breaker); a nil TracerProvider means no-op tracing.
type Components struct {
	Policy         proxy.ResiliencePolicy
	TracerProvider oteltrace.TracerProvider
	Shutdown       func(context.Context) error
}

// tracerProviderConstructor is the seam that tests replace to avoid dialling a real
// OTLP exporter. Production code always uses tracing.NewProvider.
// NOT safe for concurrent mutation; do not call t.Parallel() in tests that swap this.
var tracerProviderConstructor = func(endpoint string, insecure bool, sampleRatio float64) (oteltrace.TracerProvider, func(context.Context) error, error) {
	return tracing.NewProvider(endpoint, insecure, sampleRatio)
}

// Assemble builds Components from a validated *config.Config.
// With all resilience and tracing knobs at their defaults it returns a
// zero-value ResiliencePolicy, a nil TracerProvider, and a non-nil no-op
// shutdown — preserving today's exact single-shot behaviour.
func Assemble(cfg *config.Config) (Components, error) {
	if cfg == nil {
		return Components{}, errors.New("runtime: config is nil")
	}

	c := Components{
		Shutdown: func(_ context.Context) error { return nil },
	}

	// Build resilience policy. Fields are set directly to avoid importing the
	// resilience package; the types are already carried by proxy.ResiliencePolicy.
	if cfg.WorkerRetryMax > 0 {
		c.Policy.Retry.MaxAttempts = cfg.WorkerRetryMax
		c.Policy.Retry.BaseBackoff = cfg.RetryBaseBackoff()
	}
	if cfg.WorkerBreakerThreshold > 0 {
		c.Policy.Breaker.Threshold = cfg.WorkerBreakerThreshold
		c.Policy.Breaker.Cooldown = cfg.BreakerCooldown()
	}

	if cfg.OtelTracingEnabled {
		tp, shutdown, err := tracerProviderConstructor(cfg.OtelExporterEndpoint, cfg.OtelExporterInsecure, cfg.OtelSampleRatio)
		if err != nil {
			// Return c (not Components{}) so the caller gets a nil TracerProvider
			// and a non-nil no-op Shutdown even when provider construction fails.
			return c, err
		}
		c.TracerProvider = tp
		// Chain prev shutdown before the provider's shutdown so future resilience
		// subsystems that register their own shutdown are not silently dropped.
		prevShutdown := c.Shutdown
		var once sync.Once
		var shutdownErr error
		c.Shutdown = func(ctx context.Context) error {
			// Both prevShutdown and shutdown always run so neither leaks resources
			// even when the other fails. errors.Join returns nil if both return nil.
			once.Do(func() {
				shutdownErr = errors.Join(prevShutdown(ctx), shutdown(ctx))
			})
			return shutdownErr
		}
	}

	return c, nil
}
