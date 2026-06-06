package runtime

import (
	"context"
	"errors"
	"fmt"
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

// Assemble builds Components from a validated *config.Config.
// With all resilience and tracing knobs at their defaults it returns a
// zero-value ResiliencePolicy, a nil TracerProvider, and a non-nil no-op
// shutdown — preserving today's exact single-shot behaviour.
func Assemble(cfg *config.Config) (Components, error) {
	return assemble(cfg, func(endpoint string, insecure bool, sampleRatio float64) (oteltrace.TracerProvider, func(context.Context) error, error) {
		return tracing.NewProvider(endpoint, insecure, sampleRatio)
	})
}

// assemble is the testable core of Assemble. ctor injects the tracer-provider
// factory, allowing tests to avoid dialling a real OTLP endpoint.
func assemble(cfg *config.Config, ctor func(string, bool, float64) (oteltrace.TracerProvider, func(context.Context) error, error)) (Components, error) {
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
		tp, shutdown, err := ctor(cfg.OtelExporterEndpoint, cfg.OtelExporterInsecure, cfg.OtelSampleRatio)
		if err != nil {
			// Return c (not Components{}) so the caller gets a nil TracerProvider
			// and a non-nil no-op Shutdown even when provider construction fails.
			return c, fmt.Errorf("runtime: constructing tracer provider: %w", err)
		}
		if tp == nil {
			return c, fmt.Errorf("runtime: tracer provider constructor returned nil provider")
		}
		// Guard against a constructor that returns nil shutdown with nil error.
		shutdownFn := shutdown
		if shutdownFn == nil {
			shutdownFn = func(_ context.Context) error { return nil }
		}
		c.TracerProvider = tp
		// sync.Once guarantees the provider's shutdown runs exactly once;
		// the result is cached and returned on all subsequent calls.
		var once sync.Once
		var shutdownErr error
		c.Shutdown = func(ctx context.Context) error {
			once.Do(func() {
				shutdownErr = shutdownFn(ctx)
			})
			return shutdownErr
		}
	}

	return c, nil
}
