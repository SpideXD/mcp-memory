package main

import (
	"context"
	"fmt"
	"sync"
	"time"

	"mcp-memory/logger"
	"mcp-memory/worker"
)

type workerSystem struct {
	retainJobs  chan MemoryJob
	reflectJobs chan MemoryJob
	retainPool  *worker.Pool
	reflectPool *worker.Pool
	log         *logger.Logger
	stopOnce    sync.Once
}

func newWorkerSystem(cfg Config, log *logger.Logger) *workerSystem {
	return &workerSystem{
		retainJobs:  make(chan MemoryJob, cfg.JobBufferSize),
		reflectJobs: make(chan MemoryJob, cfg.JobBufferSize),
		log:         log,
	}
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
			result, err := s.retainAPIWithContext(job.Ctx, job.Bank, job.Data)
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
			result, err := s.reflectAPIWithContext(job.Ctx, job.Bank, job.Data)
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

	go s.sessionCleaner()
}

func (ws *workerSystem) stop() {
	ws.stopOnce.Do(func() {
		// Stop pools first — workers will drain in-flight jobs
		ws.retainPool.Stop()
		ws.reflectPool.Stop()
		// Then close channels — no senders remain
		close(ws.retainJobs)
		close(ws.reflectJobs)
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
			s.log.Error("session cleaner panic", "panic", fmt.Sprintf("%v", r))
			s.alerts.Send(AlertCritical, fmt.Sprintf("Session cleaner panicked: %v", r), nil)
		}
	}()
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
