package main

import (
	"time"

	"mcp-memory/backend"
)

// circuitBreaker is the type alias for backward compatibility with existing code/tests.
type circuitBreaker = backend.CircuitBreaker

// newCircuitBreaker is a compatibility wrapper that calls backend.NewCircuitBreaker.
func newCircuitBreaker(threshold int, cooldown time.Duration) *circuitBreaker {
	return backend.NewCircuitBreaker(threshold, cooldown)
}
