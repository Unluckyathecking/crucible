package resilience

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestIsRetryable(t *testing.T) {
	bg := context.Background()
	cancelled, cancel := context.WithCancel(context.Background())
	cancel() // immediately cancel so cancelled.Err() == context.Canceled

	cases := []struct {
		name   string
		ctx    context.Context
		err    error
		status int
		want   bool
	}{
		{"transport error live ctx", bg, errors.New("connect refused"), 0, true},
		{"500 live ctx", bg, nil, 500, true},
		{"503 live ctx", bg, nil, 503, true},
		// 4xx is never retried — a real HTTP response arrived; the worker is reachable.
		{"4xx nil err", bg, nil, 400, false},
		{"4xx with err", bg, fmt.Errorf("worker returned status 400: bad request"), 400, false},
		{"200 not retried", bg, nil, 200, false},
		{"200 decode error", bg, fmt.Errorf("decode worker response: unexpected EOF"), 200, false},
		// context.Canceled / context.DeadlineExceeded are never retryable.
		{"context canceled in err", bg, context.Canceled, 0, false},
		{"wraps canceled in err", bg, fmt.Errorf("worker call: %w", context.Canceled), 0, false},
		{"deadline exceeded in err", bg, context.DeadlineExceeded, 0, false},
		{"wraps deadline in err", bg, fmt.Errorf("worker call: %w", context.DeadlineExceeded), 0, false},
		// Cancelled context short-circuits even for otherwise-retryable transport errors.
		{"cancelled ctx transport error", cancelled, errors.New("connect refused"), 0, false},
		{"cancelled ctx 500", cancelled, nil, 500, false},
		{"nil err zero status", bg, nil, 0, false},
		{"pre-flight statusNone (-1)", bg, errors.New("build request: bad url"), -1, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := IsRetryable(tc.ctx, tc.err, tc.status)
			if got != tc.want {
				t.Errorf("IsRetryable(%v, %d) = %v, want %v", tc.err, tc.status, got, tc.want)
			}
		})
	}
}

func TestPolicy_Sleep_HappyPath(t *testing.T) {
	p := Policy{MaxAttempts: 3, BaseBackoff: 1 * time.Millisecond, MaxBackoff: 5 * time.Millisecond}
	// n=0: first retry, base delay.
	if err := p.Sleep(context.Background(), 0); err != nil {
		t.Fatalf("Sleep: %v", err)
	}
}

func TestPolicy_Sleep_ContextExpired(t *testing.T) {
	p := Policy{MaxAttempts: 3, BaseBackoff: 200 * time.Millisecond, MaxBackoff: 5 * time.Second}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	start := time.Now()
	err := p.Sleep(ctx, 0)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("Sleep: expected error when ctx expires, got nil")
	}
	if !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled) {
		t.Errorf("Sleep returned %v, want context error", err)
	}
	// Context expires after 20ms; Sleep must return promptly (well under 100ms).
	if elapsed > 100*time.Millisecond {
		t.Errorf("Sleep took %v; should have returned promptly on ctx expiry", elapsed)
	}
}

func TestPolicy_Sleep_DefaultsApplied(t *testing.T) {
	// Zero Policy still produces a valid (small) backoff.
	p := Policy{}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	// n=0: first retry with default base (100ms), well within the 2s context.
	if err := p.Sleep(ctx, 0); err != nil {
		t.Fatalf("Sleep with zero Policy: %v", err)
	}
}

// TestPolicy_Sleep_BackoffDoublingAndCap verifies that the cap doubles each retry
// up to MaxBackoff and that sleep durations fall in the [backoffCap/2, backoffCap]
// jitter band with generous CI margins.
func TestPolicy_Sleep_BackoffDoublingAndCap(t *testing.T) {
	if testing.Short() {
		t.Skip("timing test skipped in short mode")
	}
	// n is 0-indexed: n=0 means "first retry" (sleep before the 2nd attempt),
	// n=1 means "second retry", etc.
	// base=10ms, max=40ms → caps: n=0→10ms, n=1→20ms, n=2→40ms, n=3→40ms (capped)
	// Equal jitter band [cap/2, cap]: n=0 in [5,10], n=1 in [10,20], n=2+ in [20,40].
	p := Policy{BaseBackoff: 10 * time.Millisecond, MaxBackoff: 40 * time.Millisecond}
	cases := []struct {
		n              int
		minFloor, maxCeil time.Duration
	}{
		{0, 4 * time.Millisecond, 20 * time.Millisecond},  // first retry: [5ms,10ms] + CI slack
		{1, 8 * time.Millisecond, 35 * time.Millisecond},  // second retry: [10ms,20ms] + CI slack
		{2, 15 * time.Millisecond, 60 * time.Millisecond}, // third retry: [20ms,40ms] + CI slack
		{3, 15 * time.Millisecond, 60 * time.Millisecond}, // fourth retry: capped at max
	}
	for _, tc := range cases {
		start := time.Now()
		if err := p.Sleep(context.Background(), tc.n); err != nil {
			t.Fatalf("n=%d Sleep: %v", tc.n, err)
		}
		elapsed := time.Since(start)
		if elapsed < tc.minFloor {
			t.Errorf("n=%d: elapsed %v below floor %v (jitter should be >= cap/2)", tc.n, elapsed, tc.minFloor)
		}
		if elapsed > tc.maxCeil {
			t.Errorf("n=%d: elapsed %v above ceil %v (exceeds MaxBackoff)", tc.n, elapsed, tc.maxCeil)
		}
	}
}

