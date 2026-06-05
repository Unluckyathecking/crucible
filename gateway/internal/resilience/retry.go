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
// A cancelled or timed-out context is never retryable: checking ctx first
// prevents a stale transport error (e.g. connection reset concurrent with
// cancellation) from incorrectly triggering a retry when the caller has
// already given up. Callers must still check ctx.Err() after Sleep; this
// function only governs whether to attempt a sleep at all.
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
