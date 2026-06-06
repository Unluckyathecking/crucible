package runtime

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net"
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

// shutdownHandle bundles a sync.Once with its cached error and the underlying
// shutdown function. newShutdownHandle heap-allocates the handle; the returned
// bound method value (handle.shutdown) captures the pointer, so all copies of a
// Components struct share the same idempotent shutdown state. mu guards h.err
// to make the synchronisation explicit for both normal and panic-recovery paths.
type shutdownHandle struct {
	once sync.Once
	mu   sync.Mutex
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
				h.mu.Lock()
				h.err = fmt.Errorf("runtime: tracer shutdown panicked: %v", r)
				h.mu.Unlock()
			}
		}()
		err := h.fn(ctx)
		h.mu.Lock()
		h.err = err
		h.mu.Unlock()
	})
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.err
}

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
		return Components{Shutdown: noopShutdown}, fmt.Errorf("runtime: WorkerRetryMax must be >= 0, got %d", cfg.WorkerRetryMax)
	}
	if cfg.WorkerRetryMax > 0 {
		if cfg.WorkerRetryBackoffMS < 0 {
			return Components{Shutdown: noopShutdown}, fmt.Errorf("runtime: WorkerRetryBackoffMS must be >= 0 when retry is enabled, got %d", cfg.WorkerRetryBackoffMS)
		}
		c.Policy.Retry.MaxAttempts = cfg.WorkerRetryMax
		c.Policy.Retry.BaseBackoff = cfg.RetryBaseBackoff()
	}
	if cfg.WorkerBreakerThreshold < 0 {
		return Components{Shutdown: noopShutdown}, fmt.Errorf("runtime: WorkerBreakerThreshold must be >= 0, got %d", cfg.WorkerBreakerThreshold)
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
		// Validate endpoint format before passing to the OTLP constructor so
		// misconfigured values produce a clear error at assembly time rather than
		// a confusing dial error at first trace export.
		host, _, err := net.SplitHostPort(cfg.OtelExporterEndpoint)
		if err != nil {
			return Components{Shutdown: noopShutdown}, fmt.Errorf("runtime: OtelExporterEndpoint must be host:port (e.g. localhost:4318): %w", err)
		}
		if host == "" {
			return Components{Shutdown: noopShutdown}, fmt.Errorf("runtime: OtelExporterEndpoint must have a non-empty host, got %q", cfg.OtelExporterEndpoint)
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
			// context.Background is used intentionally: assemble has no caller
			// context to propagate, and this cleanup is best-effort on failure.
			// Join any cleanup error into the returned error so both failures surface.
			if shutdown != nil {
				if shutdownErr := shutdown(context.Background()); shutdownErr != nil {
					ctorErr = errors.Join(ctorErr, fmt.Errorf("runtime: cleaning up partial tracer provider: %w", shutdownErr))
				}
			}
			return Components{Shutdown: noopShutdown}, fmt.Errorf("runtime: constructing tracer provider: %w", ctorErr)
		}
		if tp == nil {
			// Guard against a constructor that returns (nil, shutdown, nil).
			// Call any provided cleanup to avoid resource leaks. Join any cleanup
			// error into the returned error for visibility, consistent with the
			// partial-init cleanup above.
			nilErr := fmt.Errorf("runtime: tracer provider constructor returned nil provider for endpoint %q", cfg.OtelExporterEndpoint)
			if shutdown != nil {
				if cleanupErr := shutdown(context.Background()); cleanupErr != nil {
					nilErr = errors.Join(nilErr, fmt.Errorf("runtime: cleaning up partial tracer provider: %w", cleanupErr))
				}
			}
			return Components{Shutdown: noopShutdown}, nilErr
		}
		c.TracerProvider = tp
		handle := newShutdownHandle(shutdown)
		c.Shutdown = handle.shutdown
	}

	return c, nil
}
