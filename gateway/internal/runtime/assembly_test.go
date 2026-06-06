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
	if err := c.Shutdown(context.Background()); err != nil {
		t.Errorf("no-op shutdown returned error: %v", err)
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
	_, err := Assemble(cfg)
	if !errors.Is(err, wantErr) {
		t.Errorf("error: want %v, got %v", wantErr, err)
	}
}

func TestAssemble_ShutdownIdempotency(t *testing.T) {
	// No-op shutdown is inherently idempotent.
	cfg := &config.Config{}
	c, err := Assemble(cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for i := 0; i < 3; i++ {
		if err := c.Shutdown(context.Background()); err != nil {
			t.Errorf("no-op shutdown call %d: unexpected error %v", i+1, err)
		}
	}

	// Tracing shutdown delegates to the provider's shutdown; verify no panic on
	// repeated calls (the underlying closure is called each time — idempotency
	// is the provider's responsibility, which the SDK guarantees).
	orig := tracerProviderConstructor
	t.Cleanup(func() { tracerProviderConstructor = orig })

	callCount := 0
	tracerProviderConstructor = func(_ string, _ bool, _ float64) (oteltrace.TracerProvider, func(context.Context) error, error) {
		shutdown := func(_ context.Context) error {
			callCount++
			return nil
		}
		return noop.NewTracerProvider(), shutdown, nil
	}

	tc, err := Assemble(&config.Config{
		OtelTracingEnabled:   true,
		OtelExporterEndpoint: "otel.example.com:4317",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	_ = tc.Shutdown(context.Background())
	_ = tc.Shutdown(context.Background())
	if callCount != 2 {
		t.Errorf("shutdown call count: want 2, got %d", callCount)
	}
}
