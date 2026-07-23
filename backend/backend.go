package backend

import (
	"context"
	"errors"
	"time"
)

// ErrNotSupported is returned by optional operations the backend doesn't implement.
var ErrNotSupported = errors.New("operation not supported by current backend")

// Backend is the single interface for all memory backends.
type Backend interface {
	// Retain stores content in the backend. Returns response body on success.
	Retain(ctx context.Context, bank string, content string) (string, error)

	// Recall searches memory. Returns response body (JSON with recalled memories).
	Recall(ctx context.Context, bank string, query string) (string, error)

	// Reflect synthesizes memories for insights. Empty query = full improve.
	Reflect(ctx context.Context, bank string, query string) (string, error)

	// Health checks backend connectivity. Returns nil if healthy.
	Health(ctx context.Context) error

	// Name returns the backend name (e.g., "hindsight", "cognee").
	Name() string

	// IsSync returns true for backends whose operations complete inline.
	// True:  handlers use worker pool (Hindsight)
	// False: handlers spawn goroutine with semaphore (Cognee)
	IsSync() bool

	// Forget removes a specific memory. Optional — may return ErrNotSupported.
	Forget(ctx context.Context, bank string, contentID string) (string, error)
}

// BackendConfig is the flat configuration struct passed to the New factory.
// It avoids circular imports by not referencing the main package's Config.
type BackendConfig struct {
	Backend               string        // "hindsight", "cognee-python", "cognee-rust"
	HindsightPort         string
	CogneePort            string
	BackendRetainTimeout  time.Duration
	BackendRecallTimeout  time.Duration
	BackendReflectTimeout time.Duration
	CogneeRetainTimeout   time.Duration // Cognee-specific: 900s default, used for HTTP client timeout
	RetryAttempts         int
	RetryDelay            time.Duration
	RetryMaxDelay         time.Duration
	CircuitBreakerThreshold int
	CircuitBreakerCooldown  time.Duration
}

// New creates the appropriate Backend based on the config.
func New(cfg BackendConfig) Backend {
	switch cfg.Backend {
	case "hindsight":
		return newHindsightBackend(cfg)
	case "cognee-python", "cognee-rust":
		return newCogneeBackend(cfg)
	default:
		// Default to hindsight for backward compatibility
		return newHindsightBackend(cfg)
	}
}
