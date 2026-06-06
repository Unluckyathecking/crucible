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

// noopShutdown is the no-op used for Components.Shutdown when tracing is
// disabled. A single package-level value avoids a closure allocation on every
// assemble call in the common (tracing-off) path.
var noopShutdown = func(_ context.Context) error { return nil }

// Components holds the assembled runtime dependencies ready for injection into
// proxy.New and server.Deps. Zero values are safe: a zero ResiliencePolicy means
// single-shot (no retry, no breaker); a nil TracerProvider means no-op tracing.
//
// Shutdown is a closure that captures a sync.Once on the heap. Copying a
// Components value copies the func pointer but all copies share the same
// once-guarded execution: the provider shuts down exactly once regardless of
// how many copies exist.
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
		tp, shutdown, err := tracing.NewProvider(endpoint, insecure, sampleRatio)
		if tp == nil {
			// tracing.NewProvider returns a concrete *sdktrace.TracerProvider; a
			// typed-nil wrapped in the interface is non-nil, defeating assemble's
			// nil-provider guard. Return an untyped nil explicitly.
			return nil, shutdown, err
		}
		return tp, shutdown, err
	})
}

// assemble is the testable core of Assemble. ctor injects the tracer-provider
// factory, allowing tests to avoid dialling a real OTLP endpoint.
func assemble(cfg *config.Config, ctor func(string, bool, float64) (oteltrace.TracerProvider, func(context.Context) error, error)) (Components, error) {
	if cfg == nil {
		return Components{}, errors.New("runtime: config is nil")
	}

	c := Components{
		Shutdown: noopShutdown,
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
			// If the constructor returned a cleanup func alongside the error,
			// call it now to avoid leaking a partially-initialised provider.
			if shutdown != nil {
				_ = shutdown(context.Background())
			}
			// Return c (not Components{}) so the caller gets a nil TracerProvider
			// and a non-nil no-op Shutdown even when provider construction fails.
			return c, fmt.Errorf("runtime: constructing tracer provider: %w", err)
		}
		if tp == nil {
			return c, errors.New("runtime: tracer provider constructor returned nil provider")
		}
		// Guard against a constructor that returns nil shutdown with nil error.
		shutdownFn := shutdown
		if shutdownFn == nil {
			shutdownFn = noopShutdown
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
