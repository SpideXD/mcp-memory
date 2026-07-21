package metrics

import (
	"strings"
	"time"

	"mcp-memory/logger"
)

// Timer wraps a Histogram with duration-specific start/stop and snapshot.
type Timer struct {
	hist *Histogram
	name string
}

// NewTimer creates a timer and auto-registers it.
func NewTimer(name string) *Timer {
	return &Timer{hist: NewHistogram(name), name: name}
}

// WithLogger attaches a logger for debug logging.
func (t *Timer) WithLogger(log *logger.Logger) *Timer {
	t.hist.WithLogger(log)
	return t
}

// Name returns the timer name.
func (t *Timer) Name() string { return t.name }

// Start begins a timing measurement.
func (t *Timer) Start() *TimerHandle {
	return &TimerHandle{t: t, start: time.Now()}
}

// Stop records the elapsed duration.
func (t *Timer) Stop(h *TimerHandle) {
	d := time.Since(h.start)
	t.hist.Record(d.Nanoseconds())

	if t.hist.log != nil {
		t.hist.log.Debug("timer stop",
			"name", t.name,
			"duration", d.String(),
			"count", t.hist.count.Load(),
		)
	}
}

// Snapshot returns the timer state with nanos converted to duration strings.
// _avg (float64) is also converted since it represents mean nanoseconds.
func (t *Timer) Snapshot() map[string]interface{} {
	raw := t.hist.Snapshot()
	result := make(map[string]interface{})
	for k, v := range raw {
		switch {
		case strings.HasSuffix(k, "_count"), strings.HasSuffix(k, "_rate"):
			result[k] = v // pass through as-is
		case strings.HasSuffix(k, "_avg"):
			// _avg is float64 nanos — convert to duration string.
			if avg, ok := v.(float64); ok {
				result[k] = time.Duration(int64(avg)).String()
			} else {
				result[k] = v
			}
		case strings.Contains(k, "_"):
			// int64 nanos keys (p50/p90/p99/min/max) → duration string.
			if ns, ok := v.(int64); ok {
				result[k] = time.Duration(ns).String()
			} else {
				result[k] = v
			}
		default:
			result[k] = v
		}
	}
	return result
}

// TimerHandle holds the start time for a measurement.
type TimerHandle struct {
	t     *Timer
	start time.Time
}
