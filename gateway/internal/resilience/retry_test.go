package resilience

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestIsRetryable(t *testing.T) {
	cases := []struct {
		name   string
		err    error
		status int
		want   bool
	}{
		{"transport error", errors.New("connect refused"), 0, true},
		{"500", nil, 500, true},
		{"503", nil, 503, true},
		{"4xx not retried", nil, 400, false},
		{"200 not retried", nil, 200, false},
		// context.Canceled and context.DeadlineExceeded are never retryable — both
		// mean the caller no longer wants the result (or the per-call http.Client
		// timeout fired, in which case retrying the same slow worker just wastes time).
		{"context canceled", context.Canceled, 0, false},
		{"wraps canceled", fmt.Errorf("worker call: %w", context.Canceled), 0, false},
		{"deadline exceeded status 0", context.DeadlineExceeded, 0, false},
		{"wraps deadline status 0", fmt.Errorf("worker call: %w", context.DeadlineExceeded), 0, false},
		{"nil err zero status", nil, 0, false},
		{"pre-flight statusNone (-1)", errors.New("build request: bad url"), -1, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := IsRetryable(tc.err, tc.status)
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
	// base=10ms, max=40ms → caps: n=0→10ms, n=1→20ms, n=2→40ms, n=3→40ms (capped)
	// Equal jitter band [cap/2, cap]: n=0 in [5,10], n=1 in [10,20], n=2+ in [20,40].
	p := Policy{BaseBackoff: 10 * time.Millisecond, MaxBackoff: 40 * time.Millisecond}
	cases := []struct {
		n              int
		minFloor, maxCeil time.Duration
	}{
		{0, 2 * time.Millisecond, 30 * time.Millisecond},  // [5ms,10ms] + CI slack
		{1, 5 * time.Millisecond, 50 * time.Millisecond},  // [10ms,20ms] + CI slack
		{2, 10 * time.Millisecond, 80 * time.Millisecond}, // [20ms,40ms] + CI slack
		{3, 10 * time.Millisecond, 80 * time.Millisecond}, // capped — same range as n=2
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
