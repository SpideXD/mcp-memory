package metrics

import (
	"math"
	"sync"
	"time"
)

// EWMA tracks an exponentially weighted moving average of events per second.
// There is no background ticker — decay is computed from wall-clock time
// elapsed between Update calls. A ~30-second half-life means that after 30s
// of inactivity the reported rate drops to ~50% of its previous value.
//
// Unlike a simple counter/time-window approach, the EWMA smoothly converges
// to the true rate regardless of burstiness or idle periods.
type EWMA struct {
	alpha       float64
	initialized bool
	rate        float64
	lastUpdate  time.Time
	mu          sync.Mutex
}

// newEWMA creates a rate tracker with a ~30-second half-life.
func newEWMA() *EWMA {
	return &EWMA{
		alpha: 1 - math.Exp(-1.0/30.0), // half-life ≈ 30 seconds
	}
}

// Update incorporates n events that occurred since the last Update call.
// n is converted to an instantaneous rate (events/second) using the elapsed
// wall-clock time, then smoothed into the EWMA.
func (e *EWMA) Update(n float64) {
	e.mu.Lock()
	defer e.mu.Unlock()

	now := time.Now()
	elapsed := now.Sub(e.lastUpdate).Seconds()
	if elapsed <= 0 {
		elapsed = 0.0001 // floor to prevent division by zero on sub-ms spikes
	}

	// Convert count to instantaneous rate for this interval.
	instantRate := n / elapsed

	if e.initialized {
		decay := math.Pow(1-e.alpha, elapsed)
		e.rate = e.rate*decay + instantRate*(1-decay)
	} else {
		e.rate = instantRate
		e.initialized = true
	}

	e.lastUpdate = now
}

// Value returns the current smoothed rate in events per second, decayed to
// the present time. An idle system approaches zero within ~3 minutes.
func (e *EWMA) Value() float64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.initialized {
		return 0
	}
	elapsed := time.Since(e.lastUpdate).Seconds()
	if elapsed <= 0 {
		elapsed = 0.0001
	}
	return e.rate * math.Pow(1-e.alpha, elapsed)
}
