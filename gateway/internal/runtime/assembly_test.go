package runtime

import (
	"context"
	"errors"
	"testing"
	"time"

	oteltrace "go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"

	"github.com/Unluckyathecking/crucible/gateway/internal/config"
)

func TestAssemble_DefaultOff(t *testing.T) {
	cfg := &config.Config{}
	c, err := assemble(cfg, func(_ string, _ bool, _ float64) (oteltrace.TracerProvider, func(context.Context) error, error) {
		t.Fatal("ctor must not be called when tracing is disabled")
		return nil, nil, nil
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
	// Call twice to verify no-op idempotency.
	for i := 0; i < 2; i++ {
		if err := c.Shutdown(context.Background()); err != nil {
			t.Errorf("no-op shutdown call %d: unexpected error %v", i+1, err)
		}
	}
}

func TestAssemble_NilConfig(t *testing.T) {
	_, err := Assemble(nil)
	if err == nil {
		t.Error("want error for nil config, got nil")
	}
}

func TestAssemble_RetryEnabled(t *testing.T) {
	cfg := &config.Config{
		WorkerRetryMax:       3,
		WorkerRetryBackoffMS: 200,
	}
	c, err := assemble(cfg, func(_ string, _ bool, _ float64) (oteltrace.TracerProvider, func(context.Context) error, error) {
		t.Fatal("ctor must not be called when tracing is disabled")
		return nil, nil, nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c.Policy.Retry.MaxAttempts != 3 {
		t.Errorf("MaxAttempts: want 3, got %d", c.Policy.Retry.MaxAttempts)
	}
	if c.Policy.Retry.BaseBackoff != 200*time.Millisecond {
		t.Errorf("BaseBackoff: want 200ms, got %v", c.Policy.Retry.BaseBackoff)
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
		t.Fatal("ctor must not be called when tracing is disabled")
		return nil, nil, nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c.Policy.Breaker.Threshold != 5 {
		t.Errorf("Threshold: want 5, got %d", c.Policy.Breaker.Threshold)
	}
	if c.Policy.Breaker.Cooldown != 3*time.Second {
		t.Errorf("Cooldown: want 3s, got %v", c.Policy.Breaker.Cooldown)
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
		t.Fatal("ctor must not be called when tracing is disabled")
		return nil, nil, nil
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
	if c.TracerProvider == nil {
		t.Error("TracerProvider: want non-nil")
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
		t.Error("Shutdown: want non-nil no-op even on provider error")
	}
}

func TestAssemble_TracingPartialError(t *testing.T) {
	// ctor returns a non-nil provider alongside an error (partial initialisation).
	// assemble must propagate the error and must not expose the partially-built
	// provider — the caller should not see a TracerProvider they cannot use.
	fakeTP := noop.NewTracerProvider()
	wantErr := errors.New("partial init failure")
	partialCtor := func(_ string, _ bool, _ float64) (oteltrace.TracerProvider, func(context.Context) error, error) {
		return fakeTP, nil, wantErr
	}
	cfg := &config.Config{
		OtelTracingEnabled:   true,
		OtelExporterEndpoint: "otel.example.com:4317",
	}
	c, err := assemble(cfg, partialCtor)
	if !errors.Is(err, wantErr) {
		t.Errorf("error: want %v (or wrapping it), got %v", wantErr, err)
	}
	if c.TracerProvider != nil {
		t.Error("TracerProvider: want nil when ctor returns error")
	}
	if c.Shutdown == nil {
		t.Error("Shutdown: want non-nil no-op even on partial error")
	}
}

func TestAssemble_ShutdownIdempotency(t *testing.T) {
	t.Run("no-op", func(t *testing.T) {
		c, err := assemble(&config.Config{}, func(_ string, _ bool, _ float64) (oteltrace.TracerProvider, func(context.Context) error, error) {
			t.Fatal("ctor must not be called when tracing is disabled")
			return nil, nil, nil
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
		errShutdownCtor := func(_ string, _ bool, _ float64) (oteltrace.TracerProvider, func(context.Context) error, error) {
			return noop.NewTracerProvider(), func(_ context.Context) error {
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
}

func TestAssemble_ZeroBreakerCooldown(t *testing.T) {
	// When WorkerBreakerCooldownMS is zero, BreakerCooldown() returns zero;
	// assemble must accept it rather than treat it as an error.
	cfg := &config.Config{
		WorkerBreakerThreshold:  1,
		WorkerBreakerCooldownMS: 0,
	}
	c, err := assemble(cfg, func(_ string, _ bool, _ float64) (oteltrace.TracerProvider, func(context.Context) error, error) {
		t.Fatal("ctor must not be called when tracing is disabled")
		return nil, nil, nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c.Policy.Breaker.Threshold != 1 {
		t.Errorf("Threshold: want 1, got %d", c.Policy.Breaker.Threshold)
	}
	if c.Policy.Breaker.Cooldown != 0 {
		t.Errorf("Cooldown: want 0 when config helper returns zero, got %v", c.Policy.Breaker.Cooldown)
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
	if c.Policy.Retry.BaseBackoff != 100*time.Millisecond {
		t.Errorf("Retry.BaseBackoff: want 100ms, got %v", c.Policy.Retry.BaseBackoff)
	}
	if c.Policy.Breaker.Threshold != 3 {
		t.Errorf("Breaker.Threshold: want 3, got %d", c.Policy.Breaker.Threshold)
	}
	if c.Policy.Breaker.Cooldown != time.Second {
		t.Errorf("Breaker.Cooldown: want 1s, got %v", c.Policy.Breaker.Cooldown)
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
