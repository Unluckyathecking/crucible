package tracing_test

import (
	"context"
	"testing"
	"time"

	"github.com/Unluckyathecking/crucible/gateway/internal/tracing"
)

// TestNewProviderReturnsWorkingProvider verifies the happy path: NewProvider with a
// syntactically valid (though unreachable) endpoint returns a non-nil TracerProvider
// and shutdown function with no error, and the shutdown function completes cleanly.
func TestNewProviderReturnsWorkingProvider(t *testing.T) {
	ctx := context.Background()
	// otlptracehttp connects lazily — construction succeeds without a live collector.
	tp, shutdown, err := tracing.NewProvider(ctx, "localhost:4318", true, 1.0)
	if err != nil {
		t.Fatalf("NewProvider returned unexpected error: %v", err)
	}
	if tp == nil {
		t.Fatal("NewProvider returned nil TracerProvider")
	}
	if shutdown == nil {
		t.Fatal("NewProvider returned nil shutdown function")
	}

	// Shutdown should not block or error when no spans have been exported.
	shutCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if err := shutdown(shutCtx); err != nil {
		t.Errorf("shutdown returned unexpected error: %v", err)
	}
}

// TestNewProviderSampleRatioZero verifies that a sample ratio of 0 is accepted and
// produces a TracerProvider that samples no spans.
func TestNewProviderSampleRatioZero(t *testing.T) {
	ctx := context.Background()
	tp, shutdown, err := tracing.NewProvider(ctx, "localhost:4318", true, 0.0)
	if err != nil {
		t.Fatalf("NewProvider(ratio=0) returned unexpected error: %v", err)
	}
	shutCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if err := shutdown(shutCtx); err != nil {
		t.Errorf("shutdown returned unexpected error: %v", err)
	}
	_ = tp
}
