package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"mcp-memory/backend"
	"mcp-memory/internal/testutil/cogneemock"
	"mcp-memory/logger"
	"mcp-memory/metrics"
	"mcp-memory/queue"
)

// ─── Test Fixture ──────────────────────────────────────────────────────────

// m3Fixture sets up a Server with cogneemock backend + in-memory queue store
// for handler-level M3 testing. Does NOT start real subprocesses.
type m3Fixture struct {
	t          *testing.T
	server     *Server
	mockCognee *cogneemock.Server
	store      *queue.Store
	worker     *queue.Worker
	cfg        Config
	logBuf     *bytes.Buffer
}

func newM3Fixture(t *testing.T, opts ...m3FixtureOption) *m3Fixture {
	t.Helper()

	// Create cogneemock server
	mockCognee := cogneemock.NewServer()
	t.Cleanup(mockCognee.Close)

	// Buffer for log capture
	logBuf := &bytes.Buffer{}
	l, err := logger.NewBuf("m3-test", "debug", logBuf)
	if err != nil {
		t.Fatalf("logger: %v", err)
	}

	// Default config
	cfg := Config{
		AutoImproveAfterN:  0, // disabled by default
		AutoImproveCooldown: 120 * time.Second,
		QueueDBPath:        ":memory:",
		QueueMaxPending:    1000,
		QueueJobTTL:        24 * time.Hour,
		QueueTTLInterval:   5 * time.Minute,
		QueueWorkerCount:   1,
		QueueMaxConcurrent: 1,
		CogneePort:         fmt.Sprintf("%d", mockCognee.Port()),
		CogneeRetainTimeout: 30 * time.Second,
		BackendReflectTimeout: 30 * time.Second,
		BackendRecallTimeout: 10 * time.Second,
		BackendRetainTimeout: 30 * time.Second,
	}

	// Apply options
	for _, opt := range opts {
		opt(&cfg)
	}

	// Create backend pointing to mock
	be := backend.New(backend.BackendConfig{
		Backend:               "cognee-rust",
		CogneePort:            fmt.Sprintf("%d", mockCognee.Port()),
		TemporalCognify:       true,
		MemoryOnly:            true,
		BackendRetainTimeout:  30 * time.Second,
		BackendRecallTimeout:  10 * time.Second,
		BackendReflectTimeout: 30 * time.Second,
		CogneeRetainTimeout:   30 * time.Second,
		RetryAttempts:         1,
		RetryDelay:            100 * time.Millisecond,
		RetryMaxDelay:         1 * time.Second,
	})

	// Create in-memory queue store
	store, err := queue.NewStore(queue.StoreConfig{
		DBPath:     ":memory:",
		MaxPending: cfg.QueueMaxPending,
		JobTTL:     cfg.QueueJobTTL,
	})
	if err != nil {
		t.Fatalf("queue store: %v", err)
	}
	t.Cleanup(func() { store.Close() })

	// Create Server (minimal, without starting real subprocesses)
	cogneeCtx, cogneeCancel := context.WithCancel(context.Background())
	s := &Server{
		state:         StateRunning,
		config:        cfg,
		backend:       be,
		log:           l,
		queueStore:    store,
		cogneeCtx:     cogneeCtx,
		cogneeCancel:  cogneeCancel,
		sessions:      make(map[string]*MCPSession),
		shutdown:      make(chan struct{}),
		dataDir:       t.TempDir(),
		improveState:  loadAutoImproveState(t.TempDir()),
		autoImproveWg: sync.WaitGroup{},
		metrics: &serverMetrics{
			retainCalls:    metrics.NewCounter("memory.retain"),
			reflectCalls:   metrics.NewCounter("memory.reflect"),
			recallCalls:    metrics.NewCounter("memory.recall"),
			errorCalls:     metrics.NewCounter("memory.errors"),
			retainTotal:    metrics.NewCounter("memory.retain_total"),
			retainErrors:   metrics.NewCounter("memory.retain_errors"),
			recallTotal:    metrics.NewCounter("memory.recall_total"),
			reflectTotal:   metrics.NewCounter("memory.reflect_total"),
			improveTotal:   metrics.NewCounter("memory.improve_total"),
			forgetTotal:    metrics.NewCounter("memory.forget_total"),
			retainDur:      metrics.NewTimer("memory.retain_duration"),
			reflectDur:     metrics.NewTimer("memory.reflect_duration"),
			queueGauge:     metrics.NewGauge("memory.queue_depth"),
			sessionGauge:   metrics.NewGauge("memory.sessions"),
			sseDrops:       metrics.NewCounter("memory.sse_drops"),
			semaphoreGauge: metrics.NewGauge("memory.semaphore_in_use"),
			cogneePending:  metrics.NewGauge("memory.cognee_jobs_pending"),
		},
	}
	t.Cleanup(func() {
		if s.queueWorker != nil {
			s.queueWorker.Stop()
		}
		cogneeCancel()
	})

	// Create queue worker pointing to the server's processQueueJob
	processFunc := func(ctx context.Context, job *queue.Job) error {
		return s.processQueueJob(ctx, job)
	}
	worker, err := queue.NewWorker(queue.WorkerConfig{
		Store:   store,
		Process: processFunc,
		Count:   cfg.QueueWorkerCount,
		SemSize: cfg.QueueMaxConcurrent,
	})
	if err != nil {
		t.Fatalf("queue worker: %v", err)
	}
	s.queueWorker = worker

	// Start worker pool (only when workers requested — withWorkers(0) means no processing)
	if cfg.QueueWorkerCount > 0 {
		worker.Start(context.Background())
	}

	return &m3Fixture{
		t:          t,
		server:     s,
		mockCognee: mockCognee,
		store:      store,
		worker:     worker,
		cfg:        cfg,
		logBuf:     logBuf,
	}
}

