package main

import (
	"sync/atomic"
	"time"
)

// Backend is an enum for the memory backend type.
// Valid values: "cognee-python", "cognee-rust".
// Default is "cognee-python".
type Backend string

const (
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
