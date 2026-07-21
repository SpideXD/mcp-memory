package metrics

import (
	"encoding/json"
	"strings"
	"sync"
)

// Registry holds all registered metrics. Global instance auto-registers
// NewCounter, NewGauge, NewHistogram, and NewTimer on creation.
type Registry struct {
	mu      sync.RWMutex
	metrics []Metric
}

var global = NewRegistry()

// NewRegistry creates an empty registry.
func NewRegistry() *Registry { return &Registry{} }

// Register adds a metric to the registry. Safe for concurrent use.
func (r *Registry) Register(m Metric) {
	r.mu.Lock()
	r.metrics = append(r.metrics, m)
	r.mu.Unlock()
}

// Snapshot returns all metrics matching the given prefix (dot-boundary match).
// "collector" matches "collector.files" but NOT "collectors_misc" or "collector_v2".
// Pass "" for all metrics.
func (r *Registry) Snapshot(prefix string) map[string]interface{} {
	r.mu.RLock()
	defer r.mu.RUnlock()

	result := make(map[string]interface{})
	for _, m := range r.metrics {
		if prefix != "" {
			// Dot-boundary match: "coll" must NOT match "collector.files".
			if !strings.HasPrefix(m.Name(), prefix+".") && m.Name() != prefix {
				continue
			}
		}
		for k, v := range m.Snapshot() {
			result[k] = v
		}
	}
	return result
}

// MarshalJSON serializes the full snapshot as JSON.
func (r *Registry) MarshalJSON() ([]byte, error) {
	return json.Marshal(r.Snapshot(""))
}

// Snapshot returns all registered metrics matching the given prefix from the
// global registry. Pass "" for all metrics.
func Snapshot(prefix string) map[string]interface{} {
	return global.Snapshot(prefix)
}

// ClearRegistry removes all metrics from the global registry.
// For testing only.
func ClearRegistry() {
	global.mu.Lock()
	global.metrics = nil
	global.mu.Unlock()
}

// SysSnapshot returns the current system stats. Convenience wrapper.
func SysSnapshot() map[string]interface{} {
	return CaptureSysStats()
}
