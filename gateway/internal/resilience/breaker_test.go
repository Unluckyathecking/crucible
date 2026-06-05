package resilience

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestBreaker_ZeroCooldownPanics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("NewBreaker with Threshold>0 and Cooldown=0 should panic")
		}
	}()
	NewBreaker(BreakerConfig{Threshold: 1, Cooldown: 0}, nil)
}

func TestBreaker_Disabled(t *testing.T) {
	b := NewBreaker(BreakerConfig{Threshold: 0}, nil)
	for i := 0; i < 10; i++ {
		b.RecordFailure(0)
		if _, err := b.Allow(); err != nil {
			t.Fatalf("disabled breaker Allow() = %v, want nil", err)
		}
	}
	if b.CurrentState() != StateClosed {
		t.Errorf("disabled breaker state = %v, want StateClosed", b.CurrentState())
	}
}

func TestBreaker_NilSafe(t *testing.T) {
	var b *Breaker
	if _, err := b.Allow(); err != nil {
		t.Fatalf("nil Breaker.Allow() = %v, want nil", err)
	}
	b.RecordSuccess(0)
	b.RecordFailure(0)
	if b.CurrentState() != StateClosed {
		t.Errorf("nil Breaker.CurrentState() = %v, want StateClosed", b.CurrentState())
	}
}

func TestBreaker_OpenAfterThreshold(t *testing.T) {
	b := NewBreaker(BreakerConfig{Threshold: 3, Cooldown: time.Minute}, nil)

	for i := 0; i < 2; i++ {
		b.RecordFailure(0)
		if b.CurrentState() != StateClosed {
			t.Fatalf("after %d failures want StateClosed, got %v", i+1, b.CurrentState())
		}
		if _, err := b.Allow(); err != nil {
			t.Fatalf("Allow() after %d failures = %v, want nil", i+1, err)
		}
	}

	b.RecordFailure(0) // 3rd failure — must open
	if b.CurrentState() != StateOpen {
		t.Fatalf("after threshold want StateOpen, got %v", b.CurrentState())
	}
}

func TestBreaker_FastFailWhileOpen(t *testing.T) {
	b := NewBreaker(BreakerConfig{Threshold: 1, Cooldown: time.Hour}, nil)
	b.RecordFailure(0)

	for i := 0; i < 5; i++ {
		if _, err := b.Allow(); !errors.Is(err, ErrBreakerOpen) {
			t.Fatalf("Allow() = %v, want ErrBreakerOpen while open", err)
		}
	}
}

func TestBreaker_HalfOpenAfterCooldown(t *testing.T) {
	now := time.Now()
	b := NewBreaker(BreakerConfig{Threshold: 1, Cooldown: time.Second}, nil).
		WithNow(func() time.Time { return now })

	b.RecordFailure(0)

	if _, err := b.Allow(); !errors.Is(err, ErrBreakerOpen) {
		t.Fatal("expected ErrBreakerOpen before cooldown")
	}

	// Advance past cooldown.
	now = now.Add(2 * time.Second)

	tok, err := b.Allow()
	if err != nil {
		t.Fatalf("Allow() after cooldown = %v, want nil (probe)", err)
	}
	if tok == 0 {
		t.Fatal("probe Allow() returned token 0, want non-zero generation token")
	}
	if b.CurrentState() != StateHalfOpen {
		t.Fatalf("want StateHalfOpen after cooldown probe, got %v", b.CurrentState())
	}

	// A second concurrent caller should still be blocked.
	if _, err := b.Allow(); !errors.Is(err, ErrBreakerOpen) {
		t.Fatalf("concurrent Allow() during probe = %v, want ErrBreakerOpen", err)
	}
}

func TestBreaker_ClosesOnSuccessfulProbe(t *testing.T) {
	now := time.Now()
	var transitions []State
	b := NewBreaker(BreakerConfig{Threshold: 1, Cooldown: time.Second}, func(s State) {
		transitions = append(transitions, s)
	}).WithNow(func() time.Time { return now })

	b.RecordFailure(0)        // → StateOpen
	now = now.Add(2 * time.Second)
	tok, err := b.Allow()     // probe → StateHalfOpen
	if err != nil {
		t.Fatalf("Allow(): %v", err)
	}
	b.RecordSuccess(tok) // → StateClosed

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
	if _, err := b.Allow(); err != nil {
		t.Fatalf("Allow() after close = %v, want nil", err)
	}
}

