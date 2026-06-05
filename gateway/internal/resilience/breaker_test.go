package resilience

import (
	"errors"
	"sync"
	"testing"
	"time"
)

func TestBreaker_Disabled(t *testing.T) {
	b := NewBreaker(BreakerConfig{Threshold: 0}, nil)
	for i := 0; i < 10; i++ {
		b.RecordFailure()
		if err := b.Allow(); err != nil {
			t.Fatalf("disabled breaker Allow() = %v, want nil", err)
		}
	}
	if b.CurrentState() != StateClosed {
		t.Errorf("disabled breaker state = %v, want StateClosed", b.CurrentState())
	}
}

func TestBreaker_NilSafe(t *testing.T) {
	var b *Breaker
	if err := b.Allow(); err != nil {
		t.Fatalf("nil Breaker.Allow() = %v, want nil", err)
	}
	b.RecordSuccess()
	b.RecordFailure()
	if b.CurrentState() != StateClosed {
		t.Errorf("nil Breaker.CurrentState() = %v, want StateClosed", b.CurrentState())
	}
}

func TestBreaker_OpenAfterThreshold(t *testing.T) {
	b := NewBreaker(BreakerConfig{Threshold: 3, Cooldown: time.Minute}, nil)

	for i := 0; i < 2; i++ {
		b.RecordFailure()
		if b.CurrentState() != StateClosed {
			t.Fatalf("after %d failures want StateClosed, got %v", i+1, b.CurrentState())
		}
		if err := b.Allow(); err != nil {
			t.Fatalf("Allow() after %d failures = %v, want nil", i+1, err)
		}
	}

	b.RecordFailure() // 3rd failure — must open
	if b.CurrentState() != StateOpen {
		t.Fatalf("after threshold want StateOpen, got %v", b.CurrentState())
	}
}

func TestBreaker_FastFailWhileOpen(t *testing.T) {
	b := NewBreaker(BreakerConfig{Threshold: 1, Cooldown: time.Hour}, nil)
	b.RecordFailure()

	for i := 0; i < 5; i++ {
		if err := b.Allow(); !errors.Is(err, ErrBreakerOpen) {
			t.Fatalf("Allow() = %v, want ErrBreakerOpen while open", err)
		}
	}
}

func TestBreaker_HalfOpenAfterCooldown(t *testing.T) {
	now := time.Now()
	b := NewBreaker(BreakerConfig{Threshold: 1, Cooldown: time.Second}, nil).
		WithNow(func() time.Time { return now })

	b.RecordFailure()

	if err := b.Allow(); !errors.Is(err, ErrBreakerOpen) {
		t.Fatal("expected ErrBreakerOpen before cooldown")
	}

	// Advance past cooldown.
	now = now.Add(2 * time.Second)

	if err := b.Allow(); err != nil {
		t.Fatalf("Allow() after cooldown = %v, want nil (probe)", err)
	}
	if b.CurrentState() != StateHalfOpen {
		t.Fatalf("want StateHalfOpen after cooldown probe, got %v", b.CurrentState())
	}

	// A second concurrent caller should still be blocked.
	if err := b.Allow(); !errors.Is(err, ErrBreakerOpen) {
		t.Fatalf("concurrent Allow() during probe = %v, want ErrBreakerOpen", err)
	}
}

func TestBreaker_ClosesOnSuccessfulProbe(t *testing.T) {
	now := time.Now()
	var transitions []State
	b := NewBreaker(BreakerConfig{Threshold: 1, Cooldown: time.Second}, func(s State) {
		transitions = append(transitions, s)
	}).WithNow(func() time.Time { return now })

	b.RecordFailure()         // → StateOpen
	now = now.Add(2 * time.Second)
	if err := b.Allow(); err != nil { // probe → StateHalfOpen
		t.Fatalf("Allow(): %v", err)
	}
	b.RecordSuccess() // → StateClosed

	if b.CurrentState() != StateClosed {
		t.Fatalf("want StateClosed after successful probe, got %v", b.CurrentState())
	}
	want := []State{StateOpen, StateHalfOpen, StateClosed}
	if len(transitions) != len(want) {
		t.Fatalf("transitions = %v, want %v", transitions, want)
	}
	for i, s := range want {
		if transitions[i] != s {
			t.Errorf("transitions[%d] = %v, want %v", i, transitions[i], s)
		}
	}
	// Verify breaker accepts calls after closing.
	if err := b.Allow(); err != nil {
		t.Fatalf("Allow() after close = %v, want nil", err)
	}
}

