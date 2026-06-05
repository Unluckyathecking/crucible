package tracing_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Unluckyathecking/crucible/gateway/internal/tracing"
)

// TestNewProviderEmptyEndpointReturnsError verifies that NewProvider rejects an empty
// endpoint immediately rather than constructing an exporter that silently fails at
// export time. This protects callers that bypass config.Load validation.
func TestNewProviderEmptyEndpointReturnsError(t *testing.T) {
	t.Parallel()
	_, _, err := tracing.NewProvider("", false, 1.0)
	if err == nil {
		t.Fatal("NewProvider with empty endpoint should return an error, got nil")
	}
}

// TestNewProviderReturnsWorkingProvider verifies the happy path: NewProvider with a
// syntactically valid (though unreachable) endpoint returns a non-nil TracerProvider
// and shutdown function with no error, and the shutdown function completes cleanly.
func TestNewProviderReturnsWorkingProvider(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
	tp, shutdown, err := tracing.NewProvider("localhost:4318", true, 0.0)
	if err != nil {
		t.Fatalf("NewProvider(ratio=0) returned unexpected error: %v", err)
	}

	// Guard against nil shutdown in case NewProvider returned early with a nil func.
	if shutdown == nil {
		t.Fatal("NewProvider(ratio=0) returned nil shutdown function")
	}
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
	t.Parallel()
	tp, shutdown, err := tracing.NewProvider("localhost:4318", true, 1.0)
	if err != nil {
		t.Fatalf("NewProvider(ratio=1) returned unexpected error: %v", err)
	}
	if shutdown == nil {
		t.Fatal("NewProvider(ratio=1) returned nil shutdown function")
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
	t.Parallel()
	_, shutdown, err := tracing.NewProvider("localhost:4318", true, 1.0)
	if err != nil {
		t.Fatalf("NewProvider returned unexpected error: %v", err)
	}
	// Ensure the provider is properly shut down after the test even when the
	// cancelled-context call does not fully flush the BatchSpanProcessor.
	t.Cleanup(func() {
		if shutdown == nil {
			return
		}
		cleanCtx, cleanCancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cleanCancel()
		_ = shutdown(cleanCtx)
	})

	// Cancel the context before calling shutdown — the BSP must honour it and return
	// quickly with a context error rather than waiting for the full export timeout.
	cancelledCtx, cancel := context.WithCancel(context.Background())
	cancel()

	start := time.Now()
	errc := make(chan error, 1)
	go func() { errc <- shutdown(cancelledCtx) }()

	select {
	case err := <-errc:
		if err == nil {
			t.Error("shutdown with cancelled context should return an error, got nil")
		} else if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("expected context.Canceled or context.DeadlineExceeded error, got %v", err)
		}
		if elapsed := time.Since(start); elapsed > 2*time.Second {
			t.Errorf("shutdown took %v; expected return within 2 s for cancelled context", elapsed)
		}
	case <-time.After(5 * time.Second):
		t.Error("shutdown with cancelled context blocked for > 5 s; expected prompt return")
		// Drain the buffered channel so the spawned goroutine can exit cleanly.
		// cancelledCtx is already cancelled so shutdown will return once the BSP
		// honours it; the drain below prevents the goroutine from outliving the test.
		<-errc
	}
}