func TestBreaker_ReopensOnFailedProbe(t *testing.T) {
	now := time.Now()
	b := NewBreaker(BreakerConfig{Threshold: 1, Cooldown: time.Second}, nil).
		WithNow(func() time.Time { return now })

	b.RecordFailure(0)
	now = now.Add(2 * time.Second)
	tok, _ := b.Allow()    // probe
	b.RecordFailure(tok)   // probe failed → re-open

	if b.CurrentState() != StateOpen {
		t.Fatalf("want StateOpen after failed probe, got %v", b.CurrentState())
	}
	if _, err := b.Allow(); !errors.Is(err, ErrBreakerOpen) {
		t.Fatalf("Allow() after re-open = %v, want ErrBreakerOpen", err)
	}
}

func TestBreaker_SuccessResetsFailureCount(t *testing.T) {
	b := NewBreaker(BreakerConfig{Threshold: 3, Cooldown: time.Minute}, nil)
	b.RecordFailure(0)
	b.RecordFailure(0)
	b.RecordSuccess(0) // resets failure count
	b.RecordFailure(0)
	b.RecordFailure(0)
	// Still only 2 failures after the reset, should still be closed.
	if b.CurrentState() != StateClosed {
		t.Fatalf("want StateClosed (count was reset), got %v", b.CurrentState())
	}
}

func TestBreaker_RecordSuccessFromOpenDoesNotClose(t *testing.T) {
	// Simulate a request admitted while the breaker is Closed; concurrent failures then open
	// the breaker before the request completes. The stale RecordSuccess must not close the
	// breaker, and must not interfere with a future probe.
	now := time.Now()
	b := NewBreaker(BreakerConfig{Threshold: 1, Cooldown: time.Second}, nil).
		WithNow(func() time.Time { return now })
	staleTok, err := b.Allow() // request admitted from Closed state (token == 0)
	if err != nil {
		t.Fatal("Allow():", err)
	}
	b.RecordFailure(0) // concurrent failure opens the breaker while the request is in flight
	if b.CurrentState() != StateOpen {
		t.Fatal("want StateOpen after threshold failure")
	}
	b.RecordSuccess(staleTok) // stale success from the earlier Allow(); must NOT close
	if b.CurrentState() != StateOpen {
		t.Fatalf("RecordSuccess from Open closed breaker; want StateOpen")
	}
	// After cooldown the probe slot must be free for a fresh probe.
	now = now.Add(2 * time.Second)
	if _, err := b.Allow(); err != nil {
		t.Fatalf("Allow() after stale success + cooldown = %v, want nil (fresh probe)", err)
	}
}

func TestBreaker_RecordAbortReleasesProbeWithoutClosing(t *testing.T) {
	now := time.Now()
	b := NewBreaker(BreakerConfig{Threshold: 1, Cooldown: time.Second}, nil).
		WithNow(func() time.Time { return now })

	b.RecordFailure(0)             // → StateOpen
	now = now.Add(2 * time.Second) // past cooldown
	tok, err := b.Allow()          // → StateHalfOpen, probeInFlight = true
	if err != nil {
		t.Fatalf("Allow(): %v", err)
	}
	b.RecordAbort(tok) // caller cancelled probe; release slot without health signal

	// Breaker must stay HalfOpen — no recovery was confirmed.
	if b.CurrentState() != StateHalfOpen {
		t.Fatalf("want StateHalfOpen after abort, got %v", b.CurrentState())
	}
	// Probe slot must be free so the next request can attempt a fresh probe.
	if _, err := b.Allow(); err != nil {
		t.Fatalf("Allow() after abort = %v, want nil (fresh probe available)", err)
	}
}

