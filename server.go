package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"gopkg.in/natefinch/lumberjack.v2"
	"mcp-memory/backend"
	"mcp-memory/logger"
	"mcp-memory/metrics"
	"mcp-memory/queue"
)

type Server struct {
	mu    sync.RWMutex
	state ServiceState

	config Config
	svc    *services

	sessions   map[string]*MCPSession
	sessionsMu sync.RWMutex

	panics      atomic.Int64
	stopMonitor context.CancelFunc
	shutdown    chan struct{}
	shutdownOnce sync.Once
	alerts      *AlertClient

	startTime time.Time

	// Backend adapter — single dimension of variation
	backend backend.Backend

	// Observability
	log     *logger.Logger
	metrics *serverMetrics

	// Cognee infrastructure
	cogneeCtx    context.Context    // cancelled on Stop() for goroutine coordination
	cogneeCancel context.CancelFunc // called from Stop()

	// Queue infrastructure (M3)
	queueStore  *queue.Store  // SQLite-backed job store
	queueWorker *queue.Worker // Worker pool for async retain/reflect processing

	autoImproveWg sync.WaitGroup // tracks in-flight auto-improve goroutines (replaces cogneeWg)

	// Auto-reflect state (M4)
	reflectStates sync.Map // map[string]*reflectState — per-bank reflect tracking

	// Auto-improve state — periodic graph optimization
	improveState *autoImproveState // per-bank counters + in-flight tracking
	dataDir      string            // directory for improve_state.json persistence
}

type serverMetrics struct {
	recallCalls  *metrics.Counter
	retainCalls  *metrics.Counter
	reflectCalls *metrics.Counter
	errorCalls   *metrics.Counter
	retainDur    *metrics.Timer
	reflectDur   *metrics.Timer
	queueGauge   *metrics.Gauge
	sessionGauge *metrics.Gauge
	sseDrops     *metrics.Counter
	// Spec-required metrics (AC-M7.13)
	retainTotal    *metrics.Counter // memory.retain_total
	retainErrors   *metrics.Counter // memory.retain_errors
	recallTotal    *metrics.Counter // memory.recall_total
	reflectTotal   *metrics.Counter // memory.reflect_total
	improveTotal   *metrics.Counter // memory.improve_total
	forgetTotal    *metrics.Counter // memory.forget_total
	semaphoreGauge *metrics.Gauge   // memory.semaphore_in_use (Cognee only)
	cogneePending  *metrics.Gauge   // memory.cognee_jobs_pending (Cognee only)
}

func NewServer(config Config) *Server {
	wd, _ := os.Getwd()
	logWriter := &lumberjack.Logger{
		Filename:   filepath.Join(wd, "logs", "memory.log"),
		MaxSize:    10,
		MaxBackups: 3,
		MaxAge:     7,
		Compress:   true,
	}
	blog, _ := logger.NewBuf("memory", "info", logWriter, logger.WithSource())
	alertClient := NewAlertClient(config.AlertURL, config.AlertMode)

	backendCfg := backend.BackendConfig{
		Backend:               string(config.Backend),
		CogneePort:            config.CogneePort,
		BackendRetainTimeout:  config.BackendRetainTimeout,
		BackendRecallTimeout:  config.BackendRecallTimeout,
		BackendReflectTimeout: config.BackendReflectTimeout,
		CogneeRetainTimeout:   config.CogneeRetainTimeout,
		RetryAttempts:         config.RetryAttempts,
		RetryDelay:            config.RetryDelay,
		RetryMaxDelay:         config.RetryMaxDelay,
		TemporalCognify:       config.TemporalCognify,
		MemoryOnly:            config.MemoryOnly,
	}

	s := &Server{
		state:    StateStopped,
		config:   config,
		backend:  backend.New(backendCfg),
		svc:      newServices(config, blog, alertClient),
		sessions: make(map[string]*MCPSession),
		log:      blog,
		shutdown: make(chan struct{}),
		alerts:   alertClient,
		metrics: &serverMetrics{
			recallCalls:  metrics.NewCounter("memory.recall"),
			retainCalls:  metrics.NewCounter("memory.retain"),
			reflectCalls: metrics.NewCounter("memory.reflect"),
			errorCalls:   metrics.NewCounter("memory.errors"),
			retainDur:    metrics.NewTimer("memory.retain_duration"),
			reflectDur:   metrics.NewTimer("memory.reflect_duration"),
			queueGauge:   metrics.NewGauge("memory.queue_depth"),
			sessionGauge: metrics.NewGauge("memory.sessions"),
			sseDrops:     metrics.NewCounter("memory.sse_drops"),
			// Spec-required metrics (AC-M7.13)
			retainTotal:    metrics.NewCounter("memory.retain_total"),
			retainErrors:   metrics.NewCounter("memory.retain_errors"),
			recallTotal:    metrics.NewCounter("memory.recall_total"),
			reflectTotal:   metrics.NewCounter("memory.reflect_total"),
			improveTotal:   metrics.NewCounter("memory.improve_total"),
			forgetTotal:    metrics.NewCounter("memory.forget_total"),
			semaphoreGauge: metrics.NewGauge("memory.semaphore_in_use"),
			cogneePending:  metrics.NewGauge("memory.cognee_jobs_pending"),
		},
	}

	// Cognee context — created during NewServer, cancelled during Stop()
	s.cogneeCtx, s.cogneeCancel = context.WithCancel(context.Background())
	s.dataDir = getEnv("DATA_DIR", "./data")
	s.improveState = loadAutoImproveState(s.dataDir)

	return s
}

