package queue

import (
	"context"
	"errors"
	"log"
	"sync"
	"time"
)

// ProcessFunc is called for each dequeued job.
// Return nil for success (job → completed), non-nil for failure (job → failed/dead).
type ProcessFunc func(ctx context.Context, job *Job) error

// WorkerConfig configures the Worker pool.
type WorkerConfig struct {
	Store    *Store       // required — the queue store
	Process  ProcessFunc  // required — called for each dequeued job
	Count    int          // number of worker goroutines (0 = DefaultWorkerCount)
	SemSize  int          // max concurrent process calls (0 = DefaultSemSize)
}

// Worker manages a pool of goroutines that dequeue and process jobs.
// Safe for concurrent use. Start() and Stop() are idempotent.
type Worker struct {
	store   *Store
	sem     chan struct{}
	count   int
	process ProcessFunc
	wg      sync.WaitGroup
	cancel  context.CancelFunc
	mu      sync.Mutex // protects cancel during Start/Stop race
}

// NewWorker creates a Worker without spawning goroutines.
// Returns an error if Store or Process is nil.
func NewWorker(cfg WorkerConfig) (*Worker, error) {
	if cfg.Store == nil {
		return nil, errors.New("queue: WorkerConfig.Store must not be nil")
	}
	if cfg.Process == nil {
		return nil, errors.New("queue: WorkerConfig.Process must not be nil")
	}

	count := cfg.Count
	if count <= 0 {
		count = DefaultWorkerCount
	}
	semSize := cfg.SemSize
	if semSize <= 0 {
		semSize = DefaultSemSize
	}

	return &Worker{
		store:   cfg.Store,
		sem:     make(chan struct{}, semSize),
		count:   count,
		process: cfg.Process,
	}, nil
}

// Start spawns worker goroutines. Idempotent — calling twice does not double-spawn.
func (w *Worker) Start(ctx context.Context) {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.cancel != nil {
		return // already started
	}

	workerCtx, cancel := context.WithCancel(ctx)
	w.cancel = cancel

	for i := 0; i < w.count; i++ {
		w.wg.Add(1)
		go w.workerLoop(workerCtx, i)
	}
}

// Stop cancels all workers and waits for them to exit. Idempotent.
func (w *Worker) Stop() {
	w.mu.Lock()
	if w.cancel == nil {
		w.mu.Unlock()
		return // never started
	}
	w.cancel()
	w.cancel = nil
	w.mu.Unlock()

	w.wg.Wait()
}

// workerLoop is the main loop for a single worker goroutine.
func (w *Worker) workerLoop(ctx context.Context, id int) {
	defer w.wg.Done()
	defer func() {
		if r := recover(); r != nil {
			log.Printf("queue: worker %d panicked: %v", id, r)
		}
	}()

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		job, err := w.store.NextPending()
		if err != nil {
			log.Printf("queue: worker %d NextPending error: %v", id, err)
			time.Sleep(100 * time.Millisecond)
			continue
		}
		if job == nil {
			// Empty queue — backoff
			time.Sleep(100 * time.Millisecond)
			continue
		}

		// Acquire semaphore (B1 fix: use helper function so defer runs per-iteration)
		select {
		case w.sem <- struct{}{}:
		case <-ctx.Done():
			return
		}

		w.processWithSemaphore(ctx, job)
	}
}

// processWithSemaphore wraps processJob with semaphore release on return (B1 fix).
func (w *Worker) processWithSemaphore(ctx context.Context, job *Job) {
	defer func() { <-w.sem }()
	w.processJob(ctx, job)
}

// processJob calls the ProcessFunc and handles the result.
func (w *Worker) processJob(ctx context.Context, job *Job) {
	processErr := w.process(ctx, job)

	if processErr == nil {
		// Success
		if err := w.store.UpdateStatus(job.ID, StatusCompleted, job.Result, ""); err != nil {
			log.Printf("queue: worker UpdateStatus(completed) error: %v", err)
		}
		return
	}

	// Failure — mark as failed (increments retry_count in DB)
	if err := w.store.UpdateStatus(job.ID, StatusFailed, "", processErr.Error()); err != nil {
		log.Printf("queue: worker UpdateStatus(failed) error: %v", err)
		return
	}

	// Re-read job to get updated retry_count
	updatedJob, err := w.store.Get(job.ID)
	if err != nil || updatedJob == nil {
		log.Printf("queue: worker Get(job) after failed error: %v", err)
		return
	}

	if updatedJob.CanRetry() {
		if err := w.store.UpdateStatus(job.ID, StatusPending, "", ""); err != nil {
			log.Printf("queue: worker UpdateStatus(pending retry) error: %v", err)
		}
	} else {
		if err := w.store.UpdateStatus(job.ID, StatusDead, "", processErr.Error()); err != nil {
			log.Printf("queue: worker UpdateStatus(dead) error: %v", err)
		}
	}
}
