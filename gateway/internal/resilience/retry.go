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
// status == 0 with a non-nil error means a transport/network error occurred before
// an HTTP response arrived (connection refused, reset, etc.) — this is retryable.
// status == 0 with a nil error is not retryable (should not occur in practice).
// status < 0 means a pre-flight build error that never reached the worker (not retryable).
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
	// HTTP 5xx: server-side failure, safe to retry.
	if status >= 500 {
		return true
	}
	// Transport/network error: no HTTP response arrived (conn refused, reset, etc.).
	// status == 0 signals this; status in [1, 499] means a real HTTP response arrived
	// (even 4xx), which is not retryable.
	if err != nil && status == 0 {
		return true
	}
	return false
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

	// Exponential ceiling: base * 2^n, capped at maxB.
	// Clamp base to maxB first so a BaseBackoff > MaxBackoff doesn't exceed the ceiling
	// on the first retry. Then double once per retry up to maxB.
	ceiling := base
	if ceiling > maxB {
		ceiling = maxB
	}
	for i := 0; i < n; i++ {
		if ceiling >= maxB {
			break
		}
		ceiling *= 2
		if ceiling > maxB {
			ceiling = maxB
		}
	}

	// Equal jitter: uniform in [ceiling/2, ceiling] using crypto/rand — required to
	// prevent synchronized retry storms when multiple gateway instances retry together.
	// math/rand must NOT be used here even though it is seeded automatically in Go 1.20+:
	// its PRNG output is predictable with enough observations, breaking desynchronization.
	half := ceiling / 2
	var d time.Duration
	if half > 0 {
		jitter, err := rand.Int(rand.Reader, big.NewInt(int64(half)+1))
		if err != nil {
			d = half // fallback on OS RNG failure; preserves partial desynchronization
		} else {
			d = half + time.Duration(jitter.Int64())
		}
	} else {
		d = ceiling
	}

	t := time.NewTimer(d)
	// Explicit drain is required: 'defer t.Stop()' alone does not drain t.C when
	// Stop() returns false (timer already fired), which would prevent the timer from
	// being garbage collected. The select below ensures exactly one of the two cases
	// fires — there is no goroutine leak regardless of which path executes first.
	select {
	case <-ctx.Done():
		if !t.Stop() {
			<-t.C // timer already fired; drain so the runtime can GC it
		}
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
