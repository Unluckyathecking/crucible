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
	// Transport/network error (no HTTP response) or HTTP 5xx server error.
	return (err != nil && status == 0) || status >= 500
}

// Sleep waits for the jittered exponential backoff before retry n.
// n is 0-indexed: 0 = first retry (base delay), 1 = second retry (base*2), etc.
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

	// Exponential cap: base * 2^n, capped at maxB.
	// Clamp base to maxB first so a BaseBackoff > MaxBackoff doesn't exceed ceiling
	// on the first retry. Then double once per retry up to maxB.
	backoffCap := base
	if backoffCap > maxB {
		backoffCap = maxB
	}
	for i := 0; i < n; i++ {
		if backoffCap >= maxB {
			break
		}
		backoffCap *= 2
		if backoffCap > maxB {
			backoffCap = maxB
		}
	}

	// Equal jitter: uniform in [backoffCap/2, backoffCap] using cryptographically secure
	// randomness to prevent synchronized retry storms across multiple gateway instances.
	half := backoffCap / 2
	var d time.Duration
	if half > 0 {
		jitter, err := rand.Int(rand.Reader, big.NewInt(int64(half)+1))
		if err != nil {
			d = half // fallback on OS RNG failure; preserves partial desynchronization
		} else {
			d = half + time.Duration(jitter.Int64())
		}
	} else {
		d = backoffCap
	}

	t := time.NewTimer(d)
	select {
	case <-ctx.Done():
		// Go 1.23+: Stop() drains t.C before returning, so no manual drain needed.
		// The old "if !t.Stop() { <-t.C }" pattern blocks forever in Go 1.23+ because
		// Stop already empties the channel.
		t.Stop()
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
