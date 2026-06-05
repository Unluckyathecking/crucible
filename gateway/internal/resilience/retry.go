// Package resilience provides retry and circuit-breaker policies for worker calls.
package resilience

import (
	"context"
	"errors"
	"math/rand"
	"time"
)

// Policy controls retry behaviour for worker calls.
// Zero value (MaxAttempts == 0) disables retries, preserving today's single-shot behaviour.
type Policy struct {
	// MaxAttempts is total attempts including the first call; <= 1 means single-shot.
	MaxAttempts int
	// BaseBackoff is the starting backoff before the first retry. Defaults to 100ms.
	BaseBackoff time.Duration
	// MaxBackoff caps exponential growth. Defaults to 5s.
	MaxBackoff time.Duration
}

// IsRetryable reports whether a call outcome warrants a retry.
// status == 0 means the error occurred before any HTTP response arrived (transport failure).
// Context cancellation / deadline errors are never retried — the caller is already gone.
func IsRetryable(err error, status int) bool {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	// Transport or network error before HTTP response.
	if err != nil && status == 0 {
		return true
	}
	return status >= 500
}

// Sleep waits for the jittered exponential backoff before attempt n (1 = before 2nd try).
// Returns ctx.Err() if the context expires during the wait.
func (p Policy) Sleep(ctx context.Context, n int) error {
	base := p.BaseBackoff
	if base <= 0 {
		base = 100 * time.Millisecond
	}
	maxB := p.MaxBackoff
	if maxB <= 0 {
		maxB = 5 * time.Second
	}

	// Exponential cap: base * 2^(n-1), capped at maxB.
	cap := base
	for i := 1; i < n; i++ {
		cap *= 2
		if cap >= maxB {
			cap = maxB
			break
		}
	}
	if cap > maxB {
		cap = maxB
	}

	// Equal jitter: uniform in [cap/2, cap].
	half := cap / 2
	var d time.Duration
	if half > 0 {
		d = half + time.Duration(rand.Int63n(int64(half)+1))
	} else {
		d = cap
	}

	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
