package main

import (
	"context"
	"sync/atomic"
	"time"
)

// Backend is an enum for the memory backend type.
// Valid values: "hindsight", "cognee-python", "cognee-rust".
// Default is "hindsight" — backward compatible.
type Backend string

const (
	BackendHindsight    Backend = "hindsight"
	BackendCogneePython Backend = "cognee-python"
	BackendCogneeRust   Backend = "cognee-rust"
)

type ServiceState string

const (
	StateStopped  ServiceState = "stopped"
	StateStarting ServiceState = "starting"
	StateDegraded ServiceState = "degraded"
	StateRunning  ServiceState = "running"
)

// MCPSession holds per-agent state. Bank is immutable after creation.
type MCPSession struct {
	SessionID  string
	Bank       string
	SSEChannel chan string
	CreatedAt  time.Time
	LastActive time.Time
	closed     atomic.Bool
}

func (s *MCPSession) Close() {
	if s.closed.CompareAndSwap(false, true) {
		close(s.SSEChannel)
	}
}

func (s *MCPSession) IsClosed() bool {
	return s.closed.Load()
}

// MemoryJob is a work item for the channel worker pool.
type MemoryJob struct {
	Bank    string
	Method  string // "retain" or "reflect"
	Data    string
	Result  chan MemoryResult
	Ctx     context.Context    // cancelled when queueJob times out
	Cancel  context.CancelFunc // called by queueJob on timeout
}

type MemoryResult struct {
	Data string
	Err  error
}
