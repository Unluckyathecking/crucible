package runtime

import (
	"context"
	"errors"
	"math"
	"strings"
	"sync"
	"testing"

	oteltrace "go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"

	"github.com/Unluckyathecking/crucible/gateway/internal/config"
)

func TestAssemble_DefaultOff(t *testing.T) {
	cfg := &config.Config{
		// all fields at zero values: tracing disabled, no retry, no breaker
	}
	c, err := assemble(cfg, func(_ string, _ bool, _ float64) (oteltrace.TracerProvider, func(context.Context) error, error) {
		t.Helper()
		t.Fatalf("ctor must not be called when tracing is disabled")
		panic("unreachable")
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c.Policy.Retry.MaxAttempts != 0 {
		t.Errorf("Retry.MaxAttempts: want 0, got %d", c.Policy.Retry.MaxAttempts)
	}
	if c.Policy.Retry.BaseBackoff != 0 {
		t.Errorf("Retry.BaseBackoff: want 0, got %v", c.Policy.Retry.BaseBackoff)
	}
	if c.Policy.Breaker.Threshold != 0 {
		t.Errorf("Breaker.Threshold: want 0, got %d", c.Policy.Breaker.Threshold)
	}
	if c.Policy.Breaker.Cooldown != 0 {
		t.Errorf("Breaker.Cooldown: want 0, got %v", c.Policy.Breaker.Cooldown)
	}
	if c.TracerProvider != nil {
		t.Error("TracerProvider: want nil when tracing disabled")
	}
	if c.Shutdown == nil {
		t.Fatal("Shutdown: want non-nil no-op func")
	}
	// Verify Shutdown is safe to call repeatedly without panic.
	for i := 0; i < 2; i++ {
		if err := c.Shutdown(context.Background()); err != nil {
			t.Errorf("no-op shutdown call %d: unexpected error %v", i+1, err)
		}
	}
}

func TestAssemble_NilConfig(t *testing.T) {
	c, err := Assemble(nil)
	if err == nil {
		t.Error("want error for nil config, got nil")
	}
	if c.Shutdown == nil {
		t.Error("Shutdown: want non-nil no-op even on nil config error")
	}
	if err := c.Shutdown(context.Background()); err != nil {
		t.Errorf("no-op shutdown on nil config: unexpected error %v", err)
	}
}

func TestAssemble_RetryEnabled(t *testing.T) {
	cfg := &config.Config{
		WorkerRetryMax:       3,
		WorkerRetryBackoffMS: 200,
	}
	c, err := assemble(cfg, func(_ string, _ bool, _ float64) (oteltrace.TracerProvider, func(context.Context) error, error) {
		t.Helper()
		t.Fatalf("ctor must not be called when tracing is disabled")
		panic("unreachable")
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c.Policy.Retry.MaxAttempts != 3 {
		t.Errorf("MaxAttempts: want 3, got %d", c.Policy.Retry.MaxAttempts)
	}
	if c.Policy.Retry.BaseBackoff != cfg.RetryBaseBackoff() {
		t.Errorf("BaseBackoff: want %v, got %v", cfg.RetryBaseBackoff(), c.Policy.Retry.BaseBackoff)
	}
	if c.Policy.Breaker.Threshold != 0 {
		t.Errorf("Breaker.Threshold should be zero when breaker disabled, got %d", c.Policy.Breaker.Threshold)
	}
	if c.TracerProvider != nil {
		t.Error("TracerProvider: want nil when tracing disabled")
	}
}

func TestAssemble_BreakerEnabled(t *testing.T) {
	cfg := &config.Config{
		WorkerBreakerThreshold:  5,
		WorkerBreakerCooldownMS: 3000,
	}
	c, err := assemble(cfg, func(_ string, _ bool, _ float64) (oteltrace.TracerProvider, func(context.Context) error, error) {
		t.Helper()
		t.Fatalf("ctor must not be called when tracing is disabled")
		panic("unreachable")
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c.Policy.Breaker.Threshold != 5 {
		t.Errorf("Threshold: want 5, got %d", c.Policy.Breaker.Threshold)
	}
	if c.Policy.Breaker.Cooldown != cfg.BreakerCooldown() {
		t.Errorf("Cooldown: want %v, got %v", cfg.BreakerCooldown(), c.Policy.Breaker.Cooldown)
	}
	if c.Policy.Retry.MaxAttempts != 0 {
		t.Errorf("Retry.MaxAttempts should be zero when retry disabled, got %d", c.Policy.Retry.MaxAttempts)
	}
}

func TestAssemble_ZeroDurations(t *testing.T) {
	// When WorkerRetryBackoffMS is zero, RetryBaseBackoff() returns zero;
	// assemble must accept it rather than treat it as an error.
	cfg := &config.Config{
		WorkerRetryMax:       1,
		WorkerRetryBackoffMS: 0,
	}
	c, err := assemble(cfg, func(_ string, _ bool, _ float64) (oteltrace.TracerProvider, func(context.Context) error, error) {
		t.Helper()
		t.Fatalf("ctor must not be called when tracing is disabled")
		panic("unreachable")
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c.Policy.Retry.MaxAttempts != 1 {
		t.Errorf("MaxAttempts: want 1, got %d", c.Policy.Retry.MaxAttempts)
	}
	if c.Policy.Retry.BaseBackoff != 0 {
		t.Errorf("BaseBackoff: want 0 when config helper returns zero, got %v", c.Policy.Retry.BaseBackoff)
	}
}

func TestAssemble_TracingEnabled(t *testing.T) {
	shutdownCalls := 0
	fakeShutdown := func(_ context.Context) error {
		shutdownCalls++
		return nil
	}
	fakeTP := noop.NewTracerProvider()

	mockCtor := func(endpoint string, insecure bool, sampleRatio float64) (oteltrace.TracerProvider, func(context.Context) error, error) {
		if endpoint != "otel.example.com:4317" {
			t.Errorf("endpoint: want %q, got %q", "otel.example.com:4317", endpoint)
		}
		if !insecure {
			t.Error("insecure: want true")
		}
		if sampleRatio != 0.5 {
			t.Errorf("sampleRatio: want 0.5, got %v", sampleRatio)
		}
		return fakeTP, fakeShutdown, nil
	}

	cfg := &config.Config{
		OtelTracingEnabled:   true,
		OtelExporterEndpoint: "otel.example.com:4317",
		OtelExporterInsecure: true,
		OtelSampleRatio:      0.5,
	}
	c, err := assemble(cfg, mockCtor)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c.TracerProvider != fakeTP {
		t.Errorf("TracerProvider: want fakeTP, got %v", c.TracerProvider)
	}
	// Verify the provider is functional, not just present.
	if tr := c.TracerProvider.Tracer("test"); tr == nil {
		t.Error("Tracer(): want non-nil tracer from assembled provider")
	}
	if c.Shutdown == nil {
		t.Fatal("Shutdown: want non-nil")
	}
	_ = c.Shutdown(context.Background())
	if shutdownCalls != 1 {
		t.Errorf("shutdown delegate calls: want 1, got %d", shutdownCalls)
	}
}

func TestAssemble_TracingNilProvider(t *testing.T) {
	// If the constructor returns (nil, nil, nil) Assemble must error rather than
	// silently delivering a nil TracerProvider to callers who expect a real one.
	nilCtor := func(_ string, _ bool, _ float64) (oteltrace.TracerProvider, func(context.Context) error, error) {
		return nil, nil, nil
	}
	cfg := &config.Config{
		OtelTracingEnabled:   true,
		OtelExporterEndpoint: "otel.example.com:4317",
	}
	c, err := assemble(cfg, nilCtor)
	if err == nil {
		t.Fatal("want error when constructor returns nil provider, got nil")
	}
	if c.Shutdown == nil {
		t.Error("Shutdown: want non-nil no-op even on nil provider error")
	}

	// When the constructor returns (nil, nonNilShutdown, nil), the cleanup func
	// must be called to avoid leaking any resources it holds.
	shutdownCalled := false
	nilProviderWithShutdownCtor := func(_ string, _ bool, _ float64) (oteltrace.TracerProvider, func(context.Context) error, error) {
		return nil, func(_ context.Context) error {
			shutdownCalled = true
			return nil
		}, nil
	}
	_, err = assemble(cfg, nilProviderWithShutdownCtor)
	if err == nil {
		t.Fatal("want error when constructor returns nil provider, got nil")
	}
	if !shutdownCalled {
		t.Error("shutdown cleanup must be called when constructor returns nil provider with non-nil shutdown")
	}
}

func TestAssemble_TracingNilShutdown(t *testing.T) {
	// If the constructor returns a non-nil provider with nil shutdown (contract
	// violation), the nil-shutdown guard substitutes a no-op so Shutdown() is
	// always safe to call.
	fakeTP := noop.NewTracerProvider()
	nilShutdownCtor := func(_ string, _ bool, _ float64) (oteltrace.TracerProvider, func(context.Context) error, error) {
		return fakeTP, nil, nil
	}
	cfg := &config.Config{
		OtelTracingEnabled:   true,
		OtelExporterEndpoint: "otel.example.com:4317",
	}
	c, err := assemble(cfg, nilShutdownCtor)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c.TracerProvider != fakeTP {
		t.Error("TracerProvider: want fakeTP")
	}
	if c.Shutdown == nil {
		t.Fatal("Shutdown: want non-nil")
	}
	if err := c.Shutdown(context.Background()); err != nil {
		t.Errorf("shutdown: unexpected error %v", err)
	}
}

func TestAssemble_TracingErrorPropagated(t *testing.T) {
	wantErr := errors.New("mock exporter failure")
	errCtor := func(_ string, _ bool, _ float64) (oteltrace.TracerProvider, func(context.Context) error, error) {
		return nil, nil, wantErr
	}

	cfg := &config.Config{
		OtelTracingEnabled:   true,
		OtelExporterEndpoint: "otel.example.com:4317",
	}
	c, err := assemble(cfg, errCtor)
	// assemble wraps the error; errors.Is unwraps the chain.
	if !errors.Is(err, wantErr) {
		t.Errorf("error: want %v (or wrapping it), got %v", wantErr, err)
	}
	if c.TracerProvider != nil {
		t.Error("TracerProvider: want nil on provider error")
	}
	if c.Shutdown == nil {
		t.Fatal("Shutdown: want non-nil no-op even on provider error")
	}
	if err := c.Shutdown(context.Background()); err != nil {
		t.Errorf("Shutdown on provider error: unexpected error %v", err)
	}
}

func TestAssemble_TracingPartialError(t *testing.T) {
	// ctor returns a non-nil provider alongside an error (partial initialisation).
	// assemble must propagate the error and must not expose the partially-built
	// provider — the caller should not see a TracerProvider they cannot use.
	fakeTP := noop.NewTracerProvider()
	wantErr := errors.New("partial init failure")
	cfg := &config.Config{
		OtelTracingEnabled:   true,
		OtelExporterEndpoint: "otel.example.com:4317",
	}

	t.Run("nil-shutdown", func(t *testing.T) {
		partialCtor := func(_ string, _ bool, _ float64) (oteltrace.TracerProvider, func(context.Context) error, error) {
			return fakeTP, nil, wantErr
		}
		c, err := assemble(cfg, partialCtor)
		if !errors.Is(err, wantErr) {
			t.Errorf("error: want %v (or wrapping it), got %v", wantErr, err)
		}
		if c.TracerProvider != nil {
			t.Error("TracerProvider: want nil when ctor returns error")
		}
		if c.Shutdown == nil {
			t.Fatal("Shutdown: want non-nil no-op even on partial error")
		}
		if err := c.Shutdown(context.Background()); err != nil {
			t.Errorf("Shutdown on partial error: unexpected error %v", err)
		}
	})

	t.Run("non-nil-shutdown", func(t *testing.T) {
		// When ctor returns a non-nil cleanup func alongside the error, assemble
		// must call it to avoid leaking the partially-initialised provider.
		shutdownCalled := false
		partialCtor := func(_ string, _ bool, _ float64) (oteltrace.TracerProvider, func(context.Context) error, error) {
			return fakeTP, func(_ context.Context) error {
				shutdownCalled = true
				return nil
			}, wantErr
		}
		c, err := assemble(cfg, partialCtor)
		if !errors.Is(err, wantErr) {
			t.Errorf("error: want %v (or wrapping it), got %v", wantErr, err)
		}
		if c.TracerProvider != nil {
			t.Error("TracerProvider: want nil when ctor returns error")
		}
		if !shutdownCalled {
			t.Error("ctor cleanup func must be called when ctor returns non-nil shutdown with error")
		}
	})

	t.Run("non-nil-shutdown-with-cleanup-error", func(t *testing.T) {
		// When the cleanup func itself errors, assemble must join both the ctor
		// error and the cleanup error so callers see both failures.
		cleanupErr := errors.New("cleanup failed")
		shutdownCalled := false
		partialCtor := func(_ string, _ bool, _ float64) (oteltrace.TracerProvider, func(context.Context) error, error) {
			return fakeTP, func(_ context.Context) error {
				shutdownCalled = true
				return cleanupErr
			}, wantErr
		}
		c, err := assemble(cfg, partialCtor)
		if !errors.Is(err, wantErr) {
			t.Errorf("ctor error: want %v (or wrapping it), got %v", wantErr, err)
		}
		if !errors.Is(err, cleanupErr) {
			t.Errorf("cleanup error: want %v joined into returned error, got %v", cleanupErr, err)
		}
		if c.TracerProvider != nil {
			t.Error("TracerProvider: want nil when ctor returns error")
		}
		if !shutdownCalled {
			t.Error("ctor cleanup func must be called when ctor returns non-nil shutdown with error")
		}
	})
}

func TestAssemble_ShutdownIdempotency(t *testing.T) {
	t.Run("no-op", func(t *testing.T) {
		c, err := assemble(&config.Config{}, func(_ string, _ bool, _ float64) (oteltrace.TracerProvider, func(context.Context) error, error) {
			t.Helper()
			t.Fatalf("ctor must not be called when tracing is disabled")
			panic("unreachable")
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		for i := 0; i < 3; i++ {
			if err := c.Shutdown(context.Background()); err != nil {
				t.Errorf("no-op shutdown call %d: unexpected error %v", i+1, err)
			}
		}
	})

	t.Run("tracing-once-counted", func(t *testing.T) {
		// sync.Once ensures the provider delegate runs exactly once.
		callCount := 0
		mockCtor := func(_ string, _ bool, _ float64) (oteltrace.TracerProvider, func(context.Context) error, error) {
			return noop.NewTracerProvider(), func(_ context.Context) error {
				callCount++
				return nil
			}, nil
		}

		tc, err := assemble(&config.Config{
			OtelTracingEnabled:   true,
			OtelExporterEndpoint: "otel.example.com:4317",
		}, mockCtor)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		_ = tc.Shutdown(context.Background())
		_ = tc.Shutdown(context.Background())
		if callCount != 1 {
			t.Errorf("shutdown idempotency: want 1 call (sync.Once), got %d", callCount)
		}
	})

	t.Run("shutdown-error-cached", func(t *testing.T) {
		// When the provider shutdown returns an error, sync.Once caches it;
		// subsequent calls return the same error without re-invoking shutdown.
		wantErr := errors.New("provider shutdown failed")
		callCount := 0
		errShutdownCtor := func(_ string, _ bool, _ float64) (oteltrace.TracerProvider, func(context.Context) error, error) {
			return noop.NewTracerProvider(), func(_ context.Context) error {
				callCount++
				return wantErr
			}, nil
		}

		tc, err := assemble(&config.Config{
			OtelTracingEnabled:   true,
			OtelExporterEndpoint: "otel.example.com:4317",
		}, errShutdownCtor)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		err1 := tc.Shutdown(context.Background())
		err2 := tc.Shutdown(context.Background())
		if callCount != 1 {
			t.Errorf("shutdown delegate calls: want 1 (sync.Once), got %d", callCount)
		}
		if !errors.Is(err1, wantErr) {
			t.Errorf("first shutdown: want %v, got %v", wantErr, err1)
		}
		if !errors.Is(err2, wantErr) {
			t.Errorf("second shutdown: want error wrapping %v, got %v", wantErr, err2)
		}
	})

	t.Run("tracing-shutdown-calls-provider-delegate", func(t *testing.T) {
		providerShutdownRan := false
		shutdownCtor := func(_ string, _ bool, _ float64) (oteltrace.TracerProvider, func(context.Context) error, error) {
			return noop.NewTracerProvider(), func(_ context.Context) error {
				providerShutdownRan = true
				return nil
			}, nil
		}

		tc, err := assemble(&config.Config{
			OtelTracingEnabled:   true,
			OtelExporterEndpoint: "otel.example.com:4317",
		}, shutdownCtor)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if err := tc.Shutdown(context.Background()); err != nil {
			t.Errorf("unexpected shutdown error: %v", err)
		}
		if !providerShutdownRan {
			t.Error("provider shutdown must be called on Shutdown()")
		}
	})

	t.Run("concurrent-shutdown-race", func(t *testing.T) {
		// Verifies that concurrent Shutdown calls are race-free under -race and
		// that sync.Once ensures the delegate runs exactly once.
		callCount := 0
		var mu sync.Mutex
		mockCtor := func(_ string, _ bool, _ float64) (oteltrace.TracerProvider, func(context.Context) error, error) {
			return noop.NewTracerProvider(), func(_ context.Context) error {
				mu.Lock()
				callCount++
				mu.Unlock()
				return nil
			}, nil
		}
		tc, err := assemble(&config.Config{
			OtelTracingEnabled:   true,
			OtelExporterEndpoint: "otel.example.com:4317",
		}, mockCtor)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		var wg sync.WaitGroup
		for i := 0; i < 10; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_ = tc.Shutdown(context.Background())
			}()
		}
		wg.Wait()
		if callCount != 1 {
			t.Errorf("shutdown delegate calls: want 1 (sync.Once), got %d", callCount)
		}
	})

	t.Run("concurrent-panic", func(t *testing.T) {
		// Verifies that concurrent Shutdown calls where the delegate panics are
		// race-free under -race: sync.Once + recover() must ensure exactly one
		// panic is captured and all concurrent callers receive the same cached error.
		mockCtor := func(_ string, _ bool, _ float64) (oteltrace.TracerProvider, func(context.Context) error, error) {
			return noop.NewTracerProvider(), func(_ context.Context) error {
				panic("concurrent panic test")
			}, nil
		}
		tc, err := assemble(&config.Config{
			OtelTracingEnabled:   true,
			OtelExporterEndpoint: "otel.example.com:4317",
		}, mockCtor)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		const n = 10
		errs := make([]error, n)
		var wg sync.WaitGroup
		for i := 0; i < n; i++ {
			i := i
			wg.Add(1)
			go func() {
				defer wg.Done()
				errs[i] = tc.Shutdown(context.Background())
			}()
		}
		wg.Wait()
		var ref string
		for i, e := range errs {
			if e == nil {
				t.Errorf("goroutine %d: want non-nil error after panic, got nil", i)
				continue
			}
			if !strings.Contains(e.Error(), "panicked") {
				t.Errorf("goroutine %d: want error mentioning 'panicked', got %v", i, e)
			}
			if ref == "" {
				ref = e.Error()
			} else if e.Error() != ref {
				t.Errorf("goroutine %d: want same cached error message, got %q vs %q", i, e.Error(), ref)
			}
		}
	})
}

func TestAssemble_InvalidSampleRatio(t *testing.T) {
	// OtelSampleRatio must be in [0.0, 1.0]; values outside that range (including
	// NaN and Inf which slip through naive < 0 || > 1 guards) are rejected before
	// the ctor is called.
	invalid := []float64{-0.1, 1.1, 2.0, -1.0, math.NaN(), math.Inf(1), math.Inf(-1)}
	for _, ratio := range invalid {
		ratio := ratio
		cfg := &config.Config{
			OtelTracingEnabled:   true,
			OtelExporterEndpoint: "otel.example.com:4317",
			OtelSampleRatio:      ratio,
		}
		c, err := assemble(cfg, func(_ string, _ bool, _ float64) (oteltrace.TracerProvider, func(context.Context) error, error) {
			t.Helper()
			t.Fatalf("ctor must not be called for invalid sample ratio %v", ratio)
			panic("unreachable")
		})
		if err == nil {
			t.Errorf("OtelSampleRatio %v: want error for out-of-range ratio, got nil", ratio)
		} else if !strings.Contains(err.Error(), "OtelSampleRatio") {
			t.Errorf("OtelSampleRatio %v: error message should mention OtelSampleRatio, got %v", ratio, err)
		}
		if c.Shutdown == nil {
			t.Errorf("OtelSampleRatio %v: Shutdown must be non-nil no-op even on validation error", ratio)
		}
	}
}

func TestAssemble_EmptyEndpoint(t *testing.T) {
	// OtelExporterEndpoint must not be empty when tracing is enabled.
	cfg := &config.Config{
		OtelTracingEnabled:   true,
		OtelExporterEndpoint: "",
	}
	c, err := assemble(cfg, func(_ string, _ bool, _ float64) (oteltrace.TracerProvider, func(context.Context) error, error) {
		t.Helper()
		t.Fatal("ctor must not be called when endpoint is empty")
		panic("unreachable")
	})
	if err == nil {
		t.Error("want error for empty OtelExporterEndpoint when tracing enabled, got nil")
	} else if !strings.Contains(err.Error(), "OtelExporterEndpoint") {
		t.Errorf("error message: want mention of OtelExporterEndpoint, got %v", err)
	}
	if c.Shutdown == nil {
		t.Error("Shutdown: want non-nil no-op even on empty endpoint error")
	}
	if err := c.Shutdown(context.Background()); err != nil {
		t.Errorf("no-op shutdown on empty endpoint error: unexpected error %v", err)
	}
}

func TestAssemble_NegativeConfigRejected(t *testing.T) {
	// Negative retry/breaker counts are rejected. config.Load already validates
	// these, but assemble guards defensively for *config.Config values constructed
	// directly (e.g. in tests) to surface misconfiguration rather than silently
	// producing a disabled policy.
	noopCtor := func(_ string, _ bool, _ float64) (oteltrace.TracerProvider, func(context.Context) error, error) {
		t.Helper()
		t.Fatal("ctor must not be called")
		panic("unreachable")
	}

	t.Run("negative-retry", func(t *testing.T) {
		c, err := assemble(&config.Config{WorkerRetryMax: -1}, noopCtor)
		if err == nil {
			t.Error("want error for negative WorkerRetryMax, got nil")
		} else if !strings.Contains(err.Error(), "WorkerRetryMax") {
			t.Errorf("error message: want mention of WorkerRetryMax, got %v", err)
		}
		if c.Shutdown == nil {
			t.Error("Shutdown: want non-nil no-op even on validation error")
		}
	})

	t.Run("negative-breaker", func(t *testing.T) {
		c, err := assemble(&config.Config{WorkerBreakerThreshold: -1}, noopCtor)
		if err == nil {
			t.Error("want error for negative WorkerBreakerThreshold, got nil")
		} else if !strings.Contains(err.Error(), "WorkerBreakerThreshold") {
			t.Errorf("error message: want mention of WorkerBreakerThreshold, got %v", err)
		}
		if c.Shutdown == nil {
			t.Error("Shutdown: want non-nil no-op even on validation error")
		}
	})

	t.Run("cooldown-without-breaker", func(t *testing.T) {
		// A non-zero cooldown with threshold == 0 silently discards the cooldown
		// because the breaker is disabled — reject it explicitly.
		c, err := assemble(&config.Config{WorkerBreakerThreshold: 0, WorkerBreakerCooldownMS: 1000}, noopCtor)
		if err == nil {
			t.Error("want error when WorkerBreakerCooldownMS is non-zero with zero threshold, got nil")
		} else if !strings.Contains(err.Error(), "WorkerBreakerCooldownMS") {
			t.Errorf("error message: want mention of WorkerBreakerCooldownMS, got %v", err)
		}
		if c.Shutdown == nil {
			t.Error("Shutdown: want non-nil no-op even on validation error")
		}
	})
}

func TestAssemble_ZeroConfigTreatedAsDisabled(t *testing.T) {
	// Zero values for retry/breaker counts produce a zero-value policy (disabled),
	// matching the behaviour of an unconfigured gateway.
	cfg := &config.Config{
		WorkerRetryMax:         0,
		WorkerBreakerThreshold: 0,
	}
	c, err := assemble(cfg, func(_ string, _ bool, _ float64) (oteltrace.TracerProvider, func(context.Context) error, error) {
		t.Helper()
		t.Fatal("ctor must not be called when tracing is disabled")
		panic("unreachable")
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c.Policy.Retry.MaxAttempts != 0 {
		t.Errorf("Retry.MaxAttempts: want 0 for zero config, got %d", c.Policy.Retry.MaxAttempts)
	}
	if c.Policy.Breaker.Threshold != 0 {
		t.Errorf("Breaker.Threshold: want 0 for zero config, got %d", c.Policy.Breaker.Threshold)
	}
}

func TestAssemble_ZeroBreakerCooldown(t *testing.T) {
	// WorkerBreakerCooldownMS must be > 0 when WorkerBreakerThreshold > 0: a zero
	// cooldown would cause proxy.New to panic with "BreakerConfig.Cooldown must be > 0".
	cfg := &config.Config{
		WorkerBreakerThreshold:  1,
		WorkerBreakerCooldownMS: 0,
	}
	c, err := assemble(cfg, func(_ string, _ bool, _ float64) (oteltrace.TracerProvider, func(context.Context) error, error) {
		t.Helper()
		t.Fatalf("ctor must not be called when tracing is disabled")
		panic("unreachable")
	})
	if err == nil {
		t.Error("want error when WorkerBreakerCooldownMS is 0 with non-zero threshold, got nil")
	} else if !strings.Contains(err.Error(), "WorkerBreakerCooldownMS") {
		t.Errorf("error message: want mention of WorkerBreakerCooldownMS, got %v", err)
	}
	if c.Shutdown == nil {
		t.Error("Shutdown: want non-nil no-op even on validation error")
	}
}

func TestAssemble_AllEnabled(t *testing.T) {
	mockCtor := func(_ string, _ bool, _ float64) (oteltrace.TracerProvider, func(context.Context) error, error) {
		return noop.NewTracerProvider(), func(_ context.Context) error { return nil }, nil
	}

	cfg := &config.Config{
		WorkerRetryMax:          2,
		WorkerRetryBackoffMS:    100,
		WorkerBreakerThreshold:  3,
		WorkerBreakerCooldownMS: 1000,
		OtelTracingEnabled:      true,
		OtelExporterEndpoint:    "otel.example.com:4317",
		OtelSampleRatio:         1.0,
	}
	c, err := assemble(cfg, mockCtor)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c.Policy.Retry.MaxAttempts != 2 {
		t.Errorf("Retry.MaxAttempts: want 2, got %d", c.Policy.Retry.MaxAttempts)
	}
	if c.Policy.Retry.BaseBackoff != cfg.RetryBaseBackoff() {
		t.Errorf("Retry.BaseBackoff: want %v, got %v", cfg.RetryBaseBackoff(), c.Policy.Retry.BaseBackoff)
	}
	if c.Policy.Breaker.Threshold != 3 {
		t.Errorf("Breaker.Threshold: want 3, got %d", c.Policy.Breaker.Threshold)
	}
	if c.Policy.Breaker.Cooldown != cfg.BreakerCooldown() {
		t.Errorf("Breaker.Cooldown: want %v, got %v", cfg.BreakerCooldown(), c.Policy.Breaker.Cooldown)
	}
	if c.TracerProvider == nil {
		t.Error("TracerProvider: want non-nil")
	}
	if c.Shutdown == nil {
		t.Fatal("Shutdown: want non-nil")
	}
	if err := c.Shutdown(context.Background()); err != nil {
		t.Errorf("shutdown: unexpected error %v", err)
	}
}

func TestAssemble_PublicDelegation(t *testing.T) {
	// Verifies the public Assemble function correctly delegates to assemble by
	// exercising the no-tracing path, which requires no real OTLP server.
	// The delegation path for tracing is covered by the assemble+mock tests above.
	c, err := Assemble(&config.Config{
		WorkerRetryMax:       2,
		WorkerRetryBackoffMS: 50,
	})
	if err != nil {
		t.Fatalf("Assemble: unexpected error: %v", err)
	}
	if c.Policy.Retry.MaxAttempts != 2 {
		t.Errorf("Retry.MaxAttempts: want 2, got %d", c.Policy.Retry.MaxAttempts)
	}
	if c.TracerProvider != nil {
		t.Error("TracerProvider: want nil when tracing disabled")
	}
	if c.Shutdown == nil {
		t.Fatal("Shutdown: want non-nil no-op")
	}
	if err := c.Shutdown(context.Background()); err != nil {
		t.Errorf("no-op shutdown: unexpected error %v", err)
	}
}

func TestAssemble_NegativeBackoffCooldownRejected(t *testing.T) {
	// Negative millisecond values for backoff/cooldown produce negative time.Duration
	// values that can break downstream components. assemble rejects them defensively.
	noopCtor := func(_ string, _ bool, _ float64) (oteltrace.TracerProvider, func(context.Context) error, error) {
		t.Helper()
		t.Fatal("ctor must not be called")
		panic("unreachable")
	}

	t.Run("negative-retry-backoff", func(t *testing.T) {
		c, err := assemble(&config.Config{WorkerRetryMax: 2, WorkerRetryBackoffMS: -1}, noopCtor)
		if err == nil {
			t.Error("want error for negative WorkerRetryBackoffMS, got nil")
		} else if !strings.Contains(err.Error(), "WorkerRetryBackoffMS") {
			t.Errorf("error message: want mention of WorkerRetryBackoffMS, got %v", err)
		}
		if c.Shutdown == nil {
			t.Error("Shutdown: want non-nil no-op even on validation error")
		}
	})

	t.Run("negative-breaker-cooldown-disabled", func(t *testing.T) {
		// A negative WorkerBreakerCooldownMS with zero threshold is caught by the
		// "must be 0 when threshold is 0" guard (since -1 != 0).
		c, err := assemble(&config.Config{WorkerBreakerThreshold: 0, WorkerBreakerCooldownMS: -1}, noopCtor)
		if err == nil {
			t.Error("want error for negative WorkerBreakerCooldownMS with disabled breaker, got nil")
		} else if !strings.Contains(err.Error(), "WorkerBreakerCooldownMS") {
			t.Errorf("error message: want mention of WorkerBreakerCooldownMS, got %v", err)
		}
		if c.Shutdown == nil {
			t.Error("Shutdown: want non-nil no-op even on validation error")
		}
	})

	t.Run("negative-breaker-cooldown", func(t *testing.T) {
		c, err := assemble(&config.Config{WorkerBreakerThreshold: 3, WorkerBreakerCooldownMS: -1}, noopCtor)
		if err == nil {
			t.Error("want error for negative WorkerBreakerCooldownMS, got nil")
		} else if !strings.Contains(err.Error(), "WorkerBreakerCooldownMS") {
			t.Errorf("error message: want mention of WorkerBreakerCooldownMS, got %v", err)
		}
		if c.Shutdown == nil {
			t.Error("Shutdown: want non-nil no-op even on validation error")
		}
	})
}

func TestAssemble_ShutdownPanicRecovered(t *testing.T) {
	// If shutdownFn panics, recover() inside once.Do must capture the panic as
	// an error rather than leaving shutdownErr nil (which would silently hide
	// the failure on all subsequent Shutdown calls).
	panicCtor := func(_ string, _ bool, _ float64) (oteltrace.TracerProvider, func(context.Context) error, error) {
		return noop.NewTracerProvider(), func(_ context.Context) error {
			panic("simulated provider panic")
		}, nil
	}
	c, err := assemble(&config.Config{
		OtelTracingEnabled:   true,
		OtelExporterEndpoint: "otel.example.com:4317",
	}, panicCtor)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	err1 := c.Shutdown(context.Background())
	if err1 == nil {
		t.Error("Shutdown after panic: want non-nil error, got nil")
	} else if !strings.Contains(err1.Error(), "panicked") {
		t.Errorf("Shutdown after panic: want error mentioning panicked, got %v", err1)
	}
	// Second call must return the cached error, not re-panic.
	err2 := c.Shutdown(context.Background())
	if err2 == nil {
		t.Error("second Shutdown after panic: want cached non-nil error, got nil")
	}
	// sync.Once caches h.err; both calls must return the same error message.
	if err1 == nil || err2 == nil {
		t.Fatal("expected non-nil errors from both shutdown calls")
	}
	if err1.Error() != err2.Error() {
		t.Errorf("cached panic error: want same error message, got %q and %q", err1.Error(), err2.Error())
	}
}
