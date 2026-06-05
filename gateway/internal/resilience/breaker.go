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
// so it may safely call back into the breaker or acquire other locks. The state
// parameter s is the value at the moment of transition; by the time onState runs,
// b.state may have advanced further due to concurrent goroutines. For operational
// metrics (e.g. Prometheus gauges) this transient staleness is acceptable — it
// self-corrects on the next state transition.
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
	now := b.now()
	var onState func(State)
	var result error
	var newState State
	switch b.state {
	case StateOpen:
		if now.Before(b.openUntil) {
			result = ErrBreakerOpen
		} else {
			// Cooldown elapsed — allow exactly one probe.
			b.state = StateHalfOpen
			b.probeInFlight = true
			onState = b.onState
			newState = StateHalfOpen
		}
	case StateHalfOpen:
		if b.probeInFlight {
			result = ErrBreakerOpen
		} else {
			b.probeInFlight = true
		}
	case StateClosed:
		// No probe slot needed; proceed with the call.
	}
	b.mu.Unlock()
	if onState != nil {
		onState(newState)
	}
	return result
}

// RecordSuccess records a successful call.
//   - StateClosed: resets the failure counter (partial streak forgotten).
//   - StateHalfOpen: closes the breaker (probe succeeded).
//   - StateOpen: stale success from a call admitted before the breaker tripped;
//     failure streak is preserved so the cooldown + probe path decides recovery.
//
// probeInFlight is only ever true in StateHalfOpen. In StateClosed, Allow() never
// sets it. In StateOpen, RecordFailure clears it unconditionally before any Open
// transition, so it is always false on entry in those two states.
//
// Known limitation: a stale StateClosed success that arrives after the breaker has
// already entered StateHalfOpen will close the breaker prematurely. This is
// self-correcting: the actual probe still runs and RecordFailure will re-open if
// the worker is unhealthy. A per-probe generation token would prevent it but is
// out of scope for this implementation.
func (b *Breaker) RecordSuccess() {
	if b == nil || b.cfg.Threshold <= 0 {
		return
	}
	b.mu.Lock()
	var onState func(State)
	switch b.state {
	case StateHalfOpen:
		// Probe succeeded → close the breaker and reset the failure streak.
		b.failures = 0
		b.probeInFlight = false
		b.state = StateClosed
		onState = b.onState
	case StateOpen:
		// Stale success from a request admitted before the breaker tripped.
		// Failure streak is preserved — recovery requires a successful probe.
		// probeInFlight is always false in StateOpen per the invariant above;
		// the clear is defensive so future refactors can't accidentally leak it.
		b.probeInFlight = false
	case StateClosed:
		// Normal healthy call: reset the failure streak so transient failures are
		// forgotten once a success arrives. probeInFlight is always false here
		// (see invariants above); no modification needed.
		b.failures = 0
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
	now := b.now()
	b.probeInFlight = false
	b.failures++
	var onState func(State)
	switch b.state {
	case StateClosed:
		if b.failures >= b.cfg.Threshold {
			b.openUntil = now.Add(b.cfg.Cooldown)
			b.state = StateOpen
			onState = b.onState
		}
	case StateHalfOpen:
		// Probe failed — reset cooldown and re-open.
		b.openUntil = now.Add(b.cfg.Cooldown)
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