// TestBreaker_StaleSuccessIgnoredInHalfOpen verifies that a stale RecordSuccess from
// a StateClosed-admitted request that arrives while the breaker is already HalfOpen does
// NOT close the breaker. The generation token prevents the stale result from being applied.
func TestBreaker_StaleSuccessIgnoredInHalfOpen(t *testing.T) {
	now := time.Now()
	b := NewBreaker(BreakerConfig{Threshold: 1, Cooldown: time.Second}, nil).
		WithNow(func() time.Time { return now })

	// A stale request admitted from StateClosed (will complete after the probe window).
	// StateClosed calls return token 0.
	staleTok, err := b.Allow()
	if err != nil {
		t.Fatal("Allow() stale:", err)
	}
	if staleTok != 0 {
		t.Fatalf("StateClosed Allow() returned non-zero token %d, want 0", staleTok)
	}

	// One failure opens the breaker (Threshold=1).
	b.RecordFailure(0)
	if b.CurrentState() != StateOpen {
		t.Fatal("want StateOpen after threshold failure")
	}

	// Advance past cooldown so Allow() transitions to HalfOpen for a probe.
	now = now.Add(2 * time.Second)
	probeTok, err := b.Allow()
	if err != nil {
		t.Fatal("probe Allow():", err)
	}
	if probeTok == 0 {
		t.Fatal("probe Allow() returned token 0, want non-zero generation token")
	}
	if b.CurrentState() != StateHalfOpen {
		t.Fatal("want StateHalfOpen before stale success")
	}

	// Stale RecordSuccess from the earlier StateClosed call must be ignored.
	b.RecordSuccess(staleTok) // token 0 != probeGen, so this is a no-op
	if b.CurrentState() != StateHalfOpen {
		t.Fatal("want StateHalfOpen — stale success must not close the breaker")
	}

	// The real probe's success correctly closes the breaker.
	b.RecordSuccess(probeTok)
	if b.CurrentState() != StateClosed {
		t.Fatal("want StateClosed after real probe success")
	}
}

// TestBreaker_StaleAbortIgnoredInHalfOpen verifies that a stale RecordAbort from a
// StateClosed-admitted request does not release the active probe's slot.
func TestBreaker_StaleAbortIgnoredInHalfOpen(t *testing.T) {
	now := time.Now()
	b := NewBreaker(BreakerConfig{Threshold: 1, Cooldown: time.Second}, nil).
		WithNow(func() time.Time { return now })

	// Stale request from StateClosed.
	staleTok, err := b.Allow()
	if err != nil {
		t.Fatal("Allow() stale:", err)
	}

	b.RecordFailure(0) // open
	now = now.Add(2 * time.Second)
	probeTok, err := b.Allow() // probe → HalfOpen
	if err != nil {
		t.Fatal("probe Allow():", err)
	}

	// Stale RecordAbort must not clear the active probe slot.
	b.RecordAbort(staleTok)
	if b.CurrentState() != StateHalfOpen {
		t.Fatalf("want StateHalfOpen after stale abort, got %v", b.CurrentState())
	}
	// Probe slot must still be in-flight; a concurrent Allow() must be blocked.
	if _, err := b.Allow(); !errors.Is(err, ErrBreakerOpen) {
		t.Fatalf("Allow() after stale abort = %v, want ErrBreakerOpen (probe still in-flight)", err)
	}

	// Real probe succeeds → closes breaker.
	b.RecordSuccess(probeTok)
	if b.CurrentState() != StateClosed {
		t.Fatal("want StateClosed after real probe success")
	}
}

// TestBreaker_StaleFailureIgnoredInHalfOpen verifies that a stale RecordFailure from a
// StateClosed-admitted request does not re-open the breaker while a newer probe is active.
func TestBreaker_StaleFailureIgnoredInHalfOpen(t *testing.T) {
	now := time.Now()
	b := NewBreaker(BreakerConfig{Threshold: 1, Cooldown: time.Second}, nil).
		WithNow(func() time.Time { return now })

	staleTok, err := b.Allow()
	if err != nil {
		t.Fatal("Allow() stale:", err)
	}

	b.RecordFailure(0) // open
	now = now.Add(2 * time.Second)
	probeTok, err := b.Allow() // probe → HalfOpen
	if err != nil {
		t.Fatal("probe Allow():", err)
	}

	// Stale failure must not re-open the breaker or reset the cooldown.
	b.RecordFailure(staleTok)
	if b.CurrentState() != StateHalfOpen {
		t.Fatalf("want StateHalfOpen after stale failure, got %v", b.CurrentState())
	}

	// Real probe succeeds → closes breaker normally.
	b.RecordSuccess(probeTok)
	if b.CurrentState() != StateClosed {
		t.Fatal("want StateClosed after real probe success")
	}
}

