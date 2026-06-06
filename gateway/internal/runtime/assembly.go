package runtime

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sync"

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
// Shutdown must be called to release resources. On any error return from Assemble,
// Shutdown is still non-nil and safe to call (it is the no-op).
type Components struct {
	Policy         proxy.ResiliencePolicy
	TracerProvider oteltrace.TracerProvider
	Shutdown       func(context.Context) error
}

// Assemble builds Components from a validated *config.Config.
// With all resilience and tracing knobs at their defaults it returns a
// zero-value ResiliencePolicy, a nil TracerProvider, and a non-nil no-op
// shutdown — preserving today's exact single-shot behaviour.
// On error, the returned Components always has a non-nil no-op Shutdown.
func Assemble(cfg *config.Config) (Components, error) {
	return assemble(cfg, func(endpoint string, insecure bool, sampleRatio float64) (oteltrace.TracerProvider, func(context.Context) error, error) {
		return tracing.NewProvider(endpoint, insecure, sampleRatio)
	})
}

// assemble is the testable core of Assemble. ctor injects the tracer-provider
// factory, allowing tests to avoid dialling a real OTLP endpoint.
func assemble(cfg *config.Config, ctor func(string, bool, float64) (oteltrace.TracerProvider, func(context.Context) error, error)) (Components, error) {
	if cfg == nil {
		return Components{Shutdown: noopShutdown}, errors.New("runtime: config is nil")
	}

	c := Components{
		Shutdown: noopShutdown,
	}

	// Build resilience policy. Fields are set directly to avoid importing the
	// resilience package; the types are already carried by proxy.ResiliencePolicy.
	// config.Load already rejects negative values, but assemble guards defensively
	// so that a *config.Config constructed directly (e.g. in tests) cannot produce
	// a silently broken policy with a nonsensical negative count.
	if cfg.WorkerRetryMax < 0 {
		return c, fmt.Errorf("runtime: WorkerRetryMax must be >= 0, got %d", cfg.WorkerRetryMax)
	}
	if cfg.WorkerRetryMax > 0 {
		if cfg.WorkerRetryBackoffMS < 0 {
			return c, fmt.Errorf("runtime: WorkerRetryBackoffMS must be >= 0 when retry is enabled, got %d", cfg.WorkerRetryBackoffMS)
		}
		c.Policy.Retry.MaxAttempts = cfg.WorkerRetryMax
		c.Policy.Retry.BaseBackoff = cfg.RetryBaseBackoff()
	}
	if cfg.WorkerBreakerThreshold < 0 {
		return c, fmt.Errorf("runtime: WorkerBreakerThreshold must be >= 0, got %d", cfg.WorkerBreakerThreshold)
	}
	if cfg.WorkerBreakerThreshold > 0 {
		if cfg.WorkerBreakerCooldownMS < 0 {
			return c, fmt.Errorf("runtime: WorkerBreakerCooldownMS must be >= 0 when breaker is enabled, got %d", cfg.WorkerBreakerCooldownMS)
		}
		c.Policy.Breaker.Threshold = cfg.WorkerBreakerThreshold
		c.Policy.Breaker.Cooldown = cfg.BreakerCooldown()
	}

	if cfg.OtelTracingEnabled {
		if cfg.OtelExporterEndpoint == "" {
			return c, errors.New("runtime: OtelExporterEndpoint must not be empty when tracing is enabled")
		}
		// NaN fails all IEEE 754 comparisons, so it must be checked explicitly.
		// Inf is also rejected: it is outside [0.0, 1.0] semantically but passes
		// the < 0 || > 1 check for positive infinity.
		if cfg.OtelSampleRatio < 0 || cfg.OtelSampleRatio > 1 || math.IsNaN(cfg.OtelSampleRatio) || math.IsInf(cfg.OtelSampleRatio, 0) {
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
			// Guard against a constructor that returns (nil, shutdown, nil).
			// Call any provided cleanup to avoid resource leaks.
			if shutdown != nil {
				_ = shutdown(context.Background())
			}
			return c, fmt.Errorf("runtime: tracer provider constructor returned nil provider for endpoint %q", cfg.OtelExporterEndpoint)
		}
		// Guard against a constructor that returns nil shutdown with nil error.
		shutdownFn := shutdown
		if shutdownFn == nil {
			shutdownFn = noopShutdown
		}
		c.TracerProvider = tp
		// h bundles the sync.Once and its cached result so both are heap-allocated
		// together and their relationship is explicit to readers and the race detector.
		// sync.Once guarantees Shutdown runs exactly once; per the Go memory model the
		// write to h.err inside once.Do happens-before the return of every concurrent
		// once.Do call, so the subsequent read is race-free. recover() converts a
		// panicking shutdownFn into an error so callers always see a failure rather
		// than a silently nil result.
		var h struct {
			once sync.Once
			err  error
		}
		c.Shutdown = func(ctx context.Context) error {
			h.once.Do(func() {
				defer func() {
					if r := recover(); r != nil {
						if e, ok := r.(error); ok {
							h.err = fmt.Errorf("runtime: tracer shutdown panicked: %w", e)
						} else {
							h.err = fmt.Errorf("runtime: tracer shutdown panicked: %v", r)
						}
					}
				}()
				h.err = shutdownFn(ctx)
			})
			return h.err
		}
	}

	return c, nil
}
