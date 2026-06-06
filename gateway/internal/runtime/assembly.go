package runtime

import (
	"context"
	"errors"
	"fmt"
	"math"
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
func cleanupTracer(shutdown func(context.Context) error, baseErr error) error {
	ctx, cancel := context.WithTimeout(context.Background(), tracerCleanupTimeout)
	defer cancel()
	if shutdownErr := shutdown(ctx); shutdownErr != nil {
		return errors.Join(baseErr, fmt.Errorf("runtime: cleaning up partial tracer provider: %w", shutdownErr))
	}
	return baseErr
}

// shutdownHandle wraps sync.Once to call h.fn at most once and cache the result.
// The closure passed to once.Do recovers any panic from h.fn and stores it as an
// error, so the closure always returns normally; once.Do marks it done on the
// first call regardless of panics.
type shutdownHandle struct {
	once sync.Once
	err  error
	fn   func(context.Context) error
}

func newShutdownHandle(fn func(context.Context) error) *shutdownHandle {
	if fn == nil {
		fn = noopShutdown
	}
	return &shutdownHandle{fn: fn}
}

func (h *shutdownHandle) shutdown(ctx context.Context) error {
	h.once.Do(func() {
		defer func() {
			if r := recover(); r != nil {
				panicErr := fmt.Errorf("runtime: tracer shutdown panicked: %v", r)
				if h.err != nil {
					h.err = errors.Join(h.err, panicErr)
				} else {
					h.err = panicErr
				}
			}
		}()
		h.err = h.fn(ctx)
	})
	// once.Do's happens-before guarantee makes this read race-free.
	return h.err
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
// factory so tests can avoid dialling a real OTLP endpoint. cfg must not be nil.
func assemble(cfg *config.Config, ctor func(string, bool, float64) (oteltrace.TracerProvider, func(context.Context) error, error)) (Components, error) {
	if cfg == nil {
		return Components{Shutdown: noopShutdown}, errors.New("runtime: config is nil")
	}

	c := Components{
		Shutdown: noopShutdown,
	}

	if cfg.WorkerRetryMax < 0 {
		return Components{Shutdown: noopShutdown}, fmt.Errorf("runtime: WorkerRetryMax must be >= 0, got %d", cfg.WorkerRetryMax)
	}
	if cfg.WorkerRetryMax > 0 {
		if cfg.WorkerRetryBackoffMS < 0 {
			return Components{Shutdown: noopShutdown}, fmt.Errorf("runtime: WorkerRetryBackoffMS must be >= 0 when retry is enabled, got %d", cfg.WorkerRetryBackoffMS)
		}
		c.Policy.Retry.MaxAttempts = cfg.WorkerRetryMax
		c.Policy.Retry.BaseBackoff = cfg.RetryBaseBackoff()
	}
	// A non-zero cooldown with a zero threshold silently discards the cooldown
	// because the breaker is disabled. Reject this foot-gun configuration explicitly.
	if cfg.WorkerBreakerThreshold == 0 && cfg.WorkerBreakerCooldownMS != 0 {
		return Components{Shutdown: noopShutdown}, fmt.Errorf("runtime: WorkerBreakerCooldownMS must be 0 when WorkerBreakerThreshold is 0 (breaker disabled), got %d", cfg.WorkerBreakerCooldownMS)
	}
	if cfg.WorkerBreakerThreshold > 0 {
		if cfg.WorkerBreakerCooldownMS <= 0 {
			return Components{Shutdown: noopShutdown}, fmt.Errorf("runtime: WorkerBreakerCooldownMS must be > 0 when breaker is enabled, got %d", cfg.WorkerBreakerCooldownMS)
		}
		c.Policy.Breaker.Threshold = cfg.WorkerBreakerThreshold
		c.Policy.Breaker.Cooldown = cfg.BreakerCooldown()
	}

	if cfg.OtelTracingEnabled {
		if cfg.OtelExporterEndpoint == "" {
			return Components{Shutdown: noopShutdown}, errors.New("runtime: OtelExporterEndpoint must not be empty when tracing is enabled")
		}
		// NaN fails all IEEE 754 comparisons, so it must be checked explicitly.
		// ±Inf are already caught by the < 0 || > 1 bounds (−∞ < 0 and +∞ > 1 are
		// both true in IEEE 754); math.IsNaN is the only special-case needed.
		if cfg.OtelSampleRatio < 0 || cfg.OtelSampleRatio > 1 || math.IsNaN(cfg.OtelSampleRatio) {
			return Components{Shutdown: noopShutdown}, fmt.Errorf("runtime: OtelSampleRatio must be in [0.0, 1.0], got %g", cfg.OtelSampleRatio)
		}
		tp, shutdown, ctorErr := ctor(cfg.OtelExporterEndpoint, cfg.OtelExporterInsecure, cfg.OtelSampleRatio)
		if ctorErr != nil {
			// If the constructor returned a cleanup func alongside the error,
			// call it now to avoid leaking a partially-initialised provider.
			if shutdown != nil {
				ctorErr = cleanupTracer(shutdown, ctorErr)
			}
			return Components{Shutdown: noopShutdown}, fmt.Errorf("runtime: constructing tracer provider: %w", ctorErr)
		}
		if tp == nil {
			// Guard against a constructor that returns (nil, shutdown, nil).
			// Call any provided cleanup to avoid resource leaks.
			nilErr := fmt.Errorf("runtime: tracer provider constructor returned nil provider for endpoint %q", cfg.OtelExporterEndpoint)
			if shutdown != nil {
				nilErr = cleanupTracer(shutdown, nilErr)
			}
			return Components{Shutdown: noopShutdown}, nilErr
		}
		c.TracerProvider = tp
		handle := newShutdownHandle(shutdown)
		c.Shutdown = handle.shutdown
	}

	return c, nil
}
