package metrics

import (
	"fmt"
	"math"
	"sort"
	"sync"
	"sync/atomic"

	"mcp-memory/logger"
)

const maxSamples = 1000

// Histogram records values and computes p50/p90/p99 percentiles
// using a ring buffer of the last maxSamples observations.
// Generic: records bytes, tokens, latency — anything.
type Histogram struct {
	name    string
	log     *logger.Logger
	mu      sync.Mutex
	count   atomic.Int64
	total   atomic.Int64
	min     atomic.Int64
	max     atomic.Int64
	samples [maxSamples]int64
	sampleN int
	sampleIdx int
	rate    *EWMA
}

// NewHistogram creates a histogram and auto-registers it.
func NewHistogram(name string) *Histogram {
	h := &Histogram{
		name: name,
		rate: newEWMA(),
	}
	h.min.Store(math.MaxInt64)
	global.Register(h)
	return h
}

// WithLogger attaches a logger for debug logging.
func (h *Histogram) WithLogger(log *logger.Logger) *Histogram {
	h.log = log
	return h
}

// Name returns the histogram name.
func (h *Histogram) Name() string { return h.name }

// Record adds a value to the histogram. Thread-safe.
// total uses saturation arithmetic to prevent silent int64 overflow.
func (h *Histogram) Record(val int64) {
	h.count.Add(1)

	// Saturation add — prevents total from overflowing silently.
	for {
		old := h.total.Load()
		new := old + val
		if new < old { // overflow: wrap-around detected
			new = math.MaxInt64
		}
		if h.total.CompareAndSwap(old, new) {
			break
		}
	}

	h.rate.Update(1)

	// Atomic min/max update.
	for {
		old := h.min.Load()
		if val >= old || h.min.CompareAndSwap(old, val) {
			break
		}
	}
	for {
		old := h.max.Load()
		if val <= old || h.max.CompareAndSwap(old, val) {
			break
		}
	}

	// Ring buffer for percentiles.
	h.mu.Lock()
	h.samples[h.sampleIdx] = val
	h.sampleIdx = (h.sampleIdx + 1) % maxSamples
	if h.sampleN < maxSamples {
		h.sampleN++
	}
	h.mu.Unlock()
}

// Percentiles computes p50, p90, p99 from the ring buffer.
func (h *Histogram) Percentiles() (p50, p90, p99 int64) {
	h.mu.Lock()
	slice := make([]int64, h.sampleN)
	copy(slice, h.samples[:h.sampleN])
	h.mu.Unlock()

	if len(slice) == 0 {
		return
	}
	sort.Slice(slice, func(i, j int) bool { return slice[i] < slice[j] })
	p50 = slice[len(slice)*50/100]
	p90 = slice[len(slice)*90/100]
	p99 = slice[len(slice)*99/100]
	return
}

// Snapshot returns the histogram state.
func (h *Histogram) Snapshot() map[string]interface{} {
	n := h.count.Load()
	if n == 0 {
		return map[string]interface{}{h.name + "_count": int64(0)}
	}
	p50, p90, p99 := h.Percentiles()
	avg := float64(h.total.Load()) / float64(n)
	return map[string]interface{}{
		h.name + "_count": n,
		h.name + "_avg":   avg,
		h.name + "_min":   h.min.Load(),
		h.name + "_max":   h.max.Load(),
		h.name + "_p50":   p50,
		h.name + "_p90":   p90,
		h.name + "_p99":   p99,
		h.name + "_rate":  fmt.Sprintf("%.1f/s", h.rate.Value()),
	}
}
