package main

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"mcp-memory/logger"
	"mcp-memory/worker"
)

type workerSystem struct {
	retainJobs  chan MemoryJob
	reflectJobs chan MemoryJob
	improveJobs chan MemoryJob // dedicated auto-improve channel
	retainPool  *worker.Pool
	reflectPool *worker.Pool
	improvePool *worker.Pool // 1 worker for auto-improve
	log         *logger.Logger
	stopOnce    sync.Once

	// Auto-improve state
	improve *autoImproveState
}

type autoImproveState struct {
	mu      sync.Mutex
	perBank map[string]*atomic.Int64 // retain counter per bank
	pending map[string]bool           // dedup: only one pending improve per bank
}

func newWorkerSystem(cfg Config, log *logger.Logger) *workerSystem {
	ws := &workerSystem{
		retainJobs:  make(chan MemoryJob, cfg.JobBufferSize),
		reflectJobs: make(chan MemoryJob, cfg.JobBufferSize),
		improveJobs: make(chan MemoryJob, cfg.JobBufferSize),
		log:         log,
	}
	if cfg.AutoImproveAfterN > 0 {
		ws.improve = &autoImproveState{
			perBank: make(map[string]*atomic.Int64),
			pending: make(map[string]bool),
		}
	}
	return ws
}

func (ws *workerSystem) start(s *Server) {
	cfg := s.config

	ws.retainPool = worker.NewPool("retain", cfg.RetainWorkers, ws.log, func(ctx context.Context) {
		select {
		case job, ok := <-ws.retainJobs:
			if !ok {
				return
			}
			// Check if job was already cancelled before processing
			select {
			case <-job.Ctx.Done():
				job.Result <- MemoryResult{Data: "", Err: fmt.Errorf("job cancelled: %w", job.Ctx.Err())}
				return
			default:
			}
			handle := s.metrics.retainDur.Start()
			result, err := s.backend.Retain(job.Ctx, job.Bank, job.Data)
			s.metrics.retainDur.Stop(handle)
			// Try to send result; if receiver timed out, just discard
			select {
			case job.Result <- MemoryResult{Data: result, Err: err}:
			default:
			}
			if err != nil {
				s.metrics.errorCalls.Inc()
				ws.log.Warn("retain failed", logger.Error(err), "bank", job.Bank)
			}
		case <-ctx.Done():
			return
		}
	})
	ws.retainPool.Start()

	ws.reflectPool = worker.NewPool("reflect", cfg.ReflectWorkers, ws.log, func(ctx context.Context) {
		select {
		case job, ok := <-ws.reflectJobs:
			if !ok {
				return
			}
			// Check if job was already cancelled before processing
			select {
			case <-job.Ctx.Done():
				job.Result <- MemoryResult{Data: "", Err: fmt.Errorf("job cancelled: %w", job.Ctx.Err())}
				return
			default:
			}
			handle := s.metrics.reflectDur.Start()
			result, err := s.backend.Reflect(job.Ctx, job.Bank, job.Data)
			s.metrics.reflectDur.Stop(handle)
			// Try to send result; if receiver timed out, just discard
			select {
			case job.Result <- MemoryResult{Data: result, Err: err}:
			default:
			}
			if err != nil {
				s.metrics.errorCalls.Inc()
				ws.log.Warn("reflect failed", logger.Error(err), "bank", job.Bank)
			}
		case <-ctx.Done():
			return
		}
	})
	ws.reflectPool.Start()

	// Auto-improve worker: 1 worker, dedicated channel, panic-recovered
	if ws.improve != nil {
		ws.improvePool = worker.NewPool("improve", 1, ws.log, func(ctx context.Context) {
			select {
			case job, ok := <-ws.improveJobs:
				if !ok {
					return
				}
				result, err := s.backend.Reflect(job.Ctx, job.Bank, "") // empty query = full improve
				ws.improve.mu.Lock()
				delete(ws.improve.pending, job.Bank)
				ws.improve.mu.Unlock()
				// Send result back if caller wants it (handleImprove)
				select {
				case job.Result <- MemoryResult{Data: result, Err: err}:
				default:
				}
				if err != nil {
					ws.log.Warn("auto-improve failed", "bank", job.Bank, logger.Error(err))
				} else {
					ws.log.Info("auto-improve completed", "bank", job.Bank)
				}
			case <-ctx.Done():
				return
			}
		})
		ws.improvePool.Start()
	}

	go s.sessionCleaner()
}

func (ws *workerSystem) stop() {
	ws.stopOnce.Do(func() {
		// Stop pools first — workers will drain in-flight jobs
		ws.retainPool.Stop()
		ws.reflectPool.Stop()
		if ws.improvePool != nil {
			ws.improvePool.Stop()
		}
		// Then close channels — no senders remain
		close(ws.retainJobs)
		close(ws.reflectJobs)
		close(ws.improveJobs)
	})
}