// processQueueJob is the ProcessFunc called by queue.Worker for each dequeued job.
// Signature must match queue.ProcessFunc exactly.
func (s *Server) processQueueJob(ctx context.Context, job *queue.Job) error {
	s.log.Info("job_dequeued", "job_id", job.ID, "type", job.Type, "bank", job.Bank)
	startTime := time.Now()

	switch job.Type {
	case "retain":
		// Create detached context with CogneeRetainTimeout.
		// Detached from ctx so shutdown doesn't abort long-running retain.
		detachedCtx, cancel := context.WithTimeout(context.Background(), s.config.CogneeRetainTimeout)
		defer cancel()

		timerHandle := s.metrics.retainDur.Start()
		result, err := s.backend.Retain(detachedCtx, job.Bank, job.Payload)
		s.metrics.retainDur.Stop(timerHandle)
		duration := time.Since(startTime)

		if err != nil {
			s.log.Error("queue: retain failed", "job_id", job.ID, "bank", job.Bank, "duration", duration, logger.Error(err))
			s.metrics.errorCalls.Inc()
			s.metrics.retainErrors.Inc()
			s.fireErrorWebhook(job.Bank, job.ID, err.Error(), "retain")
			return err
		}

		// Store result in job for UpdateStatus to persist
		job.Result = result
		s.log.Info("job_completed", "job_id", job.ID, "bank", job.Bank, "type", "retain", "duration_ms", duration.Milliseconds())

		// Trigger auto-improve after successful retain
		s.maybeAutoImprove(job.Bank)

		// M4: check auto-reflect triggers
		s.checkAutoReflect(job.Bank)

		return nil

	case "reflect":
		detachedCtx, cancel := context.WithTimeout(context.Background(), s.config.BackendReflectTimeout)
		defer cancel()

		startTime := time.Now()
		timerHandle := s.metrics.reflectDur.Start()
		_, err := s.backend.Reflect(detachedCtx, job.Bank, job.Payload)
		s.metrics.reflectDur.Stop(timerHandle)
		duration := time.Since(startTime)

		if err != nil {
			s.log.Error("queue: reflect failed", "job_id", job.ID, "bank", job.Bank, "duration", duration, logger.Error(err))
			s.metrics.errorCalls.Inc()
			return err
		}

		s.log.Info("job_completed", "job_id", job.ID, "bank", job.Bank, "type", "reflect", "duration_ms", duration.Milliseconds())
		return nil

	default:
		return fmt.Errorf("unknown job type: %s", job.Type)
	}
}

