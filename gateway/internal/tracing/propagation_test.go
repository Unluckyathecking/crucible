package tracing_test

import (
	"context"
	"strings"
	"testing"

	oteltrace "go.opentelemetry.io/otel/trace"

	"github.com/Unluckyathecking/crucible/gateway/internal/tracing"
)

// TestCaptureRestoreRoundTrip verifies that a traceparent captured from an
// active span, then restored into a fresh context, produces a span whose
// trace ID matches the original and whose parent span ID is the original
// span — the exact capture-at-enqueue / restore-at-execute round trip
// jobs.Executor and webhookout.Emitter depend on to keep one continuous
// trace across the async outbox boundary.
func TestCaptureRestoreRoundTrip(t *testing.T) {
	t.Parallel()
	tp, sr := newTestProvider(t)

	parentCtx, parentSpan := tp.Tracer("test").Start(context.Background(), "parent")
	parentSC := parentSpan.SpanContext()
	parentSpan.End()

	tv := tracing.CaptureTraceparent(parentCtx)
	if tv == "" {
		t.Fatal("CaptureTraceparent returned empty string for an active span")
	}

	restoredCtx := tracing.RestoreTraceparent(context.Background(), tv)
	childCtx, childSpan := tp.Tracer("test").Start(restoredCtx, "child")
	childSC := childSpan.SpanContext()
	childSpan.End()
	_ = childCtx

	if childSC.TraceID() != parentSC.TraceID() {
		t.Errorf("child trace ID = %s, want %s (round trip must continue the same trace)", childSC.TraceID(), parentSC.TraceID())
	}

	var found bool
	for _, s := range sr.Ended() {
		if s.Name() == "child" {
			found = true
			if s.Parent().SpanID() != parentSC.SpanID() {
				t.Errorf("child parent span ID = %s, want %s", s.Parent().SpanID(), parentSC.SpanID())
			}
		}
	}
	if !found {
		t.Fatal("child span was not recorded")
	}
}

// TestCaptureTraceparentAbsentSpanReturnsEmpty verifies that capturing from a
// context with no active span (the default-off / untraced path) is a no-op
// that returns "" rather than fabricating a value — the disabled-path
// contract callers (jobs.Store.Enqueue, webhookout.Emitter.Emit) rely on to
// persist NULL instead of a meaningless empty string.
func TestCaptureTraceparentAbsentSpanReturnsEmpty(t *testing.T) {
	t.Parallel()
	if got := tracing.CaptureTraceparent(context.Background()); got != "" {
		t.Errorf("CaptureTraceparent on a span-less context = %q, want \"\"", got)
	}
}

// TestRestoreTraceparentEmptyIsNoOp verifies that restoring an empty
// traceparent (the disabled-path / never-captured case) returns the input
// context unchanged rather than producing a span context of any kind.
func TestRestoreTraceparentEmptyIsNoOp(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	got := tracing.RestoreTraceparent(ctx, "")
	if got != ctx {
		t.Error("RestoreTraceparent(ctx, \"\") must return ctx unchanged")
	}
	if oteltrace.SpanContextFromContext(got).IsValid() {
		t.Error("RestoreTraceparent(ctx, \"\") must not produce a valid span context")
	}
}

// TestRestoreTraceparentMalformedNeverPanicsAndIsNoOp verifies that a
// malformed traceparent string is rejected by the underlying propagator
// without panicking, and that the returned context carries no remote span —
// the same tolerance Middleware already applies to a malformed inbound
// header, extended to values read back from a durable outbox row.
func TestRestoreTraceparentMalformedNeverPanicsAndIsNoOp(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("RestoreTraceparent panicked on malformed input: %v", r)
		}
	}()

	malformed := strings.Repeat("x", 55)
	got := tracing.RestoreTraceparent(context.Background(), malformed)
	if oteltrace.SpanContextFromContext(got).IsValid() {
		t.Error("RestoreTraceparent with malformed input must not produce a valid span context")
	}
}
