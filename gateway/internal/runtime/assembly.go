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
				h.err = fmt.Errorf("runtime: tracer shutdown panicked: %v", r)
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
	_              struct{} // prevents positional literals outside the package
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
func assemble(cfg *config.Config, ctor func(string, bool, float64) (oteltrace.TracerProvider, func(context.Context) error, error)) (c Components, err error) {
	c = Components{Shutdown: noopShutdown}
	if cfg == nil {
		return c, errors.New("runtime: config is nil")
	}

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
			if shutdown != nil {
				ctorErr = cleanupTracer(shutdown, ctorErr)
			}
			return c, fmt.Errorf("runtime: constructing tracer provider: %w", ctorErr)
		}
		if tp == nil {
			nilErr := fmt.Errorf("runtime: tracer provider constructor returned nil provider for endpoint %q", cfg.OtelExporterEndpoint)
			if shutdown != nil {
				nilErr = cleanupTracer(shutdown, nilErr)
			}
			return c, nilErr
		}
		c.TracerProvider = tp
		handle := newShutdownHandle(shutdown)
		c.Shutdown = handle.shutdown
	}

	return c, nil
}
