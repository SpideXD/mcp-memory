package logger

import (
	"log/slog"
	"time"
)

// Shared attribute constructors — use these instead of raw string keys.
// Prevents typos like "file_path" vs "filepath" vs "path".

func Duration(d time.Duration) slog.Attr { return slog.String("duration", d.String()) }
func Error(err error) slog.Attr          { return slog.Any("error", err) }
func File(val string) slog.Attr          { return slog.String("file", val) }
func Count(val int) slog.Attr            { return slog.Int("count", val) }
func Port(val int) slog.Attr             { return slog.Int("port", val) }
func PID(val int) slog.Attr              { return slog.Int("pid", val) }

// Semantic attributes — key is the conceptual name (project, reason, etc.),
// value type is chosen for human-readable log lines.

func ProjectID(val string) slog.Attr { return slog.String("project", val) }
func Reason(val string) slog.Attr   { return slog.String("reason", val) }
func Binary(val string) slog.Attr   { return slog.String("binary", val) }
func Idle(d time.Duration) slog.Attr { return slog.String("idle", d.String()) }
func TTL(d time.Duration) slog.Attr  { return slog.String("ttl", d.String()) }
func HealthWait(d time.Duration) slog.Attr {
	return slog.String("health_wait", d.String())
}
