package main

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"gopkg.in/natefinch/lumberjack.v2"
	"mcp-memory/backend"
	"mcp-memory/logger"
	"mcp-memory/metrics"
)

type Server struct {
	mu    sync.RWMutex
	state ServiceState

	config Config
	svc    *services

	sessions   map[string]*MCPSession
	sessionsMu sync.RWMutex

	workers     *workerSystem
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

	// Cognee-only fields — nil when BACKEND=hindsight
	cogneeSemaphore chan struct{} // buffered, bounds concurrent retains
	jobTracker      *jobTracker  // in-memory job result map + TTL cleanup
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
		Backend:                 string(config.Backend),
		HindsightPort:           config.HindsightPort,
		CogneePort:              config.CogneePort,
		BackendRetainTimeout:    config.BackendRetainTimeout,
		BackendRecallTimeout:    config.BackendRecallTimeout,
		BackendReflectTimeout:   config.BackendReflectTimeout,
		RetryAttempts:           config.RetryAttempts,
		RetryDelay:              config.RetryDelay,
		RetryMaxDelay:           config.RetryMaxDelay,
		CircuitBreakerThreshold: config.CircuitBreakerThreshold,
		CircuitBreakerCooldown:  config.CircuitBreakerCooldown,
	}

	s := &Server{
		state:    StateStopped,
		config:   config,
		backend:  backend.New(backendCfg),
		svc:      newServices(config, blog, alertClient),
		sessions: make(map[string]*MCPSession),
		workers:  newWorkerSystem(config, blog),
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
		},
	}

	// Cognee infrastructure exists iff backend is async
	if !s.backend.IsSync() {
		s.cogneeSemaphore = make(chan struct{}, config.CogneeMaxConcurrentRetains)
		s.jobTracker = newJobTracker(30 * time.Minute)
		go s.jobTrackerCleanup()
	}

	return s
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
	s.svc.savePids()  // Persist child PIDs for crash recovery

	s.workers.start(s)

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

	s.workers.stop()

	s.sessionsMu.Lock()
	for id, sess := range s.sessions {
		sess.Close()
		delete(s.sessions, id)
	}
	s.sessionsMu.Unlock()

	s.svc.stop()
	defer s.svc.clearPids()  // Ensure cleanup even if stop panics

	s.mu.Lock()
	s.state = StateStopped
	s.mu.Unlock()
	s.log.Info("shutdown complete")
}
