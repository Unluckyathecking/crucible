package runtime

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"

	oteltrace "go.opentelemetry.io/otel/trace"

	"github.com/Unluckyathecking/crucible/gateway/internal/config"
	"github.com/Unluckyathecking/crucible/gateway/internal/proxy"
	"github.com/Unluckyathecking/crucible/gateway/internal/tracing"
)

// noopShutdown is used for Components.Shutdown when tracing is disabled.
// A single package-level value avoids allocating a new closure on every
// assemble call in the common (tracing-off) path.
var noopShutdown = func(_ context.Context) error { return nil }

// Components holds the assembled runtime dependencies ready for injection into
// proxy.New and server.Deps. Zero values are safe: a zero ResiliencePolicy means
// single-shot (no retry, no breaker); a nil TracerProvider means no-op tracing.
//
// Shutdown must be called to release resources. Components contains internal
// synchronization state and must not be copied after first use.
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
	if cfg == nil {
		return Components{}, errors.New("runtime: config is nil")
	}
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
		if cfg.OtelSampleRatio < 0 || cfg.OtelSampleRatio > 1 {
			return c, fmt.Errorf("runtime: OtelSampleRatio must be in [0.0, 1.0], got %v", cfg.OtelSampleRatio)
		}
		tp, shutdown, err := ctor(cfg.OtelExporterEndpoint, cfg.OtelExporterInsecure, cfg.OtelSampleRatio)
		if err != nil {
			// If the constructor returned a cleanup func alongside the error,
			// call it now to avoid leaking a partially-initialised provider.
			// context.Background is used intentionally: assemble has no caller
			// context to propagate, and this cleanup is best-effort on failure.
			// Join any cleanup error into the returned error so both failures surface.
			if shutdown != nil {
				if shutdownErr := shutdown(context.Background()); shutdownErr != nil {
					err = errors.Join(err, fmt.Errorf("runtime: cleaning up partial tracer provider: %w", shutdownErr))
				}
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
		// sync.Once guarantees the provider shuts down exactly once. atomic.Pointer
		// stores the error result so concurrent callers observe the written value
		// without relying solely on happens-before reasoning about once.Do internals.
		var h struct {
			once sync.Once
			err  atomic.Pointer[error]
		}
		c.Shutdown = func(ctx context.Context) error {
			h.once.Do(func() {
				e := shutdownFn(ctx)
				h.err.Store(&e)
			})
			if e := h.err.Load(); e != nil {
				return *e
			}
			return nil
		}
	}

	return c, nil
}