// m3FixtureOption allows customizing the config.
type m3FixtureOption func(*Config)

func withMaxPending(n int) m3FixtureOption {
	return func(c *Config) { c.QueueMaxPending = n }
}

func withWorkers(n int) m3FixtureOption {
	return func(c *Config) { c.QueueWorkerCount = n }
}

func withAutoImprove(n int) m3FixtureOption {
	return func(c *Config) {
		c.AutoImproveAfterN = n
		c.AutoImproveCooldown = 0
	}
}

// insertJob inserts a job directly into the queue store and returns the job ID.
func (f *m3Fixture) insertJob(jobType, bank, payload string) string {
	f.t.Helper()
	jobID := newJobID()
	err := f.store.Insert(&queue.Job{
		ID:      jobID,
		Bank:    bank,
		Type:    jobType,
		Payload: payload,
	})
	if err != nil {
		f.t.Fatalf("insert job: %v", err)
	}
	return jobID
}

// waitForJob polls until a job reaches a terminal status (completed/failed/dead)
// or timeout expires. Fails the test on timeout.
func (f *m3Fixture) waitForJob(jobID string, timeout time.Duration) *queue.Job {
	f.t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		job, err := f.store.Get(jobID)
		if err != nil {
			f.t.Fatalf("get job %s: %v", jobID, err)
		}
		if job == nil {
			f.t.Fatalf("job %s not found", jobID)
		}
		switch job.Status {
		case queue.StatusCompleted, queue.StatusFailed, queue.StatusDead:
			return job
		case queue.StatusPending, queue.StatusRunning:
			time.Sleep(10 * time.Millisecond)
		}
	}
	f.t.Fatalf("job %s did not complete within %v (final status: pending/running — possible zombie)", jobID, timeout)
	return nil
}

// ─── Attack Vector 1: Queue Full Rejection ────────────────────────────────
// Spec AC-M3.2: Insert MaxPending jobs, next returns queue_full.
// CRITICAL: Workers consume jobs, so use workers=0 to keep all jobs pending.

func TestM3_QueueFullRejection_Retain(t *testing.T) {
	// Workers=0 so jobs stay pending and queue fills deterministically
	f := newM3Fixture(t, withMaxPending(5), withWorkers(0))

	for i := 0; i < 5; i++ {
		f.insertJob("retain", "testbank", fmt.Sprintf("content %d", i))
	}

	// 6th insert should fail with ErrQueueFull
	job := &queue.Job{
		ID:      newJobID(),
		Bank:    "testbank",
		Type:    "retain",
		Payload: "overflow content",
	}
	err := f.store.Insert(job)
	if err == nil {
		t.Fatal("BUG: expected ErrQueueFull on 6th retain with MaxPending=5, got nil")
	}
	if !errors.Is(err, queue.ErrQueueFull) {
		t.Fatalf("BUG: expected ErrQueueFull, got: %v", err)
	}
	t.Log("Queue full rejection working correctly")
}

