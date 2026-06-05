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
	// probeGen is incremented on every HalfOpen transition. Record* methods
	// compare their token argument against probeGen; a mismatch means the call
	// was admitted from a different (stale) breaker state and its outcome must
	// not influence the active probe.
	probeGen uint64
	onState  func(State)
	now      func() time.Time
}

// NewBreaker creates a Breaker. If cfg.Threshold <= 0 the breaker is disabled and
// every Allow returns (0, nil). onState (may be nil) is called on every state transition
// and receives the new state; it is invoked after the internal lock is released,
// so it may safely call back into the breaker or acquire other locks. The state
// parameter s is the value at the moment of transition; by the time onState runs,
// b.state may have advanced further due to concurrent goroutines. For operational
// metrics (e.g. Prometheus gauges) this transient staleness is acceptable — it
// self-corrects on the next state transition.
func NewBreaker(cfg BreakerConfig, onState func(State)) *Breaker {
	return &Breaker{cfg: cfg, onState: onState, now: time.Now}
}

// WithNow overrides the clock source. Intended for deterministic tests only;
// do not call while Allow/RecordSuccess/RecordFailure are being called concurrently.
func (b *Breaker) WithNow(now func() time.Time) *Breaker {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.now = now
	return b
}

// Allow returns (token, nil) if the call may proceed, or (0, ErrBreakerOpen) to
// fast-fail without making a network call.
//
// When Allow returns a non-zero token, the caller MUST pass that same token to
// RecordSuccess, RecordFailure, or RecordAbort to release the probe slot and record
// a health signal. The token encodes the breaker generation: Record* calls with a
// stale token (admitted under an earlier state) are silently ignored, preventing
// stale results from closing or re-opening the breaker incorrectly.
//
// A zero token is returned for calls admitted from StateClosed; callers may pass
// 0 to Record* for non-probe calls (the token check is skipped for StateClosed).
func (b *Breaker) Allow() (uint64, error) {
	if b == nil || b.cfg.Threshold <= 0 {
		return 0, nil
	}
	// The entire decision — read state, check probeInFlight, set probeInFlight — runs
	// inside a single lock acquisition so the check and the set are always atomic.
	b.mu.Lock()
	now := b.now()
	var onState func(State)
	var token uint64
	var result error
	var newState State
	switch b.state {
	case StateOpen:
		if now.Before(b.openUntil) {
			result = ErrBreakerOpen
		} else {
			// Cooldown elapsed — allow exactly one probe; bump generation.
			b.probeGen++
			b.state = StateHalfOpen
			b.probeInFlight = true
			token = b.probeGen
			onState = b.onState
			newState = StateHalfOpen
		}
	case StateHalfOpen:
		if b.probeInFlight {
			result = ErrBreakerOpen
		} else {
			// A prior probe completed (via Record*) but the breaker hasn't closed yet
			// (e.g. RecordAbort released the slot). Allow a fresh probe and bump the
			// generation so any stale Record* calls from the previous probe are ignored.
			// No state transition occurs here — the breaker stays HalfOpen.
			b.probeGen++
			b.probeInFlight = true
			token = b.probeGen
		}
	case StateClosed:
		// Non-probe call; no probe slot needed. Token stays 0.
	}
	b.mu.Unlock()
	// newState is a value captured inside the lock before Unlock(); it is not a live
	// reference to b.state. Calling onState after Unlock() is safe: the callback
	// receives an immutable copy and may safely call back into the breaker.
	if onState != nil {
		onState(newState)
	}
	return token, result
}

// RecordSuccess records a successful call.
//   - StateClosed: resets the failure counter (partial streak forgotten). token is ignored.
//   - StateHalfOpen + matching token: closes the breaker (probe succeeded).
//   - All other cases (stale token, StateOpen): no-op; failure streak and probe slot
//     are preserved so the cooldown + probe path decides recovery.
func (b *Breaker) RecordSuccess(token uint64) {
	if b == nil || b.cfg.Threshold <= 0 {
		return
	}
	b.mu.Lock()
	var onState func(State)
	switch b.state {
	case StateHalfOpen:
		if token != b.probeGen {
			// Stale success from a call admitted before the current probe generation.
			// Silently ignore — do not close the breaker or release the probe slot.
			b.mu.Unlock()
			return
		}
		// Probe succeeded → close the breaker and reset the failure streak.
		b.failures = 0
		b.probeInFlight = false
		b.state = StateClosed
		onState = b.onState
	case StateClosed:
		// Normal healthy call: reset the failure streak so transient failures are
		// forgotten once a success arrives.
		b.failures = 0
	case StateOpen:
		// Stale success from a call admitted before the breaker tripped.
		// Do NOT reset failures — the open was caused by a real streak and recovery
		// requires a successful probe, not a stale in-flight request's completion.
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
// For non-probe calls (token == 0 or admitted from StateClosed), RecordAbort is a no-op.
func (b *Breaker) RecordAbort(token uint64) {
	if b == nil || b.cfg.Threshold <= 0 {
		return
	}
	b.mu.Lock()
	// Only release the probe slot if this is the active probe generation.
	// A stale abort (token != b.probeGen) must not clear a newer probe's slot.
	if b.state == StateHalfOpen && token == b.probeGen {
		b.probeInFlight = false
	}
	b.mu.Unlock()
}

// RecordFailure records a failed call and may open the breaker.
// For StateHalfOpen + matching token: probe failed — reset cooldown and re-open.
// For StateClosed: increment failure counter; open if threshold reached.
// For StateOpen or stale token in HalfOpen: no-op (don't reset an active cooldown).
func (b *Breaker) RecordFailure(token uint64) {
	if b == nil || b.cfg.Threshold <= 0 {
		return
	}
	b.mu.Lock()
	now := b.now()
	var onState func(State)
	switch b.state {
	case StateClosed:
		b.failures++
		if b.failures >= b.cfg.Threshold {
			b.openUntil = now.Add(b.cfg.Cooldown)
			b.state = StateOpen
			onState = b.onState
		}
	case StateHalfOpen:
		if token != b.probeGen {
			// Stale failure from a call admitted before the current probe generation.
			// Do not re-open or reset the cooldown — this result is not meaningful for
			// the active probe.
			b.mu.Unlock()
			return
		}
		// Probe failed — reset cooldown and re-open.
		b.probeInFlight = false
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
