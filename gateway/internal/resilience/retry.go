// Package resilience provides retry and circuit-breaker policies for worker calls.
package resilience

import (
	"context"
	"crypto/rand"
	"errors"
	"math/big"
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

// IsRetryable reports whether a call outcome warrants a retry based on the error
// shape and HTTP status alone. It does NOT check caller context liveness — callers
// must check ctx.Err() separately as a belt-and-suspenders guard.
//
// status == 0 means a transport/network error occurred before an HTTP response
// arrived (connection refused, reset, etc.). status < 0 means a pre-flight build
// error that never reached the worker (not retryable).
func IsRetryable(err error, status int) bool {
	// Cancellation and deadline expiry are never retryable — both signal the caller
	// no longer wants the result. DeadlineExceeded covers both caller-context expiry
	// and per-call http.Client.Timeout; retrying after either just wastes resources.
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	// Pre-flight errors (e.g. request-build failure) never reached the worker.
	if status < 0 {
		return false
	}
	// Transport or network error (connection refused, reset, etc.) with no HTTP response.
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

	// Exponential cap: base * 2^(n-1), capped at maxB. n is small (max 10),
	// so the loop is at most 9 iterations; no overflow risk within that range.
	cap := base
	for i := 1; i < n; i++ {
		cap *= 2
		if cap >= maxB {
			cap = maxB
			break
		}
	}

	// Equal jitter: uniform in [cap/2, cap] using cryptographically secure randomness
	// to prevent synchronized retry storms across multiple gateway instances.
	half := cap / 2
	var d time.Duration
	if half > 0 {
		jitter, err := rand.Int(rand.Reader, big.NewInt(int64(half)+1))
		if err != nil {
			d = half // theoretical fallback; crypto/rand.Int only errors on OS RNG failure
		} else {
			d = half + time.Duration(jitter.Int64())
		}
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
