// Package resilience provides retry and circuit-breaker policies for worker calls.
package resilience

import (
	"context"
	"crypto/rand"
	"errors"
	"math/big"
	"time"
)

// IsRetryable reports whether a call outcome warrants a retry.
// Two independent guards ensure a cancelled or expired context is never retried:
//
//   - ctx.Err() != nil: the caller's context is already dead at evaluation time.
//     This catches the case where a non-context transport error (e.g. connection
//     reset) fired concurrently with a context cancellation — the error does not
//     wrap context.Canceled, but retrying would immediately fail again.
//   - errors.Is(err, Canceled|DeadlineExceeded): catches the narrow race window
//     where the context was live when the call started but was cancelled during
//     the round-trip. ctx.Err() may still be nil at this point, so the direct
//     errors.Is check on err is the authoritative guard for that case.
//
// Both checks are intentional and handle non-overlapping race windows.
// Callers must still check ctx.Err() after Sleep; this function only governs
// whether to attempt a sleep at all.
//
// status == 0 with a non-nil error means a transport/network error occurred
// before an HTTP response arrived — retryable when the context is live.
// status < 0 means a pre-flight build error that never reached the worker.
func IsRetryable(ctx context.Context, err error, status int) bool {
	if ctx.Err() != nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	if status < 0 {
		return false
	}
	if status >= 500 {
		return true
	}
	if err != nil && status == 0 {
		return true
	}
	return false
}

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
		// Overflow-safe doubling: if ceiling > maxB/2, doubling would exceed maxB.
		// Cap directly instead of doubling past it.
		if ceiling > maxB/2 {
			ceiling = maxB
		} else {
			ceiling *= 2
		}
	}

	// Equal jitter: uniform in [ceiling/2, ceiling] using crypto/rand.
	// Cryptographic unpredictability is required to prevent synchronized retry storms
	// when multiple gateway instances retry together; crypto/rand provides this property.
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
	select {
	case <-ctx.Done():
		// Go 1.23 changed time.NewTimer so that Stop drains t.C before returning
		// (go.dev/doc/go1.23: "NewTimer and NewTicker guarantee that Stop and Reset
		// drain the channel"). After Stop returns on Go 1.23+, t.C is empty — an
		// explicit <-t.C would block indefinitely. The go.mod requires a toolchain
		// that includes this guarantee, so no drain is needed or safe here.
		t.Stop()
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
