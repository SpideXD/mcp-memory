package logger

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
)

// GenerateTraceID creates a unique 16-hex-char trace ID.
// Returns an error if the random source fails (extremely rare on modern systems).
func GenerateTraceID() (string, error) {
	bytes := make([]byte, 8)
	if _, err := io.ReadFull(rand.Reader, bytes); err != nil {
		return "", fmt.Errorf("generate trace ID: %w", err)
	}
	return hex.EncodeToString(bytes), nil
}

// MustTraceID generates a trace ID or returns a fallback on error.
// Use for convenience when an error is unacceptable; prefer checking the error.
func MustTraceID() string {
	id, err := GenerateTraceID()
	if err != nil {
		return "0000000000000000"
	}
	return id
}

// contextKey is a private type used to avoid collisions in context.Value.
type contextKey int

const (
	traceIDKey contextKey = iota
)

// WithTraceID returns a child context carrying the given trace ID.
// Use TraceIDFromContext to extract it, or pass a Logger.WithContext(ctx)
// to attach it to a log line.
func WithTraceID(ctx context.Context, id string) context.Context {
	if id == "" {
		return ctx
	}
	return context.WithValue(ctx, traceIDKey, id)
}

// TraceIDFromContext returns the trace ID stored in ctx, or "" if none.
func TraceIDFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	if v, ok := ctx.Value(traceIDKey).(string); ok {
		return v
	}
	return ""
}
