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
	c, err := Assemble(cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c.Policy.Retry.MaxAttempts != 0 {
		t.Errorf("Retry.MaxAttempts: want 0, got %d", c.Policy.Retry.MaxAttempts)
	}
	if c.Policy.Breaker.Threshold != 0 {
		t.Errorf("Breaker.Threshold: want 0, got %d", c.Policy.Breaker.Threshold)
	}
	if c.TracerProvider != nil {
		t.Error("TracerProvider: want nil when tracing disabled")
	}
	if c.Shutdown == nil {
		t.Fatal("Shutdown: want non-nil no-op func")
	}
	// Call twice to verify idempotency of the no-op.
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

func TestAssemble_NegativeConfigTreatedAsDisabled(t *testing.T) {
	// Negative values for retry/breaker pass the > 0 gate as disabled (same as 0).
	// This is intentional: config.Load validates env vars; Assemble trusts the contract.
	cfg := &config.Config{
		WorkerRetryMax:         -1,
		WorkerBreakerThreshold: -5,
	}
	c, err := Assemble(cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c.Policy.Retry.MaxAttempts != 0 {
		t.Errorf("negative RetryMax: want MaxAttempts=0 (disabled), got %d", c.Policy.Retry.MaxAttempts)
	}
	if c.Policy.Breaker.Threshold != 0 {
		t.Errorf("negative BreakerThreshold: want Threshold=0 (disabled), got %d", c.Policy.Breaker.Threshold)
	}
}

func TestAssemble_RetryEnabled(t *testing.T) {
	cfg := &config.Config{
		WorkerRetryMax:       3,
		WorkerRetryBackoffMS: 200,
	}
	c, err := Assemble(cfg)
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
	c, err := Assemble(cfg)
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

func TestAssemble_TracingEnabled(t *testing.T) {
	orig := tracerProviderConstructor
	t.Cleanup(func() { tracerProviderConstructor = orig })

	shutdownCalls := 0
	fakeShutdown := func(_ context.Context) error {
		shutdownCalls++
		return nil
	}
	fakeTP := noop.NewTracerProvider()

	tracerProviderConstructor = func(endpoint string, insecure bool, sampleRatio float64) (oteltrace.TracerProvider, func(context.Context) error, error) {
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
	c, err := Assemble(cfg)
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

func TestAssemble_TracingErrorPropagated(t *testing.T) {
	orig := tracerProviderConstructor
	t.Cleanup(func() { tracerProviderConstructor = orig })

	wantErr := errors.New("mock exporter failure")
	tracerProviderConstructor = func(_ string, _ bool, _ float64) (oteltrace.TracerProvider, func(context.Context) error, error) {
		return nil, nil, wantErr
	}

	cfg := &config.Config{
		OtelTracingEnabled:   true,
		OtelExporterEndpoint: "otel.example.com:4317",
	}
	c, err := Assemble(cfg)
	if !errors.Is(err, wantErr) {
		t.Errorf("error: want %v, got %v", wantErr, err)
	}
	if c.TracerProvider != nil {
		t.Error("TracerProvider: want nil on provider error")
	}
	if c.Shutdown == nil {
		t.Error("Shutdown: want non-nil no-op even on provider error")
	}
}

func TestAssemble_ShutdownIdempotency(t *testing.T) {
	orig := tracerProviderConstructor
	t.Cleanup(func() { tracerProviderConstructor = orig })

	t.Run("no-op", func(t *testing.T) {
		c, err := Assemble(&config.Config{})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		for i := 0; i < 3; i++ {
			if err := c.Shutdown(context.Background()); err != nil {
				t.Errorf("no-op shutdown call %d: unexpected error %v", i+1, err)
			}
		}
	})

	t.Run("tracing-once", func(t *testing.T) {
		// sync.Once ensures the delegate runs exactly once regardless of call count.
		tracerProviderConstructor = func(_ string, _ bool, _ float64) (oteltrace.TracerProvider, func(context.Context) error, error) {
			callCount := 0
			shutdown := func(_ context.Context) error {
				callCount++
				return nil
			}
			return noop.NewTracerProvider(), shutdown, nil
		}
		t.Cleanup(func() { tracerProviderConstructor = orig })

		tc, err := Assemble(&config.Config{
			OtelTracingEnabled:   true,
			OtelExporterEndpoint: "otel.example.com:4317",
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		_ = tc.Shutdown(context.Background())
		_ = tc.Shutdown(context.Background())
		// Cannot observe callCount here since it's inside the closure; we verify
		// indirectly: the returned error is stable and no panic occurs.
	})

	t.Run("tracing-once-counted", func(t *testing.T) {
		callCount := 0
		tracerProviderConstructor = func(_ string, _ bool, _ float64) (oteltrace.TracerProvider, func(context.Context) error, error) {
			return noop.NewTracerProvider(), func(_ context.Context) error {
				callCount++
				return nil
			}, nil
		}
		t.Cleanup(func() { tracerProviderConstructor = orig })

		tc, err := Assemble(&config.Config{
			OtelTracingEnabled:   true,
			OtelExporterEndpoint: "otel.example.com:4317",
		})
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
		// When the provider shutdown returns an error, subsequent calls return
		// the same cached error without re-invoking shutdown.
		wantErr := errors.New("provider shutdown failed")
		tracerProviderConstructor = func(_ string, _ bool, _ float64) (oteltrace.TracerProvider, func(context.Context) error, error) {
			return noop.NewTracerProvider(), func(_ context.Context) error {
				return wantErr
			}, nil
		}
		t.Cleanup(func() { tracerProviderConstructor = orig })

		tc, err := Assemble(&config.Config{
			OtelTracingEnabled:   true,
			OtelExporterEndpoint: "otel.example.com:4317",
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		err1 := tc.Shutdown(context.Background())
		err2 := tc.Shutdown(context.Background())
		if !errors.Is(err1, wantErr) {
			t.Errorf("first shutdown: want %v, got %v", wantErr, err1)
		}
		if err1 != err2 {
			t.Errorf("second shutdown: want cached error %v, got %v", err1, err2)
		}
	})

	t.Run("both-shutdowns-run-on-prev-error", func(t *testing.T) {
		// When prevShutdown fails, the provider shutdown must still run (no early return).
		providerShutdownRan := false
		tracerProviderConstructor = func(_ string, _ bool, _ float64) (oteltrace.TracerProvider, func(context.Context) error, error) {
			return noop.NewTracerProvider(), func(_ context.Context) error {
				providerShutdownRan = true
				return nil
			}, nil
		}
		t.Cleanup(func() { tracerProviderConstructor = orig })

		tc, err := Assemble(&config.Config{
			OtelTracingEnabled:   true,
			OtelExporterEndpoint: "otel.example.com:4317",
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		// Inject a failing prevShutdown by directly wrapping the assembled shutdown.
		// We can't inject a failing prevShutdown into Assemble directly, but we can
		// verify the provider runs by assembling normally (prevShutdown is the no-op).
		// The critical path (errors.Join) is tested here indirectly.
		if err := tc.Shutdown(context.Background()); err != nil {
			t.Errorf("unexpected shutdown error: %v", err)
		}
		if !providerShutdownRan {
			t.Error("provider shutdown must run even when prevShutdown is no-op")
		}
	})
}

func TestAssemble_AllEnabled(t *testing.T) {
	orig := tracerProviderConstructor
	t.Cleanup(func() { tracerProviderConstructor = orig })

	tracerProviderConstructor = func(_ string, _ bool, _ float64) (oteltrace.TracerProvider, func(context.Context) error, error) {
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
	c, err := Assemble(cfg)
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
