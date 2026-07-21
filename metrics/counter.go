package metrics

import (
	"fmt"
	"sync/atomic"
)

// TagPair is a key-value tag for dimensional metrics.
type TagPair struct {
	Key   string
	Value string
}

// Counter is an atomic counter with EWMA rate tracking and tag support.
type Counter struct {
	name  string
	value atomic.Int64
	rate  *EWMA
	tags  []TagPair
}

// NewCounter creates a counter and auto-registers it.
func NewCounter(name string) *Counter {
	c := &Counter{name: name, rate: newEWMA()}
	global.Register(c)
	return c
}

// WithTag creates a tagged copy registered under "name{key=value}".
func (c *Counter) WithTag(key, value string) *Counter {
	tagged := &Counter{
		name: fmt.Sprintf("%s{%s=%s}", c.name, key, value),
		rate: newEWMA(),
		tags: append(c.tags, TagPair{key, value}),
	}
	global.Register(tagged)
	return tagged
}

// Name returns the counter name.
func (c *Counter) Name() string { return c.name }

// Inc increments the counter by 1.
func (c *Counter) Inc() { c.Add(1) }

// Add adds delta to the counter and records delta events in the rate tracker.
func (c *Counter) Add(delta int64) {
	c.value.Add(delta)
	c.rate.Update(float64(delta))
}

// Value returns the current counter value.
func (c *Counter) Value() int64 { return c.value.Load() }

// Snapshot returns counter state with rate.
func (c *Counter) Snapshot() map[string]interface{} {
	return map[string]interface{}{
		c.name + "_count": c.value.Load(),
		c.name + "_rate":  fmt.Sprintf("%.1f/s", c.rate.Value()),
	}
}