func TestBreaker_ReopensOnFailedProbe(t *testing.T) {
	now := time.Now()
	b := NewBreaker(BreakerConfig{Threshold: 1, Cooldown: time.Second}, nil).
		WithNow(func() time.Time { return now })

	b.RecordFailure()
	now = now.Add(2 * time.Second)
	b.Allow()         // probe
	b.RecordFailure() // probe failed → re-open

	if b.CurrentState() != StateOpen {
		t.Fatalf("want StateOpen after failed probe, got %v", b.CurrentState())
	}
	if err := b.Allow(); !errors.Is(err, ErrBreakerOpen) {
		t.Fatalf("Allow() after re-open = %v, want ErrBreakerOpen", err)
	}
}

func TestBreaker_SuccessResetsFailureCount(t *testing.T) {
	b := NewBreaker(BreakerConfig{Threshold: 3, Cooldown: time.Minute}, nil)
	b.RecordFailure()
	b.RecordFailure()
	b.RecordSuccess() // resets failure count
	b.RecordFailure()
	b.RecordFailure()
	// Still only 2 failures after the reset, should still be closed.
	if b.CurrentState() != StateClosed {
		t.Fatalf("want StateClosed (count was reset), got %v", b.CurrentState())
	}
}

func TestBreaker_RecordSuccessFromOpenDoesNotClose(t *testing.T) {
	// A success from an in-flight request that was admitted while the breaker was closed
	// must not close the breaker once it has since opened (e.g. due to concurrent failures).
	// Crucially, the stale success must release the probeInFlight slot so a future probe
	// can proceed after cooldown — otherwise the breaker is permanently stuck open.
	now := time.Now()
	b := NewBreaker(BreakerConfig{Threshold: 1, Cooldown: time.Second}, nil).
		WithNow(func() time.Time { return now })
	b.RecordFailure() // → StateOpen
	if b.CurrentState() != StateOpen {
		t.Fatal("want StateOpen after threshold failure")
	}
	b.RecordSuccess() // stale in-flight success; must NOT close
	if b.CurrentState() != StateOpen {
		t.Fatalf("RecordSuccess from Open closed breaker; want StateOpen")
	}
	// After cooldown the probe slot must be free for a fresh probe.
	now = now.Add(2 * time.Second)
	if err := b.Allow(); err != nil {
		t.Fatalf("Allow() after stale success + cooldown = %v, want nil (fresh probe)", err)
	}
}

func TestBreaker_RecordAbortReleasesProbeWithoutClosing(t *testing.T) {
	now := time.Now()
	b := NewBreaker(BreakerConfig{Threshold: 1, Cooldown: time.Second}, nil).
		WithNow(func() time.Time { return now })

	b.RecordFailure()        // → StateOpen
	now = now.Add(2 * time.Second) // past cooldown
	if err := b.Allow(); err != nil { // → StateHalfOpen, probeInFlight = true
		t.Fatalf("Allow(): %v", err)
	}
	b.RecordAbort() // caller cancelled probe; release slot without health signal

	// Breaker must stay HalfOpen — no recovery was confirmed.
	if b.CurrentState() != StateHalfOpen {
		t.Fatalf("want StateHalfOpen after abort, got %v", b.CurrentState())
	}
	// Probe slot must be free so the next request can attempt a fresh probe.
	if err := b.Allow(); err != nil {
		t.Fatalf("Allow() after abort = %v, want nil (fresh probe available)", err)
	}
}

func TestBreaker_RaceConcurrent(t *testing.T) {
	// Each goroutine records one failure. With 100 goroutines and Threshold=5,
	// the breaker must open; verify it did so correctly after the storm.
	b := NewBreaker(BreakerConfig{Threshold: 5, Cooldown: 50 * time.Millisecond}, nil)
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := b.Allow(); err == nil {
				b.RecordFailure()
			}
		}()
	}
	wg.Wait()

	// After >= Threshold concurrent failures the breaker must be open.
	if got := b.CurrentState(); got != StateOpen {
		t.Fatalf("after concurrent failures: state = %v, want StateOpen", got)
	}
	if err := b.Allow(); !errors.Is(err, ErrBreakerOpen) {
		t.Fatalf("Allow() = %v, want ErrBreakerOpen", err)
	}
}