// TestPolicy_Sleep_CeilingDoubling verifies the ceiling computation directly:
// ceilingFor must return base*2^n capped at MaxBackoff, and the doubling
// relationship must hold exactly between consecutive retry indices. This is a
// timing-free check that the exponential algorithm is correct regardless of jitter.
func TestPolicy_Sleep_CeilingDoubling(t *testing.T) {
	cases := []struct {
		base, maxB time.Duration
		n          int
		want       time.Duration
	}{
		{10 * time.Millisecond, 40 * time.Millisecond, 0, 10 * time.Millisecond},
		{10 * time.Millisecond, 40 * time.Millisecond, 1, 20 * time.Millisecond}, // doubled from n=0
		{10 * time.Millisecond, 40 * time.Millisecond, 2, 40 * time.Millisecond}, // doubled from n=1
		{10 * time.Millisecond, 40 * time.Millisecond, 3, 40 * time.Millisecond}, // capped at maxB
		{10 * time.Second, 5 * time.Millisecond, 0, 5 * time.Millisecond},         // base > max → clamped
		{1 * time.Millisecond, 5 * time.Millisecond, 100, 5 * time.Millisecond},   // large n stays capped
	}
	for _, tc := range cases {
		if got := ceilingFor(tc.base, tc.maxB, tc.n); got != tc.want {
			t.Errorf("ceilingFor(base=%v,max=%v,n=%d) = %v, want %v",
				tc.base, tc.maxB, tc.n, got, tc.want)
		}
	}
	// Verify exact doubling relationship for n=0→1 and n=1→2.
	c0 := ceilingFor(10*time.Millisecond, 40*time.Millisecond, 0)
	c1 := ceilingFor(10*time.Millisecond, 40*time.Millisecond, 1)
	c2 := ceilingFor(10*time.Millisecond, 40*time.Millisecond, 2)
	if c1 != c0*2 {
		t.Errorf("ceiling must double n=0→n=1: %v → %v", c0, c1)
	}
	if c2 != c1*2 {
		t.Errorf("ceiling must double n=1→n=2: %v → %v", c1, c2)
	}
}

// TestPolicy_Sleep_BaseLargerThanMax verifies that when BaseBackoff > MaxBackoff,
// Sleep clamps the ceiling to MaxBackoff on the first retry rather than sleeping
// for the full BaseBackoff. This exercises the documented overflow guard:
// "Clamp base to maxB first so a BaseBackoff > MaxBackoff doesn't exceed the ceiling."
func TestPolicy_Sleep_BaseLargerThanMax(t *testing.T) {
	p := Policy{BaseBackoff: 10 * time.Second, MaxBackoff: 5 * time.Millisecond}
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	start := time.Now()
	if err := p.Sleep(ctx, 0); err != nil {
		t.Fatalf("Sleep: %v", err)
	}
	elapsed := time.Since(start)
	if elapsed > 100*time.Millisecond {
		t.Errorf("Sleep took %v; base > max should be capped at MaxBackoff (%v), not BaseBackoff", elapsed, p.MaxBackoff)
	}
}

func TestPolicy_Sleep_CapBoundary(t *testing.T) {
	// Verify the overflow guard and MaxBackoff cap logic with large n.
	// With base=1ms and maxB=5ms, cap should hit maxB by n=2 (1→2→4ms, capped at 5).
	p := Policy{BaseBackoff: 1 * time.Millisecond, MaxBackoff: 5 * time.Millisecond}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	// Large n should not overflow and should complete quickly (cap is 5ms).
	for _, n := range []int{10, 50, 100, 1000} {
		if err := p.Sleep(ctx, n); err != nil {
			t.Fatalf("Sleep(ctx, %d): %v", n, err)
		}
	}
}
