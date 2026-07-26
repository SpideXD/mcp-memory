package backend

import (
	"sync"
	"time"
)

// CircuitBreaker tracks failures and fails fast when threshold is exceeded.
// Safe for concurrent use.
//
// States: CLOSED (normal) → OPEN (failing fast) → HALF_OPEN (probing) → CLOSED
// In HALF_OPEN, a single probe request is allowed through. Success closes the
// circuit; failure re-opens it for another cooldown period.
type CircuitBreaker struct {
	mu           sync.Mutex
	failures     int
	threshold    int
	cooldown     time.Duration
	trippedUntil time.Time
	halfOpen     bool // true when probing after cooldown expires
}

// NewCircuitBreaker creates a new circuit breaker with the given threshold and cooldown.
func NewCircuitBreaker(threshold int, cooldown time.Duration) *CircuitBreaker {
	return &CircuitBreaker{
		threshold: threshold,
		cooldown:  cooldown,
	}
}

// RecordFailure increments the failure count and trips the breaker if threshold reached.
func (cb *CircuitBreaker) RecordFailure() {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	cb.failures++

	if cb.halfOpen {
		// Probe failed — re-open for full cooldown
		cb.trippedUntil = time.Now().Add(cb.cooldown)
		cb.halfOpen = false
		cb.failures = 1
		return
	}

	if cb.failures >= cb.threshold {
		cb.trippedUntil = time.Now().Add(cb.cooldown)
		cb.halfOpen = false
	}
}

// RecordSuccess resets the failure count and closes the circuit.
// In half-open state, a successful probe fully resets the breaker.
func (cb *CircuitBreaker) RecordSuccess() {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	cb.failures = 0
	cb.trippedUntil = time.Time{}
	cb.halfOpen = false
}

// IsTripped returns true if the circuit breaker is open (failing fast).
// When cooldown expires, the first caller enters half-open state and is
// allowed through as a probe. Subsequent callers while the probe is in
// flight are rejected (fail fast).
func (cb *CircuitBreaker) IsTripped() bool {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	if cb.trippedUntil.IsZero() {
		return false // CLOSED
	}

	// Half-open: a probe is already in flight — reject
	if cb.halfOpen {
		return true
	}

	// OPEN but cooldown may have expired
	if time.Now().After(cb.trippedUntil) {
		// Enter half-open state — allow exactly this caller as probe
		cb.halfOpen = true
		return false
	}

	return true // OPEN, cooldown not expired
}
