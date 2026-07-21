package logger

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
)

// Logger wraps slog.Logger with a baked-in module name.
// When created with NewBuf, buf is non-nil and Bytes() is available.
type Logger struct {
	*slog.Logger
	buf *bytes.Buffer // non-nil only in buffer mode
}

// Option configures optional Logger behavior.
type Option func(*options)

type options struct {
	forceSource   bool
	forceNoSource bool
}

// WithSource forces inclusion of the source file, function, and line in every
// log line, regardless of level. Useful for one-off debugging.
func WithSource() Option {
	return func(o *options) { o.forceSource = true }
}

// WithoutSource disables source attribution even at debug level. Useful for
// production deployments where source overhead is not desired.
func WithoutSource() Option {
	return func(o *options) { o.forceNoSource = true }
}

// New creates a logger for a specific module that writes JSON to stderr.
func New(module, level string, opts ...Option) (*Logger, error) {
	return create(module, level, os.Stderr, opts)
}

// NewBuf creates a logger for a specific module that writes JSON to w.
// If w is a *bytes.Buffer, Bytes() can be used to retrieve output.
// Returns an error if w is nil.
func NewBuf(module, level string, w io.Writer, opts ...Option) (*Logger, error) {
	if w == nil {
		return nil, fmt.Errorf("logger: writer must not be nil")
	}
	l, err := create(module, level, w, opts)
	if err != nil {
		return nil, err
	}
	if buf, ok := w.(*bytes.Buffer); ok {
		l.buf = buf
	}
	return l, nil
}

// create is the shared constructor.
func create(module, level string, w io.Writer, opts []Option) (*Logger, error) {
	slogLevel, err := parseLevel(level)
	if err != nil {
		return nil, fmt.Errorf("logger: %w", err)
	}

	cfg := &options{}
	for _, opt := range opts {
		opt(cfg)
	}

	addSource := true
	if cfg.forceNoSource {
		addSource = false
	} else if cfg.forceSource {
		addSource = true
	}

	handler := slog.NewJSONHandler(w, &slog.HandlerOptions{
		Level:     slogLevel,
		AddSource: addSource,
	})

	return &Logger{
		Logger: slog.New(handler).With("module", module),
	}, nil
}

// With returns a child logger with additional key=value pairs attached.
func (l *Logger) With(args ...any) *Logger {
	return &Logger{Logger: l.Logger.With(args...), buf: l.buf}
}

// WithTrace returns a child logger with trace_id attached.
func (l *Logger) WithTrace(traceID string) *Logger {
	return l.With("trace_id", traceID)
}

// WithContext returns a child logger with any logger-relevant values
// extracted from ctx (currently: trace_id). If ctx carries no such value,
// the receiver is returned unchanged.
func (l *Logger) WithContext(ctx context.Context) *Logger {
	if ctx == nil {
		return l
	}
	if id := TraceIDFromContext(ctx); id != "" {
		return l.WithTrace(id)
	}
	return l
}

// Bytes returns the captured log output. Only meaningful in buffer mode
// (created via NewBuf with a *bytes.Buffer).
func (l *Logger) Bytes() []byte {
	if l.buf != nil {
		return l.buf.Bytes()
	}
	return nil
}

// parseLevel returns an error for unknown levels.
func parseLevel(level string) (slog.Level, error) {
	switch level {
	case "debug":
		return slog.LevelDebug, nil
	case "info":
		return slog.LevelInfo, nil
	case "warn":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return slog.LevelInfo, fmt.Errorf("unknown log level %q: use debug, info, warn, or error", level)
	}
}
