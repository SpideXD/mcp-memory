package worker

import (
	"context"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"mcp-memory/logger"
)

// Pool manages a set of worker goroutines that execute a function in a loop.
// fn receives a context.Context that is cancelled on Stop() — use ctx.Done()
// to break out of long-running work. On panic: logs and restarts after 100ms
// backoff (respects Stop() during backoff).
//
// fn should be blocking (e.g., select on channels). If fn returns instantly,
// runtime.Gosched() yields the CPU as a safety net.
type Pool struct {
	name    string
	workers int
	log     *logger.Logger
	fn      func(context.Context)
	stop    chan struct{}
	ctx     context.Context
	cancel  context.CancelFunc
	wg      sync.WaitGroup

	started  atomic.Bool
	panics   atomic.Int64
	restarts atomic.Int64
}

// NewPool creates a managed worker pool. fn is called in a loop by each worker.
// If workers < 1, it defaults to 1. fn receives a context that is cancelled
// on Stop() — use ctx.Done() to break out of iteration.
// If log is nil, the pool creates a silent logger (no output).
func NewPool(name string, workers int, log *logger.Logger, fn func(context.Context)) *Pool {
	if workers < 1 {
		workers = 1
	}
	if log == nil {
		log, _ = logger.New("pool", "error")
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Pool{
		name:    name,
		workers: workers,
		log:     log.With("pool", name),
		fn:      fn,
		stop:    make(chan struct{}),
		ctx:     ctx,
		cancel:  cancel,
	}
}

// Start launches all workers. Idempotent — second call is a no-op.
// After Stop(), Start() may be called again to restart the pool.
func (p *Pool) Start() {
	if p.started.Swap(true) {
		return
	}

	p.log.Info("pool started", "workers", p.workers)

	for i := 0; i < p.workers; i++ {
		p.wg.Add(1)
		go p.runWorker(i)
	}
}

// Stop signals all workers to finish and blocks until they exit.
// After Stop() returns, the pool may be started again with Start().
// Safe to call concurrently — uses CAS to claim shutdown exactly once.
func (p *Pool) Stop() {
	// Atomic CAS prevents concurrent stop from double-closing the channel.
	if !p.started.CompareAndSwap(true, false) {
		return
	}

	p.cancel()
	close(p.stop)

	p.wg.Wait()

	// Reset for potential restart.
	p.stop = make(chan struct{})
	p.ctx, p.cancel = context.WithCancel(context.Background())

	p.log.Info("pool stopped",
		"panics", p.panics.Load(),
		"restarts", p.restarts.Load(),
	)
}

// Stats returns runtime metrics.
func (p *Pool) Stats() map[string]interface{} {
	return map[string]interface{}{
		"name":     p.name,
		"workers":  p.workers,
		"started":  p.started.Load(),
		"panics":   p.panics.Load(),
		"restarts": p.restarts.Load(),
	}
}

// runWorker loops calling tryWork until Stop() signals.
func (p *Pool) runWorker(id int) {
	defer p.wg.Done()

	for {
		p.tryWork(id)

		select {
		case <-p.stop:
			return
		default:
			runtime.Gosched() // safety net: yield if fn() returns without blocking
		}
	}
}

// tryWork calls fn with panic recovery.
func (p *Pool) tryWork(id int) {
	defer func() {
		if r := recover(); r != nil {
			p.panics.Add(1)
			p.restarts.Add(1)
			p.log.Error("worker panicked, restarting",
				"worker", id,
				"panic", fmt.Sprintf("%v", r),
				"panics_total", p.panics.Load(),
			)

			// Backoff before restart, but abort if pool is shutting down.
			select {
			case <-time.After(100 * time.Millisecond):
			case <-p.stop:
			}
		}
	}()

	p.fn(p.ctx)
}