func (s *Server) Start() error {
	s.mu.Lock()
	if s.state == StateRunning {
		s.mu.Unlock()
		return nil
	}
	s.state = StateStarting
	s.startTime = time.Now()
	s.mu.Unlock()

	s.log.Info("starting services")

	if err := s.svc.start(); err != nil {
		s.mu.Lock()
		s.state = StateStopped
		s.mu.Unlock()
		s.log.Error("startup failed", logger.Error(err))
		return err
	}
	s.svc.savePids() // Persist child PIDs for crash recovery

	go s.sessionCleaner()

	ctx, cancel := context.WithCancel(context.Background())
	go s.svc.monitor(ctx, &s.panics)
	s.stopMonitor = cancel

	// Wait for all three services to report healthy before declaring ready
	s.log.Info("waiting for services to become healthy...")
	if err := s.svc.waitAllHealthy(s.config.StartTimeout); err != nil {
		s.log.Error("services not healthy after startup", logger.Error(err))
		s.mu.Lock()
		s.state = StateDegraded
		s.mu.Unlock()
		s.alerts.Send(AlertError, "Server started in degraded mode", map[string]interface{}{"error": err.Error()})
	} else {
		s.mu.Lock()
		s.state = StateRunning
		s.mu.Unlock()
		s.alerts.Send(AlertInfo, "Server started — all services healthy", nil)
	}

	// Allocate queue store — only after Cognee is healthy
	store, err := queue.NewStore(queue.StoreConfig{
		DBPath:     s.config.QueueDBPath,
		MaxPending: s.config.QueueMaxPending,
		JobTTL:     s.config.QueueJobTTL,
	})
	if err != nil {
		return fmt.Errorf("queue store: %w", err)
	}
	s.queueStore = store

	// Create ProcessFunc closure
	processFunc := func(ctx context.Context, job *queue.Job) error {
		return s.processQueueJob(ctx, job)
	}

	// Create worker pool
	worker, err := queue.NewWorker(queue.WorkerConfig{
		Store:   s.queueStore,
		Process: processFunc,
		Count:   s.config.QueueWorkerCount,
		SemSize: s.config.QueueMaxConcurrent,
		OnDead: func(job *queue.Job) {
			s.log.Error("job_dead", "job_id", job.ID, "bank", job.Bank, "type", job.Type,
				"error", job.Error, "retry_count", job.RetryCount, "max_retries", job.MaxRetries)
			s.fireErrorWebhook(job.Bank, job.ID, job.Error, job.Type)
		},
	})
	if err != nil {
		s.queueStore.Close()
		return fmt.Errorf("queue worker: %w", err)
	}
	s.queueWorker = worker

	// Recover orphaned jobs from previous crash
	recovered, _ := s.queueStore.Recover()
	if recovered > 0 {
		s.log.Info("queue: recovered orphaned jobs", "count", recovered)
	}

	// Start worker pool
	s.queueWorker.Start(context.Background())

	// Start TTL cleanup
	s.queueStore.StartTTLCleanup(context.Background(), s.config.QueueTTLInterval)

	s.log.Info("services started", "uptime", time.Since(s.startTime).String(), "state", s.state)
	return nil
}

func (s *Server) Stop() {
	s.mu.Lock()
	if s.state == StateStopped {
		s.mu.Unlock()
		return
	}
	s.mu.Unlock()

	s.log.Info("shutting down")
	s.alerts.Send(AlertWarn, "Server shutting down", nil)

	if s.stopMonitor != nil {
		s.stopMonitor()
	}

	// Signal session cleaner goroutine to exit (once)
	s.shutdownOnce.Do(func() { close(s.shutdown) })

	// M3: Stop queue workers and drain in-flight jobs
	if s.queueWorker != nil {
		s.log.Info("stopping queue workers...")
		s.queueWorker.Stop()
		s.log.Info("queue workers stopped")
	}

	// M3: Wait for auto-improve goroutines
	s.autoImproveWg.Wait()

	// Cancel Cognee context (belt-and-suspenders for any remaining detached work)
	if s.cogneeCancel != nil {
		s.cogneeCancel()
	}

	// M3: Close queue store
	if s.queueStore != nil {
		if err := s.queueStore.Close(); err != nil {
			s.log.Error("queue store close error", logger.Error(err))
		}
	}

	s.sessionsMu.Lock()
	for id, sess := range s.sessions {
		sess.Close()
		delete(s.sessions, id)
	}
	s.sessionsMu.Unlock()

	s.svc.stop()
	s.svc.clearPids()

	s.mu.Lock()
	s.state = StateStopped
	s.mu.Unlock()
	s.log.Info("shutdown complete")
}
