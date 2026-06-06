package runtime

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"
	"sync"
	"time"

	oteltrace "go.opentelemetry.io/otel/trace"

	"github.com/Unluckyathecking/crucible/gateway/internal/config"
	"github.com/Unluckyathecking/crucible/gateway/internal/proxy"
	"github.com/Unluckyathecking/crucible/gateway/internal/tracing"
)

// noopShutdown is the no-op Shutdown used when tracing is disabled or on error paths.
func noopShutdown(_ context.Context) error { return nil }

// tracerCleanupTimeout bounds the cleanup of a partially-initialised tracer provider
// so a hung OTLP flush cannot stall server startup indefinitely.
const tracerCleanupTimeout = 10 * time.Second

// cleanupTracer shuts down the provider and joins any cleanup error with baseErr.
// A nil shutdown is a no-op; callers need not guard against it.
// ctx is the parent for the cleanup timeout; a nil ctx falls back to
// context.Background(). Cleanup always adds a tracerCleanupTimeout deadline on
// top of the parent so callers block for at most tracerCleanupTimeout.
func cleanupTracer(ctx context.Context, shutdown func(context.Context) error, baseErr error) error {
	if shutdown == nil {
		return baseErr
	}
	parent := ctx
	if parent == nil {
		parent = context.Background()
	}
	timeoutCtx, cancel := context.WithTimeout(parent, tracerCleanupTimeout)
	defer cancel()
	if shutdownErr := shutdown(timeoutCtx); shutdownErr != nil {
		if baseErr == nil {
			return fmt.Errorf("runtime: cleaning up partial tracer provider: %w", shutdownErr)
		}
		return errors.Join(baseErr, fmt.Errorf("runtime: cleaning up partial tracer provider: %w", shutdownErr))
	}
	return baseErr
}

// Components holds the assembled runtime dependencies ready for injection into
// proxy.New and server.Deps. Always obtain Components through Assemble — a literal
// Components{} has a nil Shutdown and must not be used directly.
//
// Values returned by Assemble are always safe: a zero ResiliencePolicy means
// single-shot (no retry, no breaker); a nil TracerProvider means no-op tracing;
// Shutdown is always non-nil (it is the no-op on any error return from Assemble).
type Components struct {
	Policy         proxy.ResiliencePolicy
	TracerProvider oteltrace.TracerProvider
	Shutdown       func(context.Context) error
	_              struct{} // prevents positional literals outside the package
}

// Assemble builds Components from a validated *config.Config.
// With all resilience and tracing knobs at their defaults it returns a
// zero-value ResiliencePolicy, a nil TracerProvider, and a non-nil no-op
// shutdown — preserving today's exact single-shot behaviour.
// On error, the returned Components always has a non-nil no-op Shutdown.
func Assemble(cfg *config.Config) (Components, error) {
	return assemble(cfg, func(endpoint string, insecure bool, sampleRatio float64) (oteltrace.TracerProvider, func(context.Context) error, error) {
		tp, shutdown, err := tracing.NewProvider(endpoint, insecure, sampleRatio)
		if tp == nil {
			// Return untyped nil to avoid the typed-nil interface gotcha.
			return nil, shutdown, err
		}
		return tp, shutdown, err
	})
}

// assemble is the testable core of Assemble. ctor injects the tracer-provider
// factory so tests can avoid dialling a real OTLP endpoint. cfg must not be nil.
func assemble(cfg *config.Config, ctor func(string, bool, float64) (oteltrace.TracerProvider, func(context.Context) error, error)) (Components, error) {
	c := Components{Shutdown: noopShutdown}
	if cfg == nil {
		return c, errors.New("runtime: config is nil")
	}

	// config.Load validates WorkerRetryMax and WorkerBreakerThreshold as non-negative
	// and OtelSampleRatio as [0.0, 1.0], so >0 is the sole activation gate here;
	// zero means disabled and no invalid values reach assemble.
	if cfg.WorkerRetryMax > 0 {
		c.Policy.Retry.MaxAttempts = cfg.WorkerRetryMax
		c.Policy.Retry.BaseBackoff = cfg.RetryBaseBackoff()
	}
	if cfg.WorkerBreakerThreshold > 0 {
		c.Policy.Breaker.Threshold = cfg.WorkerBreakerThreshold
		c.Policy.Breaker.Cooldown = cfg.BreakerCooldown()
	}

	if cfg.OtelTracingEnabled {
		tp, shutdown, ctorErr := ctor(cfg.OtelExporterEndpoint, cfg.OtelExporterInsecure, cfg.OtelSampleRatio)
		if ctorErr != nil {
			return c, fmt.Errorf("runtime: constructing tracer provider: %w", cleanupTracer(context.Background(), shutdown, ctorErr))
		}
		if tp == nil {
			nilErr := fmt.Errorf("runtime: tracer provider constructor returned nil provider for endpoint %q", cfg.OtelExporterEndpoint)
			return c, cleanupTracer(context.Background(), shutdown, nilErr)
		}
		c.TracerProvider = tp
		// shutdownFn is an explicit value copy of the constructor's cleanup func.
		// The closure below calls it exactly once via sync.Once and caches the
		// result; panics are recovered and stored as errors. The first caller's
		// context is used; subsequent callers receive the cached result regardless
		// of their context.
		shutdownFn := shutdown
		if shutdownFn == nil {
			shutdownFn = noopShutdown
		}
		var (
			once        sync.Once
			shutdownErr error
		)
		c.Shutdown = func(ctx context.Context) error {
			once.Do(func() {
				defer func() {
					if r := recover(); r != nil {
						shutdownErr = fmt.Errorf("runtime: tracer shutdown panicked: %+v\n%s", r, debug.Stack())
					}
				}()
				shutdownErr = shutdownFn(ctx)
			})
			return shutdownErr
		}
	}

	return c, nil
}