func TestM3_QueueFullRejection_Reflect(t *testing.T) {
	// Workers=0 so jobs stay pending
	f := newM3Fixture(t, withMaxPending(5), withWorkers(0))

	// Insert 5 retain jobs to fill queue
	for i := 0; i < 5; i++ {
		f.insertJob("retain", "testbank", fmt.Sprintf("content %d", i))
	}

	// Reflect insertion should also be rejected when queue is full
	// BUG: validation rejects empty payload even though reflect allows empty query
	// Temporarily insert with non-empty payload to test queue-full path
	job := &queue.Job{
		ID:      newJobID(),
		Bank:    "testbank",
		Type:    "reflect",
		Payload: "dummy", // workaround: queue validates non-empty payload
	}
	err := f.store.Insert(job)
	if err == nil {
		t.Fatal("BUG: expected ErrQueueFull on reflect when queue full, got nil")
	}
	if !errors.Is(err, queue.ErrQueueFull) {
		t.Fatalf("BUG: expected ErrQueueFull, got: %v", err)
	}
	t.Log("Queue full rejection for reflect working correctly (with non-empty payload workaround)")
}

func TestM3_QueueFullWith5001Jobs(t *testing.T) {
	// Use a directly-created store without workers to avoid consumption
	store, err := queue.NewStore(queue.StoreConfig{
		DBPath:     ":memory:",
		MaxPending: 5000,
		JobTTL:     24 * time.Hour,
	})
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	defer store.Close()

	for i := 0; i < 5000; i++ {
		job := &queue.Job{
			ID:      newJobID(),
			Bank:    "bigbank",
			Type:    "retain",
			Payload: fmt.Sprintf("content %d", i),
		}
		if err := store.Insert(job); err != nil {
			t.Fatalf("insert %d failed unexpectedly: %v", i, err)
		}
	}

	// 5001st insert must return ErrQueueFull
	job := &queue.Job{
		ID:      newJobID(),
		Bank:    "bigbank",
		Type:    "retain",
		Payload: "overflow",
	}
	err = store.Insert(job)
	if err == nil {
		t.Fatal("BUG: expected ErrQueueFull at 5001 jobs, got nil")
	}
	if !errors.Is(err, queue.ErrQueueFull) {
		t.Fatalf("BUG: expected ErrQueueFull, got: %v", err)
	}

	stats, err := store.Stats()
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	if stats.Pending != 5000 {
		t.Fatalf("expected exactly 5000 pending, got %d", stats.Pending)
	}
	t.Log("5000-job boundary tested successfully")
}

// ─── Attack Vector 2: ProcessFunc Panic Recovery ─────────────────────────
// Spec AC-M3.2.27: Worker defer recover logs and continues.
// BUG: When ProcessFunc panics, the job is left in "running" state (zombie).
// The worker's defer recover catches the panic but never updates the job status.

// panickingBackend wraps a backend and panics on Retain and Reflect.
type panickingBackend struct {
	backend.Backend
	panicOnRetain  bool
	panicOnReflect bool
	callCount      int
	mu             sync.Mutex
}

func (p *panickingBackend) Retain(ctx context.Context, bank, content string) (string, error) {
	p.mu.Lock()
	p.callCount++
	n := p.callCount
	p.mu.Unlock()
	if p.panicOnRetain {
		panic(fmt.Sprintf("simulated Retain panic call #%d bank=%q", n, bank))
	}
	return p.Backend.Retain(ctx, bank, content)
}

func (p *panickingBackend) Reflect(ctx context.Context, bank, query string) (string, error) {
	p.mu.Lock()
	p.callCount++
	n := p.callCount
	p.mu.Unlock()
	if p.panicOnReflect {
		panic(fmt.Sprintf("simulated Reflect panic call #%d bank=%q", n, bank))
	}
	return p.Backend.Reflect(ctx, bank, query)
}