func (s *Server) queueJob(jobs chan MemoryJob, bank, method, data string) (*MemoryResult, error) {
	// Create a cancellable context for this job
	jobCtx, jobCancel := context.WithTimeout(context.Background(), s.config.QueueResponseTimeout)
	defer jobCancel()

	job := MemoryJob{
		Bank:   bank,
		Method: method,
		Data:   data,
		Result: make(chan MemoryResult, 1),
		Ctx:    jobCtx,
		Cancel: jobCancel,
	}

	select {
	case jobs <- job:
	case <-s.shutdown:
		jobCancel()
		return nil, fmt.Errorf("server is shutting down")
	case <-time.After(s.config.QueuePushTimeout):
		jobCancel()
		s.metrics.errorCalls.Inc()
		return nil, fmt.Errorf("memory server overloaded")
	}

	select {
	case result := <-job.Result:
		if result.Err != nil {
			s.metrics.errorCalls.Inc()
			return nil, result.Err
		}
		return &result, nil
	case <-jobCtx.Done():
		// Context expired — cancel the job so worker can abort early
		jobCancel()
		s.metrics.errorCalls.Inc()
		return nil, fmt.Errorf("operation timed out")
	}
}

func (s *Server) sessionCleaner() {
	defer func() {
		if r := recover(); r != nil {
			s.panics.Add(1)
			s.log.Error("session cleaner goroutine panicked", "panic", fmt.Sprintf("%v", r))
			s.alerts.Send(AlertCritical, fmt.Sprintf("Session cleaner panicked: %v", r), nil)
		}
	}()
	s.log.Info("goroutine_started", "name", "session_cleaner")
	defer s.log.Info("goroutine_stopped", "name", "session_cleaner")
	ticker := time.NewTicker(s.config.SessionCleanInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
		case <-s.shutdown:
			return
		}

		// Phase 1: Collect stale session IDs under read lock (minimal contention)
		type staleSession struct {
			id   string
			sess *MCPSession
		}
		var stale []staleSession
		s.sessionsMu.RLock()
		now := time.Now()
		for id, sess := range s.sessions {
			if now.Sub(sess.LastActive) > s.config.SessionIdleTimeout {
				stale = append(stale, staleSession{id: id, sess: sess})
			}
		}
		s.sessionsMu.RUnlock()

		// Phase 2: Close stale sessions without holding the lock
		for _, st := range stale {
			st.sess.Close()
			s.log.Info("session cleaned", "id", st.id[:8], "idle", now.Sub(st.sess.LastActive).Round(time.Second).String())
		}

		// Phase 3: Remove cleaned sessions under write lock (brief)
		if len(stale) > 0 {
			s.sessionsMu.Lock()
			for _, st := range stale {
				delete(s.sessions, st.id)
			}
			s.sessionsMu.Unlock()
		}

		// Phase 4: Update metrics (brief read lock)
		s.sessionsMu.RLock()
		sessionCount := len(s.sessions)
		s.sessionsMu.RUnlock()

		s.metrics.sessionGauge.Set(int64(sessionCount))
		if s.workers != nil {
			qd := len(s.workers.retainJobs) + len(s.workers.reflectJobs)
			s.metrics.queueGauge.Set(int64(qd))
			if qd > 50 {
				s.log.Warn("high queue depth", "depth", qd)
				s.alerts.Send(AlertWarn, fmt.Sprintf("High queue depth: %d", qd), map[string]interface{}{"depth": qd})
			}
		}
		if sessionCount > s.config.MaxSessions*9/10 {
			s.log.Warn("approaching session limit", "sessions", sessionCount, "max", s.config.MaxSessions)
			s.alerts.Send(AlertWarn, fmt.Sprintf("Sessions at %d/%d", sessionCount, s.config.MaxSessions), nil)
		}
	}
}

// maybeAutoImprove triggers graph improvement after N retains per bank.
// Only active when AUTO_IMPROVE_AFTER_N > 0 and backend is not Hindsight.
func (s *Server) maybeAutoImprove(bank string) {
	if s.config.AutoImproveAfterN <= 0 {
		return // disabled
	}
	ws := s.workers
	if ws.improve == nil {
		return
	}

	ws.improve.mu.Lock()
	defer ws.improve.mu.Unlock()

	// Initialize per-bank counter if needed
	if _, ok := ws.improve.perBank[bank]; !ok {
		ws.improve.perBank[bank] = &atomic.Int64{}
	}

	count := ws.improve.perBank[bank].Add(1)
	if count%int64(s.config.AutoImproveAfterN) == 0 && !ws.improve.pending[bank] {
		ws.improve.pending[bank] = true
		job := MemoryJob{Bank: bank, Method: "improve", Data: "", Result: make(chan MemoryResult, 1)}
		select {
		case ws.improveJobs <- job:
		default:
			ws.improve.pending[bank] = false // channel full, skip this cycle
		}
	}
}
