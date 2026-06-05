// Package resilience provides retry and circuit-breaker policies for gateway→worker calls.
// Zero-value BreakerConfig disables the circuit breaker, preserving single-shot behaviour
// by default. See retry.go for the retry Policy.
package resilience

import (
	"errors"
	"sync"
	"time"
)

// ErrBreakerOpen is returned by Breaker.Allow when the circuit is open.
var ErrBreakerOpen = errors.New("circuit breaker open")

// State is the circuit-breaker state. Values double as Prometheus gauge readings.
type State int

const (
	StateClosed   State = 0
	StateOpen     State = 1
	StateHalfOpen State = 2
)

// BreakerConfig controls circuit-breaker behaviour.
// Threshold <= 0 disables the breaker entirely.
type BreakerConfig struct {
	// Threshold is the number of consecutive failures before opening.
	Threshold int
	// Cooldown is how long the breaker stays open before allowing a probe.
	Cooldown time.Duration
}

// Breaker is a concurrent-safe closed/open/half-open circuit breaker.
type Breaker struct {
	cfg           BreakerConfig
	mu            sync.Mutex
	state         State
	failures      int
	openUntil     time.Time
	probeInFlight bool
	onState       func(State)
	now           func() time.Time
}

// NewBreaker creates a Breaker. If cfg.Threshold <= 0 the breaker is disabled and
// every Allow returns nil. onState (may be nil) is called on every state transition
// and receives the new state; it is invoked after the internal lock is released,
// so it may safely call back into the breaker or acquire other locks.
func NewBreaker(cfg BreakerConfig, onState func(State)) *Breaker {
	return &Breaker{cfg: cfg, onState: onState, now: time.Now}
}

// WithNow overrides the clock source. Intended for deterministic tests only.
func (b *Breaker) WithNow(now func() time.Time) *Breaker {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.now = now
	return b
}

// Allow returns nil if the call may proceed, or ErrBreakerOpen to fast-fail it
// without making a network call.
// When Allow returns nil from a half-open state the caller MUST call RecordSuccess,
// RecordFailure, or RecordAbort to release the in-flight probe slot.
func (b *Breaker) Allow() error {
	if b == nil || b.cfg.Threshold <= 0 {
		return nil
	}
	// The entire decision — read state, check probeInFlight, set probeInFlight — runs
	// inside a single lock acquisition so the check and the set are always atomic.
	b.mu.Lock()
	var onState func(State)
	var result error
	switch b.state {
	case StateOpen:
		if b.now().Before(b.openUntil) {
			result = ErrBreakerOpen
		} else {
			// Cooldown elapsed — allow exactly one probe.
			b.state = StateHalfOpen
			b.probeInFlight = true
			onState = b.onState
		}
	case StateHalfOpen:
		if b.probeInFlight {
			result = ErrBreakerOpen
		} else {
			b.probeInFlight = true
		}
	// StateClosed: result stays nil; proceed.
	}
	b.mu.Unlock()
	if onState != nil {
		onState(StateHalfOpen)
	}
	return result
}

// RecordSuccess records a successful call. Closes the breaker only from StateHalfOpen;
// resets the failure counter and probe slot from any non-open state. In StateOpen,
// the call is a stale in-flight from before the breaker opened — failure streak is
// preserved so the cooldown + probe path decides recovery.
func (b *Breaker) RecordSuccess() {
	if b == nil || b.cfg.Threshold <= 0 {
		return
	}
	b.mu.Lock()
	if b.state == StateOpen {
		// Release stale probe slot if one was in flight (idempotent if already false).
		if b.probeInFlight {
			b.probeInFlight = false
		}
		b.mu.Unlock()
		return // stale success; preserve failure streak
	}
	b.failures = 0
	b.probeInFlight = false
	var onState func(State)
	if b.state == StateHalfOpen {
		b.state = StateClosed
		onState = b.onState
	}
	b.mu.Unlock()
	if onState != nil {
		onState(StateClosed)
	}
}

// RecordAbort releases a half-open probe slot without recording a health signal.
// Use when the probe call was cancelled by the caller (context.Canceled) before
// any HTTP response arrived; unlike RecordSuccess it does not close the breaker,
// and unlike RecordFailure it does not re-open it or reset the cooldown timer.
func (b *Breaker) RecordAbort() {
	if b == nil || b.cfg.Threshold <= 0 {
		return
	}
	b.mu.Lock()
	b.probeInFlight = false
	b.mu.Unlock()
}

// RecordFailure records a failed call and may open the breaker.
func (b *Breaker) RecordFailure() {
	if b == nil || b.cfg.Threshold <= 0 {
		return
	}
	b.mu.Lock()
	b.probeInFlight = false
	b.failures++
	var onState func(State)
	switch b.state {
	case StateClosed:
		if b.failures >= b.cfg.Threshold {
			b.openUntil = b.now().Add(b.cfg.Cooldown)
			b.state = StateOpen
			onState = b.onState
		}
	case StateHalfOpen:
		// Probe failed — reset cooldown and re-open.
		b.openUntil = b.now().Add(b.cfg.Cooldown)
		b.state = StateOpen
		onState = b.onState
	// StateOpen: already open; don't reset the cooldown timer on new failures.
	}
	b.mu.Unlock()
	if onState != nil {
		onState(StateOpen)
	}
}

// CurrentState returns the breaker's current state. Safe to call concurrently.
func (b *Breaker) CurrentState() State {
	if b == nil || b.cfg.Threshold <= 0 {
		return StateClosed
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.state
}
