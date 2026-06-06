package runtime

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	oteltrace "go.opentelemetry.io/otel/trace"

	"github.com/Unluckyathecking/crucible/gateway/internal/config"
	"github.com/Unluckyathecking/crucible/gateway/internal/proxy"
	"github.com/Unluckyathecking/crucible/gateway/internal/tracing"
)

// tracerCleanupTimeout bounds the cleanup of a partially-initialised tracer provider
// so a hung OTLP flush cannot stall server startup indefinitely.
const tracerCleanupTimeout = 10 * time.Second

// noopShutdown is the no-op Shutdown used when tracing is disabled or on error paths.
func noopShutdown(_ context.Context) error { return nil }

// cleanupTracer shuts down the provider and joins any cleanup error with baseErr.
// A nil shutdown is a no-op; callers need not guard against it.
// Cleanup adds a tracerCleanupTimeout deadline to ctx; callers block for at
// most tracerCleanupTimeout (or the remaining deadline of ctx, whichever is shorter).
// Shutdown runs in a goroutine so a hung provider cannot block the caller beyond
// the deadline. Panics from shutdown are recovered and returned as errors.
func cleanupTracer(ctx context.Context, shutdown func(context.Context) error, baseErr error) error {
	if shutdown == nil {
		return baseErr
	}
	timeoutCtx, cancel := context.WithTimeout(ctx, tracerCleanupTimeout)
	defer cancel()
	type result struct{ err error }
	ch := make(chan result, 1) // buffered: goroutine can send even after we return on timeout
	go func() {
		var r result
		defer func() {
			if rec := recover(); rec != nil {
				r.err = fmt.Errorf("runtime: tracer shutdown panicked during cleanup: %v", rec)
			}
			select {
			case ch <- r:
			case <-timeoutCtx.Done():
			}
		}()
		r.err = shutdown(timeoutCtx)
	}()
	var shutdownErr error
	select {
	case r := <-ch:
		shutdownErr = r.err
	case <-timeoutCtx.Done():
		shutdownErr = fmt.Errorf("runtime: tracer cleanup timed out: %w", timeoutCtx.Err())
	}
	if shutdownErr != nil {
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
//
// The unexported _ field prevents positional struct literals outside this package,
// but does not prevent the zero-value Components{}; always use Assemble.
type Components struct {
	Policy         proxy.ResiliencePolicy
	TracerProvider oteltrace.TracerProvider
	Shutdown       func(context.Context) error
	_              struct{} // prevents positional literals outside the package
}

// Assemble builds Components from a validated *config.Config.
// ctx is used on error paths to bound cleanup of a partially-initialised tracer
// provider; it is not used on the happy path. Pass context.Background() if no
// caller deadline applies.
// With all resilience and tracing knobs at their defaults it returns a
// zero-value ResiliencePolicy, a nil TracerProvider, and a non-nil no-op
// shutdown — preserving today's exact single-shot behaviour.
// On error, the returned Components always has a non-nil no-op Shutdown.
func Assemble(ctx context.Context, cfg *config.Config) (Components, error) {
	return assemble(ctx, cfg, func(endpoint string, insecure bool, sampleRatio float64) (oteltrace.TracerProvider, func(context.Context) error, error) {
		return tracing.NewProvider(endpoint, insecure, sampleRatio)
	})
}

// assemble is the testable core of Assemble. ctor injects the tracer-provider
// factory so tests can avoid dialling a real OTLP endpoint. cfg must not be nil.
func assemble(ctx context.Context, cfg *config.Config, ctor func(string, bool, float64) (oteltrace.TracerProvider, func(context.Context) error, error)) (Components, error) {
	c := Components{Shutdown: noopShutdown}
	if cfg == nil {
		return c, errors.New("runtime: config is nil")
	}

	// >0 gates retry/breaker activation; zero or negative means disabled.
	// Duration helpers (RetryBaseBackoff, BreakerCooldown) are only written when
	// they return a positive value, guarding against negative env-var inputs.
	if cfg.WorkerRetryMax > 0 {
		c.Policy.Retry.MaxAttempts = cfg.WorkerRetryMax
		if b := cfg.RetryBaseBackoff(); b > 0 {
			c.Policy.Retry.BaseBackoff = b
		}
	}
	if cfg.WorkerBreakerThreshold > 0 {
		c.Policy.Breaker.Threshold = cfg.WorkerBreakerThreshold
		if d := cfg.BreakerCooldown(); d > 0 {
			c.Policy.Breaker.Cooldown = d
		}
	}

	if cfg.OtelTracingEnabled {
		if cfg.OtelSampleRatio < 0 || cfg.OtelSampleRatio > 1 {
			return c, fmt.Errorf("runtime: OtelSampleRatio must be in [0.0, 1.0], got %v", cfg.OtelSampleRatio)
		}
		tp, shutdown, ctorErr := ctor(cfg.OtelExporterEndpoint, cfg.OtelExporterInsecure, cfg.OtelSampleRatio)
		if ctorErr != nil {
			return c, cleanupTracer(ctx, shutdown, fmt.Errorf("runtime: constructing tracer provider: %w", ctorErr))
		}
		if tp == nil {
			nilErr := fmt.Errorf("runtime: tracer provider constructor returned nil provider for endpoint %q", cfg.OtelExporterEndpoint)
			return c, cleanupTracer(ctx, shutdown, nilErr)
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
			shutdownErr error // written exactly once, inside once.Do
		)
		c.Shutdown = func(shutdownCtx context.Context) error {
			once.Do(func() {
				defer func() {
					if r := recover(); r != nil {
						shutdownErr = fmt.Errorf("runtime: tracer shutdown panicked: %v", r)
					}
				}()
				shutdownErr = shutdownFn(shutdownCtx)
			})
			return shutdownErr
		}
	}

	return c, nil
}
