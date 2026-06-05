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
// must check ctx.Err() separately to avoid retrying after the caller has cancelled.
//
// status == 0 means a transport/network error occurred before an HTTP response
// arrived; this includes per-call http.Client.Timeout expiry, which is retryable
// because the caller's context may still be valid. status < 0 means a pre-flight
// build error that never reached the worker (not retryable).
func IsRetryable(err error, status int) bool {
	// Explicit caller cancellation is never retryable.
	if errors.Is(err, context.Canceled) {
		return false
	}
	// Pre-flight errors (e.g. request-build failure) never reached the worker.
	if status < 0 {
		return false
	}
	// Transport, network, or per-call HTTP client timeout — retryable unless the
	// caller's own context has since expired (checked separately in the retry loop).
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
	if cap > maxB {
		cap = maxB
	}

	// Equal jitter: uniform in [cap/2, cap] using cryptographically secure randomness
	// to prevent synchronized retry storms across multiple gateway instances.
	half := cap / 2
	var d time.Duration
	if half > 0 {
		n, err := rand.Int(rand.Reader, big.NewInt(int64(half)+1))
		if err != nil {
			d = half // theoretical fallback; crypto/rand.Int only errors on OS RNG failure
		} else {
			d = half + time.Duration(n.Int64())
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
