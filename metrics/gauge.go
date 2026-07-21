package metrics

import (
	"sync/atomic"
)

// Gauge is an atomic value that can go up, down, or be set directly.
type Gauge struct {
	name  string
	value atomic.Int64
}

// NewGauge creates a gauge and auto-registers it with the global registry.
func NewGauge(name string) *Gauge {
	g := &Gauge{name: name}
	global.Register(g)
	return g
}

// Name returns the gauge name.
func (g *Gauge) Name() string { return g.name }

// Inc increments the gauge by 1.
func (g *Gauge) Inc() { g.value.Add(1) }

// Dec decrements the gauge by 1.
func (g *Gauge) Dec() { g.value.Add(-1) }

// Add adds delta to the gauge.
func (g *Gauge) Add(delta int64) { g.value.Add(delta) }

// Set sets the gauge to an absolute value.
func (g *Gauge) Set(val int64) { g.value.Store(val) }

// Value returns the current gauge value.
func (g *Gauge) Value() int64 { return g.value.Load() }

// Snapshot returns the gauge state.
func (g *Gauge) Snapshot() map[string]interface{} {
	return map[string]interface{}{
		g.name: g.value.Load(),
	}
}
