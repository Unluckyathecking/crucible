package tracing_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Unluckyathecking/crucible/gateway/internal/tracing"
)

// TestNewProviderReturnsWorkingProvider verifies the happy path: NewProvider with a
// syntactically valid (though unreachable) endpoint returns a non-nil TracerProvider
// and shutdown function with no error, and the shutdown function completes cleanly.
func TestNewProviderReturnsWorkingProvider(t *testing.T) {
	// otlptracehttp connects lazily — construction succeeds without a live collector.
	tp, shutdown, err := tracing.NewProvider("localhost:4318", true, 1.0)
	if err != nil {
		t.Fatalf("NewProvider returned unexpected error: %v", err)
	}
	if tp == nil {
		t.Fatal("NewProvider returned nil TracerProvider")
	}
	if shutdown == nil {
		t.Fatal("NewProvider returned nil shutdown function")
	}

	// t.Cleanup runs shutdown with a fresh context after the test body completes,
	// avoiding any ambiguity about context cancellation order.
	t.Cleanup(func() {
		shutCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := shutdown(shutCtx); err != nil {
			t.Errorf("shutdown returned unexpected error: %v", err)
		}
	})
}

// TestNewProviderSampleRatioZero verifies that a sample ratio of 0 is accepted and
// that root spans created by the resulting provider are not sampled.
func TestNewProviderSampleRatioZero(t *testing.T) {
	tp, shutdown, err := tracing.NewProvider("localhost:4318", true, 0.0)
	if err != nil {
		t.Fatalf("NewProvider(ratio=0) returned unexpected error: %v", err)
	}

	// t.Cleanup ensures shutdown runs with a fresh context that is not derived from
	// any outer context, eliminating any defer ordering ambiguity.
	t.Cleanup(func() {
		shutCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := shutdown(shutCtx); err != nil {
			t.Errorf("shutdown returned unexpected error: %v", err)
		}
	})

	// ParentBased(TraceIDRatioBased(0)) drops all root spans.
	_, span := tp.Tracer("test").Start(context.Background(), "test.span")
	if span.SpanContext().IsSampled() {
		t.Error("ratio=0 provider must not sample root spans")
	}
	span.End()
}

// TestNewProviderSampleRatioOne verifies that a sample ratio of 1.0 is accepted and
// that every root span created by the resulting provider is sampled.
func TestNewProviderSampleRatioOne(t *testing.T) {
	tp, shutdown, err := tracing.NewProvider("localhost:4318", true, 1.0)
	if err != nil {
		t.Fatalf("NewProvider(ratio=1) returned unexpected error: %v", err)
	}
	t.Cleanup(func() {
		shutCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := shutdown(shutCtx); err != nil {
			t.Errorf("shutdown returned unexpected error: %v", err)
		}
	})

	// ParentBased(TraceIDRatioBased(1)) samples all root spans.
	_, span := tp.Tracer("test").Start(context.Background(), "test.span")
	if !span.SpanContext().IsSampled() {
		t.Error("ratio=1 provider must sample all root spans")
	}
	span.End()
}

// TestNewProviderShutdownWithCancelledContext verifies that calling the shutdown
// function with an already-cancelled context returns a context error promptly — within
// 2 s — rather than blocking until batchExportTimeout. This guards against a
// process-exit hang when the caller accidentally passes an already-done context.
func TestNewProviderShutdownWithCancelledContext(t *testing.T) {
	_, shutdown, err := tracing.NewProvider("localhost:4318", true, 1.0)
	if err != nil {
		t.Fatalf("NewProvider returned unexpected error: %v", err)
	}

	// Cancel the context before calling shutdown — the BSP must honour it and return
	// quickly with a context error rather than waiting for the full export timeout.
	cancelledCtx, cancel := context.WithCancel(context.Background())
	cancel()

	start := time.Now()
	done := make(chan error, 1)
	go func() { done <- shutdown(cancelledCtx) }()

	select {
	case err := <-done:
		if err == nil {
			t.Error("shutdown with cancelled context should return an error, got nil")
		} else if !errors.Is(err, context.Canceled) {
			t.Errorf("expected context.Canceled error, got %v", err)
		}
		if elapsed := time.Since(start); elapsed > 2*time.Second {
			t.Errorf("shutdown took %v; expected return within 2 s for cancelled context", elapsed)
		}
	case <-time.After(5 * time.Second):
		t.Error("shutdown with cancelled context blocked for > 5 s; expected prompt return")
	}
}
