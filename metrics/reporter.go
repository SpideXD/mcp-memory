package metrics

import (
	"context"
	"time"

	"mcp-memory/logger"
)

// StartReporter begins periodic metric snapshot logging at the given interval.
// Stops when ctx is cancelled. Every tick, all registered metrics are dumped
// as a single structured log line.
func StartReporter(ctx context.Context, log *logger.Logger, interval time.Duration) {
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				log.Info("metrics snapshot",
					"timestamp", time.Now().Format(time.RFC3339),
					"metrics", global.Snapshot(""),
					"system", CaptureSysStats(),
				)
			case <-ctx.Done():
				return
			}
		}
	}()
}

// StartReporterWithPrefix reports only metrics matching the given prefix.
// Stops when ctx is cancelled.
func StartReporterWithPrefix(ctx context.Context, log *logger.Logger, prefix string, interval time.Duration) {
	if prefix == "" {
		StartReporter(ctx, log, interval)
		return
	}
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				log.Info("metrics snapshot",
					"prefix", prefix,
					"timestamp", time.Now().Format(time.RFC3339),
					"metrics", global.Snapshot(prefix),
					"system", CaptureSysStats(),
				)
			case <-ctx.Done():
				return
			}
		}
	}()
}
