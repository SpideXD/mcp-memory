package backend

import (
	"sync"
	"time"
)

// CircuitBreaker tracks failures and fails fast when threshold is exceeded.
// Safe for concurrent use.
type CircuitBreaker struct {
	mu           sync.Mutex
	failures     int
	threshold    int
	cooldown     time.Duration
	lastFailure  time.Time
	trippedUntil time.Time
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
	cb.lastFailure = time.Now()
	if cb.failures >= cb.threshold {
		cb.trippedUntil = time.Now().Add(cb.cooldown)
	}
}

// RecordSuccess resets the failure count and untrips the breaker.
func (cb *CircuitBreaker) RecordSuccess() {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	cb.failures = 0
	cb.trippedUntil = time.Time{}
}

// IsTripped returns true if the circuit breaker is active (failing fast).
// After cooldown expires, allows one request through by resetting.
func (cb *CircuitBreaker) IsTripped() bool {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	if cb.trippedUntil.IsZero() {
		return false
	}
	if time.Now().After(cb.trippedUntil) {
		// Cooldown expired — allow one request through
		cb.trippedUntil = time.Time{}
		cb.failures = 0
		return false
	}
	return true
}