// TestBreaker_ExactCooldownBoundary verifies that Allow() uses strict Before for
// the openUntil check: at exactly the cooldown deadline (now == openUntil) the
// condition now.Before(openUntil) is false, so the probe IS admitted. One
// nanosecond before the deadline the probe must still be blocked.
func TestBreaker_ExactCooldownBoundary(t *testing.T) {
	base := time.Now()
	cooldown := time.Second
	openUntil := base.Add(cooldown)
	var now time.Time
	b := NewBreaker(BreakerConfig{Threshold: 1, Cooldown: cooldown}, nil).
		WithNow(func() time.Time { return now })

	now = base
	b.RecordFailure(0) // → StateOpen; openUntil = base + 1s

	// One nanosecond before the deadline: still blocked.
	now = openUntil.Add(-time.Nanosecond)
	if _, err := b.Allow(); !errors.Is(err, ErrBreakerOpen) {
		t.Fatal("expected ErrBreakerOpen 1ns before cooldown expires")
	}

	// Exactly at the deadline: probe admitted (not Before → else branch).
	now = openUntil
	tok, err := b.Allow()
	if err != nil {
		t.Fatalf("Allow() at exact cooldown boundary = %v, want nil (probe admitted)", err)
	}
	if tok == 0 {
		t.Fatal("probe token == 0, want non-zero generation token")
	}
	if b.CurrentState() != StateHalfOpen {
		t.Fatalf("state = %v, want StateHalfOpen at exact boundary", b.CurrentState())
	}
}

func TestBreaker_RaceConcurrent(t *testing.T) {
	// Each goroutine records one failure. With 100 goroutines and Threshold=5,
	// the breaker must open; verify it did so correctly after the storm.
	// Cooldown is 5 minutes so it cannot expire while the 100 goroutines are
	// still running (which would allow a probe and potentially close the breaker,
	// making the final StateOpen assertion non-deterministic).
	b := NewBreaker(BreakerConfig{Threshold: 5, Cooldown: 5 * time.Minute}, nil)
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// tok (not 0) is passed to RecordFailure to exercise the full
			// token-matching protocol under concurrency, not just the failure counter.
			if tok, err := b.Allow(); err == nil {
				b.RecordFailure(tok)
			}
		}()
	}
	wg.Wait()

	// After >= Threshold concurrent failures the breaker must be open.
	if got := b.CurrentState(); got != StateOpen {
		t.Fatalf("after concurrent failures: state = %v, want StateOpen", got)
	}
	if _, err := b.Allow(); !errors.Is(err, ErrBreakerOpen) {
		t.Fatalf("Allow() = %v, want ErrBreakerOpen", err)
	}
}

func TestBreaker_RaceConcurrentHalfOpenProbe(t *testing.T) {
	// Open the breaker, advance past cooldown, then race N goroutines through Allow().
	// Exactly 1 must be admitted as the probe; the rest must get ErrBreakerOpen.
	// The probe then succeeds and the breaker must close.
	now := time.Now()
	b := NewBreaker(BreakerConfig{Threshold: 1, Cooldown: time.Second}, nil).
		WithNow(func() time.Time { return now })

	b.RecordFailure(0)             // → StateOpen
	now = now.Add(2 * time.Second) // past cooldown

	const n = 50
	var admitted, blocked atomic.Int32
	var probeToken atomic.Uint64
	ready := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-ready
			if tok, err := b.Allow(); err == nil {
				admitted.Add(1)
				probeToken.Store(tok)
			} else {
				blocked.Add(1)
			}
		}()
	}
	close(ready) // release all goroutines simultaneously
	wg.Wait()

	if got := admitted.Load(); got != 1 {
		t.Fatalf("admitted = %d, want 1 (only one probe slot in half-open)", got)
	}
	if got := blocked.Load(); int(got) != n-1 {
		t.Fatalf("blocked = %d, want %d", got, n-1)
	}
	if b.CurrentState() != StateHalfOpen {
		t.Fatalf("state = %v, want StateHalfOpen before probe resolution", b.CurrentState())
	}

	// Successful probe must close the breaker.
	b.RecordSuccess(probeToken.Load())
	if b.CurrentState() != StateClosed {
		t.Fatalf("state = %v, want StateClosed after successful probe", b.CurrentState())
	}
}