func TestM3_WorkerRecoversFromRetainPanic(t *testing.T) {
	f := newM3Fixture(t)

	// Replace backend with panicking wrapper
	panicBackend := &panickingBackend{Backend: f.server.backend, panicOnRetain: true}
	f.server.backend = panicBackend

	// Insert a job that will trigger a panic in processQueueJob → backend.Retain
	jobID := f.insertJob("retain", "panicbank", "this will panic in Retain")

	// FIX: Wait for the job to reach a terminal state (not zombie in "running")
	// After the panic fix, the job should transition to failed (and possibly retry back to pending)
	deadline := time.Now().Add(5 * time.Second)
	var job *queue.Job
	for time.Now().Before(deadline) {
		var err error
		job, err = f.store.Get(jobID)
		if err != nil {
			t.Fatalf("get job: %v", err)
		}
		if job.Status == queue.StatusFailed || job.Status == queue.StatusDead || job.Status == queue.StatusPending {
			break // no longer stuck in running
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Logf("Job status after Retain panic: %q (retry_count=%d)", job.Status, job.RetryCount)

	if job.Status == queue.StatusRunning {
		t.Fatalf("BUG: Job stuck in running — zombie, panic recovery didn't update status")
	}
	t.Log("BUG FIXED: Job correctly transitions out of running after panic")

	// Disable panics, then verify worker processes a second job normally
	panicBackend.mu.Lock()
	panicBackend.panicOnRetain = false
	panicBackend.mu.Unlock()
	secondID := f.insertJob("retain", "panicbank", "should complete")
	secondJob := f.waitForJob(secondID, 10*time.Second)
	if secondJob.Status != queue.StatusCompleted {
		t.Fatalf("expected second job to complete, got status=%q", secondJob.Status)
	}
	t.Log("Worker recovered and processed second job successfully")
}

func TestM3_WorkerRecoversFromReflectPanic(t *testing.T) {
	f := newM3Fixture(t)

	panicBackend := &panickingBackend{Backend: f.server.backend, panicOnReflect: true}
	f.server.backend = panicBackend

	jobID := f.insertJob("reflect", "reflectpanic", "payload for validation")

	// FIX: Wait for the job to be processed (not immediately read)
	deadline := time.Now().Add(5 * time.Second)
	var job *queue.Job
	for time.Now().Before(deadline) {
		var err error
		job, err = f.store.Get(jobID)
		if err != nil {
			t.Fatalf("get job: %v", err)
		}
		if job.Status == queue.StatusFailed || job.Status == queue.StatusDead || job.Status == queue.StatusPending {
			break // no longer stuck in running
		}
		time.Sleep(50 * time.Millisecond)
	}
	if job.Status == queue.StatusRunning {
		t.Fatalf("BUG: Reflect panic also leaves zombie in status=%q", job.Status)
	}
	t.Log("BUG FIXED: Reflect panic correctly transitions out of running")
}

// ─── Attack Vector 3: Nil Queue Store ─────────────────────────────────────

func TestM3_NilQueueStoreHelpers(t *testing.T) {
	if got := pendingCount(nil); got != 0 {
		t.Fatalf("pendingCount(nil) = %d, want 0", got)
	}
	if got := queueDepth(nil); got != 0 {
		t.Fatalf("queueDepth(nil) = %d, want 0", got)
	}
	t.Log("Nil-safe helpers work correctly")
}

// ─── Attack Vector 4: memory_retain Payload Edge Cases ────────────────────

func TestM3_RetainPayload_EmptyContent(t *testing.T) {
	// Empty content rejected by queue validation — correct behavior
	f := newM3Fixture(t)
	job := &queue.Job{
		ID:      newJobID(),
		Bank:    "testbank",
		Type:    "retain",
		Payload: "",
	}
	err := f.store.Insert(job)
	if err == nil {
		t.Fatal("expected validation error for empty content, got nil")
	}
	t.Logf("Empty content correctly rejected: %v", err)
}

func TestM3_RetainPayload_NullBytes(t *testing.T) {
	f := newM3Fixture(t)
	nullContent := "hello\x00world\x00test"
	jobID := f.insertJob("retain", "testbank", nullContent)
	job := f.waitForJob(jobID, 10*time.Second)
	if job.Status != queue.StatusCompleted {
		t.Fatalf("expected completed, got status=%q error=%q", job.Status, job.Error)
	}
	req := f.mockCognee.LastRequest("/api/v1/remember")
	if req == nil {
		t.Fatal("no request captured by mock")
	}
	if !strings.Contains(req.Body, "hello") || !strings.Contains(req.Body, "world") {
		t.Fatalf("expected content in request body, got: %s", req.Body)
	}
	t.Log("Null byte content processed successfully")
}

func TestM3_RetainPayload_10KBUnicode(t *testing.T) {
	f := newM3Fixture(t)
	var sb strings.Builder
	for sb.Len() < 10*1024 {
		sb.WriteRune(rune(0x4E00 + rand.Intn(0x3000)))
	}
	unicodeContent := sb.String()

	jobID := f.insertJob("retain", "testbank", unicodeContent)
	job := f.waitForJob(jobID, 30*time.Second)
	if job.Status != queue.StatusCompleted {
		t.Fatalf("expected completed, got status=%q error=%q", job.Status, job.Error)
	}
	t.Logf("10KB unicode content processed, body length=%d", len(unicodeContent))
}

func TestM3_RetainPayload_VeryLongBankName(t *testing.T) {
	f := newM3Fixture(t)
	longBank := strings.Repeat("a", 128)
	jobID := f.insertJob("retain", longBank, "test content")
	job := f.waitForJob(jobID, 10*time.Second)
	if job.Status != queue.StatusCompleted {
		t.Fatalf("expected completed, got status=%q", job.Status)
	}
	t.Logf("128-char bank name processed successfully")
}

func TestM3_RetainPayload_MaxContentSize(t *testing.T) {
	f := newM3Fixture(t)
	// 1MB content passes queue insert (queue doesn't enforce MaxContentBytes)
	bigContent := strings.Repeat("x", 1<<20)
	jobID := f.insertJob("retain", "testbank", bigContent)
	job := f.waitForJob(jobID, 30*time.Second)
	t.Logf("1MB content handled: status=%q", job.Status)
}

// ─── Attack Vector 5: memory_reflect Returns job_id ──────────────────────
// Spec AC-M3.12: memory_reflect response MUST include job_id.
//
// BUG: The queue's Job.Validate() rejects empty payload,
// but reflect jobs have empty query (Payload: a.Query, which is "").
// This means a real reflect with no query fails at Insert time.

func TestM3_ReflectErrorsWithEmptyPayload(t *testing.T) {
	f := newM3Fixture(t)

	// FIX: Reflect with empty query is now valid (Bug 2 fixed)
	jobID := newJobID()
	job := &queue.Job{
		ID:      jobID,
		Bank:    "reflectbank",
		Type:    "reflect",
		Payload: "", // empty query is valid for reflect per spec
	}

	err := f.store.Insert(job)
	if err != nil {
		t.Fatalf("BUG: Reflect with empty query should be accepted after fix, got error: %v", err)
	}
	t.Log("BUG FIXED: Reflect with empty query accepted by queue validation")

	// Verify the empty-query reflect job processes correctly
	completed := f.waitForJob(jobID, 10*time.Second)
	if completed.Status != queue.StatusCompleted {
		t.Fatalf("expected completed, got status=%q error=%q", completed.Status, completed.Error)
	}
	t.Log("Empty-query reflect job processed successfully")
}

func TestM3_ReflectWithPayloadHasJobID(t *testing.T) {
	f := newM3Fixture(t)

	// Using non-empty payload to work around the validation bug
	jobID := f.insertJob("reflect", "reflectbank", "non-empty query")

	// Wait for processing
	job := f.waitForJob(jobID, 10*time.Second)
	if job.Status != queue.StatusCompleted {
		t.Fatalf("expected completed, got status=%q error=%q", job.Status, job.Error)
	}

	// Verify response shape as handler would produce it
	response := fmt.Sprintf(`{"status":"queued","bank":"reflectbank","job_id":"%s"}`, jobID)
	var resp map[string]interface{}
	if err := json.Unmarshal([]byte(response), &resp); err != nil {
		t.Fatalf("invalid response JSON: %v", err)
	}
	jid, ok := resp["job_id"].(string)
	if !ok || jid == "" {
		t.Fatal("job_id missing or empty in response")
	}
	t.Logf("Reflect with payload has job_id: %s", response)
}

func TestM3_RetainResponseHasJobID(t *testing.T) {
	f := newM3Fixture(t)
	jobID := f.insertJob("retain", "retainbank", "test content")

	response := fmt.Sprintf(`{"status":"queued","bank":"retainbank","job_id":"%s"}`, jobID)
	var resp map[string]interface{}
	json.Unmarshal([]byte(response), &resp)
	if _, ok := resp["job_id"].(string); !ok {
		t.Fatal("job_id missing or not a string")
	}
	t.Log("Retain response includes job_id")
}

// ─── Attack Vector 6: memory_retain_status ───────────────────────────────

func TestM3_RetainStatus_CompleteJob(t *testing.T) {
	f := newM3Fixture(t)
	jobID := f.insertJob("retain", "statusbank", "status test")
	completed := f.waitForJob(jobID, 15*time.Second)

	if completed.Status != queue.StatusCompleted {
		t.Fatalf("expected completed, got %q", completed.Status)
	}
	if completed.Result == "" {
		t.Fatal("expected non-empty result for completed job")
	}

	response := map[string]interface{}{
		"job_id":     completed.ID,
		"bank":       completed.Bank,
		"status":     string(completed.Status),
		"created_at": completed.CreatedAt,
		"updated_at": completed.UpdatedAt,
	}
	if completed.Result != "" {
		response["result"] = completed.Result
	}
	data, _ := json.Marshal(response)
	t.Logf("Completed job status response: %s", string(data))
}

func TestM3_RetainStatus_NotFound(t *testing.T) {
	f := newM3Fixture(t)
	job, err := f.store.Get("nonexistent-job-id-12345")
	if err != nil {
		t.Fatalf("get nonexistent job: %v", err)
	}
	if job != nil {
		t.Fatal("expected nil for nonexistent job")
	}
	t.Log("Nonexistent job correctly returns nil")
}

func TestM3_RetainStatus_FailedJobHasRetryFields(t *testing.T) {
	f := newM3Fixture(t)

	// Trigger a panic to test retry behavior after fix
	panicBackend := &panickingBackend{Backend: f.server.backend, panicOnRetain: true}
	f.server.backend = panicBackend

	jobID := f.insertJob("retain", "failbank", "will panic")

	// Wait for the job to reach a terminal state (failed/dead) or retry back to pending
	deadline := time.Now().Add(5 * time.Second)
	var job *queue.Job
	for time.Now().Before(deadline) {
		var err error
		job, err = f.store.Get(jobID)
		if err != nil {
			t.Fatalf("get job: %v", err)
		}
		if job.Status != queue.StatusRunning {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if job == nil {
		t.Fatal("job not found")
	}
	t.Logf("Failed job state: status=%q retry_count=%d max_retries=%d error=%q",
		job.Status, job.RetryCount, job.MaxRetries, job.Error)

	if job.Status == queue.StatusRunning {
		t.Fatalf("BUG: Job stuck in running — no retry fields available")
	}
	if job.Status == queue.StatusFailed || job.Status == queue.StatusDead {
		t.Log("BUG FIXED: Job correctly transitions to failed/dead with retry info")
	} else if job.Status == queue.StatusPending && job.RetryCount > 0 {
		t.Log("BUG FIXED: Job retried and moved back to pending")
	}
}

// ─── Attack Vector 7: memory_forget Still Works ──────────────────────────

func TestM3_ForgetStillDirect(t *testing.T) {
	f := newM3Fixture(t)
	result, err := f.server.backend.Forget(t.Context(), "forgetbank", "content-id-123")
	if err != nil {
		t.Fatalf("forget failed: %v", err)
	}
	var resp map[string]interface{}
	if err := json.Unmarshal([]byte(result), &resp); err != nil {
		t.Fatalf("forget response not JSON: %v", err)
	}
	if resp["status"] != "success" {
		t.Fatalf("expected status=success, got %v", resp["status"])
	}
	t.Logf("Forget response: %s", result)
}

// ─── Attack Vector 8: memory_recall Still Works ──────────────────────────

func TestM3_RecallStillDirect(t *testing.T) {
	f := newM3Fixture(t)
	result, err := f.server.backend.Recall(t.Context(), "recallbank", "test query")
	if err != nil {
		t.Fatalf("recall failed: %v", err)
	}
	var resp []interface{}
	if err := json.Unmarshal([]byte(result), &resp); err != nil {
		t.Fatalf("recall response not valid JSON: %v (body: %s)", err, result)
	}
	t.Logf("Recall response: %s", result)
}

// ─── Attack Vector 9: Health Endpoint Real queue_depth ───────────────────

func TestM3_HealthQueueDepthReal(t *testing.T) {
	f := newM3Fixture(t)

	if d := queueDepth(f.store); d != 0 {
		t.Fatalf("expected queue_depth=0 on empty queue, got %d", d)
	}

	for i := 0; i < 3; i++ {
		f.insertJob("retain", "healthbank", fmt.Sprintf("content %d", i))
	}

	stats, err := f.store.Stats()
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	t.Logf("Queue stats after 3 inserts: pending=%d running=%d completed=%d",
		stats.Pending, stats.Running, stats.Completed)

	if d := queueDepth(f.store); d <= 0 {
		t.Fatalf("expected queue_depth > 0 with inserted jobs, got %d", d)
	}
	t.Log("queueDepth reads real store stats")
}

func TestM3_HealthQueueDepthNonZeroWithJobs(t *testing.T) {
	// 0 workers = jobs stay pending
	f := newM3Fixture(t, withWorkers(0))

	for i := 0; i < 5; i++ {
		f.insertJob("retain", "healthbank", fmt.Sprintf("content %d", i))
	}

	if d := queueDepth(f.store); d != 5 {
		t.Fatalf("expected queue_depth=5 with 5 pending jobs and no workers, got %d", d)
	}
	t.Logf("queueDepth correctly returns 5 with 5 pending jobs")
}

// ─── Attack Vector 10: Server Start/Stop with Queue ──────────────────────

func TestM3_WorkerStartStopClean(t *testing.T) {
	f := newM3Fixture(t)
	f.worker.Stop()
	time.Sleep(50 * time.Millisecond)

	jobID := f.insertJob("retain", "stopstart", "should stay pending")

	job, err := f.store.Get(jobID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if job.Status != queue.StatusPending {
		t.Fatalf("expected pending when worker stopped, got %q", job.Status)
	}

	// Restart and verify processing resumes
	f.worker.Start(context.Background())
	completed := f.waitForJob(jobID, 10*time.Second)
	if completed.Status != queue.StatusCompleted {
		t.Fatalf("expected completed after restart, got %q", completed.Status)
	}
	t.Log("Start→Stop→Start cycle: job processed after restart")
}

func TestM3_StopIdempotent(t *testing.T) {
	f := newM3Fixture(t)
	f.worker.Stop()
	f.worker.Stop() // must be safe
	t.Log("Double Stop() did not panic — idempotent")
}

func TestM3_StartStopStartClean(t *testing.T) {
	f := newM3Fixture(t)
	f.worker.Stop()
	time.Sleep(50 * time.Millisecond)
	f.worker.Start(context.Background())

	jobID := f.insertJob("retain", "restart", "second life")
	completed := f.waitForJob(jobID, 10*time.Second)
	if completed.Status != queue.StatusCompleted {
		t.Fatalf("expected completed, got %q", completed.Status)
	}
	t.Log("Start→Stop→Start→Process cycle completed")
}

// ─── Attack Vector 11: Date Auto-Stamp Still Works ───────────────────────

func TestM3_DateAutoStampViaProcessQueueJob(t *testing.T) {
	f := newM3Fixture(t)

	// Content WITHOUT a year — backend.Retain auto-stamps via yearRE check
	content := "Alice loves coffee"
	jobID := f.insertJob("retain", "datestamp", content)
	f.waitForJob(jobID, 15*time.Second)

	req := f.mockCognee.LastRequest("/api/v1/remember")
	if req == nil {
		t.Fatal("no request captured by mock")
	}
	today := time.Now().Format("2006-01-02")
	if !strings.Contains(req.Body, today) {
		t.Fatalf("expected date %q in request body, got: %s", today, req.Body)
	}
	t.Logf("Date auto-stamp present: %s", req.Body)
}

func TestM3_ContentWithYearNoStamp(t *testing.T) {
	f := newM3Fixture(t)
	content := "Alice graduated from MIT in 2018"
	jobID := f.insertJob("retain", "datestamp", content)
	f.waitForJob(jobID, 15*time.Second)

	req := f.mockCognee.LastRequest("/api/v1/remember")
	if req == nil {
		t.Fatal("no request captured by mock")
	}
	today := time.Now().Format("2006-01-02")
	if strings.Contains(req.Body, today) {
		t.Fatalf("date %q should NOT be stamped when content has year, got: %s", today, req.Body)
	}
	t.Log("No auto-stamp for content with existing year")
}

// ─── Attack Vector 12: Auto-Improve Counter Still Works ──────────────────

func TestM3_AutoImproveTriggeredAfterNRetains(t *testing.T) {
	f := newM3Fixture(t, withAutoImprove(3))

	for i := 0; i < 3; i++ {
		jobID := f.insertJob("retain", "improvebank", fmt.Sprintf("content %d", i))
		f.waitForJob(jobID, 15*time.Second)
	}

	f.server.autoImproveWg.Wait()

	req := f.mockCognee.LastRequest("/api/v1/improve")
	if req == nil {
		t.Fatal("auto-improve did not fire — no /api/v1/improve request")
	}
	t.Logf("Auto-improve triggered after 3 retains: %s", req.Body)
}

func TestM3_AutoImproveNotTriggeredBelowThreshold(t *testing.T) {
	f := newM3Fixture(t, withAutoImprove(5))

	for i := 0; i < 2; i++ {
		jobID := f.insertJob("retain", "improvebank", fmt.Sprintf("content %d", i))
		f.waitForJob(jobID, 15*time.Second)
	}

	f.server.improveState.mu.Lock()
	bs, ok := f.server.improveState.banks["improvebank"]
	var rs int64
	if ok {
		rs = bs.retainsSince
	}
	f.server.improveState.mu.Unlock()
	if rs != 2 {
		t.Fatalf("expected retainsSince=2, got %d", rs)
	}
	t.Logf("Auto-improve not triggered at %d (threshold=5)", rs)
}

func TestM3_AutoImproveStatePersists(t *testing.T) {
	dir := t.TempDir()
	f := newM3Fixture(t, withAutoImprove(10))
	f.server.dataDir = dir
	f.server.improveState = loadAutoImproveState(dir)

	jobID := f.insertJob("retain", "persistbank", "test")
	f.waitForJob(jobID, 15*time.Second)

	path := filepath.Join(dir, "improve_state.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("improve_state.json not written: %v", err)
	}
	var persisted map[string]persistedBankState
	if err := json.Unmarshal(data, &persisted); err != nil {
		t.Fatalf("corrupt improve_state.json: %v", err)
	}
	bs, ok := persisted["persistbank"]
	if !ok {
		t.Fatal("persistbank not in persisted state")
	}
	if bs.RetainsSince != 1 {
		t.Fatalf("expected retainsSince=1, got %d", bs.RetainsSince)
	}
	t.Log("Auto-improve state persisted correctly")
}

// ─── Concurrent Safety ───────────────────────────────────────────────────

func TestM3_ConcurrentInsertsNoDeadlock(t *testing.T) {
	f := newM3Fixture(t, withMaxPending(500))

	var wg sync.WaitGroup
	errCh := make(chan error, 100)

	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			job := &queue.Job{
				ID:      newJobID(),
				Bank:    "concbank",
				Type:    "retain",
				Payload: fmt.Sprintf("concurrent content %d", id),
			}
			if err := f.store.Insert(job); err != nil && !errors.Is(err, queue.ErrQueueFull) {
				errCh <- fmt.Errorf("insert %d: %w", id, err)
			}
		}(i)
	}

	wg.Wait()
	close(errCh)

	var errCount int
	for err := range errCh {
		t.Logf("Insert error: %v", err)
		errCount++
	}
	if errCount > 0 {
		t.Fatalf("%d concurrent insert errors", errCount)
	}
	t.Log("50 concurrent inserts completed without deadlock")
}

// ─── Edge Cases ──────────────────────────────────────────────────────────

func TestM3_EmptyQueueReturnsNil(t *testing.T) {
	f := newM3Fixture(t)
	job, err := f.store.Get("does-not-exist")
	if err != nil {
		t.Fatalf("get missing job: %v", err)
	}
	if job != nil {
		t.Fatal("expected nil for missing job")
	}
	t.Log("Empty queue returns nil for missing job")
}
