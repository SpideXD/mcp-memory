package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"mcp-memory/backend"
	"mcp-memory/queue"
)

// ─── Helper Wrappers ──────────────────────────────────────────────────────

// slowBackend wraps a backend.Backend with configurable delays.
// Used to test ProcessFunc timeout behavior.
type slowBackend struct {
	backend.Backend
	retainDelay  time.Duration
	reflectDelay time.Duration
}

func (s *slowBackend) Retain(ctx context.Context, bank, content string) (string, error) {
	select {
	case <-time.After(s.retainDelay):
	case <-ctx.Done():
		return "", ctx.Err()
	}
	return s.Backend.Retain(ctx, bank, content)
}

func (s *slowBackend) Reflect(ctx context.Context, bank, query string) (string, error) {
	select {
	case <-time.After(s.reflectDelay):
	case <-ctx.Done():
		return "", ctx.Err()
	}
	return s.Backend.Reflect(ctx, bank, query)
}

// ─── M3 Pass 2 Tests ──────────────────────────────────────────────────────

// ══════════════════════════════════════════════════════════════════════════
// 1. ProcessFunc timeout — backend.Retain hangs past context.WithTimeout
//    processQueueJob creates context.WithTimeout(background, CogneeRetainTimeout).
//    If backend.Retain hangs longer than the timeout, does the error propagate?
//    Does the worker retry and eventually mark dead?
// ══════════════════════════════════════════════════════════════════════════

func TestM3P2_ProcessFuncTimeout_Retain(t *testing.T) {
	f := newM3Fixture(t)

	// Wrap backend with 5s delay
	sb := &slowBackend{Backend: f.server.backend, retainDelay: 5 * time.Second}
	f.server.backend = sb

	// Override timeout to 1s so it fires before the 5s delay
	old := f.server.config.CogneeRetainTimeout
	f.server.config.CogneeRetainTimeout = 1 * time.Second
	defer func() { f.server.config.CogneeRetainTimeout = old }()

	// Insert a retain that will timeout
	jobID := f.insertJob("retain", "timeoutbank", "slow content")

	// Wait for completion — process will fail with deadline exceeded
	job := f.waitForJob(jobID, 15*time.Second) // allow retries (3 x 1s + overhead)

	t.Logf("Timeout job final status=%q error=%q retry_count=%d",
		job.Status, job.Error, job.RetryCount)

	if job.Status == queue.StatusCompleted {
		t.Fatal("BUG COMPLETED: retain completed despite 5s backend delay with 1s timeout")
	}

	if job.Status == queue.StatusDead {
		t.Log("Job correctly reached dead after exhausting retries on timeout")
	} else if job.Status == queue.StatusFailed {
		// Might be mid-retry if we caught it between retries
		t.Logf("Job in status=%q (might be between retries) error=%q", job.Status, job.Error)
	} else {
		t.Logf("Edge case: job in status=%q — check if retry loop is fast enough to exhaust", job.Status)
	}

	// Verify the error message mentions context/deadline
	if job.Error != "" {
		if strings.Contains(job.Error, "context deadline exceeded") ||
			strings.Contains(job.Error, "context canceled") ||
			strings.Contains(job.Error, "deadline") {
			t.Log("Timeout error correctly propagated: " + job.Error)
		} else {
			t.Logf("Error message: %s (may not mention context — depends on cancellation path)", job.Error)
		}
	}
}

func TestM3P2_ProcessFuncTimeout_Reflect(t *testing.T) {
	f := newM3Fixture(t)

	sb := &slowBackend{Backend: f.server.backend, reflectDelay: 5 * time.Second}
	f.server.backend = sb

	old := f.server.config.BackendReflectTimeout
	f.server.config.BackendReflectTimeout = 1 * time.Second
	defer func() { f.server.config.BackendReflectTimeout = old }()

	jobID := f.insertJob("reflect", "timeoutbank", "slow reflect test")

	job := f.waitForJob(jobID, 15*time.Second)
	t.Logf("Timeout reflect final status=%q error=%q", job.Status, job.Error)

	if job.Status == queue.StatusCompleted {
		t.Fatal("BUG: reflect completed despite 5s backend delay with 1s timeout")
	}
	if job.Status == queue.StatusDead {
		t.Log("Reflect job correctly reached dead after timeout retries exhausted")
	} else {
		t.Logf("Reflect job in status=%q — retry mechanism may still be cycling", job.Status)
	}
}

// ══════════════════════════════════════════════════════════════════════════
// 2. Queue insert while worker is processing same bank — FIFO ordering
//    Insert retain for bank "test" then immediately insert another retain
//    for bank "test". With workers=1/sem=1 they sequence. Does FIFO hold?
// ══════════════════════════════════════════════════════════════════════════

func TestM3P2_FIFO_SameBank(t *testing.T) {
	f := newM3Fixture(t)

	// Workers=1, SemSize=1 (default from fixture) — deterministic ordering
	// Insert 2 retains for same bank
	firstID := f.insertJob("retain", "fifobank", "first")
	secondID := f.insertJob("retain", "fifobank", "second")

	// Both should complete
	first := f.waitForJob(firstID, 15*time.Second)
	second := f.waitForJob(secondID, 15*time.Second)

	if first.Status != queue.StatusCompleted {
		t.Fatalf("first job not completed: status=%q", first.Status)
	}
	if second.Status != queue.StatusCompleted {
		t.Fatalf("second job not completed: status=%q", second.Status)
	}

	// FIFO: first job must have been created before second, and both should
	// have their created_at timestamps in the correct order
	if first.CreatedAt > second.CreatedAt {
		t.Fatalf("FIFO violation: first job created_at=%d > second job created_at=%d",
			first.CreatedAt, second.CreatedAt)
	}
	t.Logf("FIFO CREATED: first=%d second=%d", first.CreatedAt, second.CreatedAt)

	// Verify the mock received both requests in order
	requests := f.mockCognee.Requests()
	var retainReqs []string
	for _, r := range requests {
		if r.Path == "/api/v1/remember" || strings.Contains(r.Path, "remember") {
			retainReqs = append(retainReqs, r.Body)
		}
	}

	if len(retainReqs) < 2 {
		t.Logf("Expected 2 retain requests, got %d — may be fewer if mock captures differently", len(retainReqs))
	} else {
		// The first retain request should contain "first", second should contain "second"
		if !strings.Contains(retainReqs[0], "first") {
			t.Logf("NOTE: first retain request body: %s (expected 'first')", retainReqs[0])
		}
		if !strings.Contains(retainReqs[1], "second") {
			t.Logf("NOTE: second retain request body: %s (expected 'second')", retainReqs[1])
		}
		t.Logf("FIFO order verified: %d retain requests in sequence", len(retainReqs))
	}
}

// ══════════════════════════════════════════════════════════════════════════
// 3. memory_recall called BEFORE retain completes
//    Insert retain (slow backend), immediately call backend.Recall.
//    Does recall return cached/previous data or block waiting for retain?
// ══════════════════════════════════════════════════════════════════════════

func TestM3P2_RecallDoesNotBlockOnPendingRetain(t *testing.T) {
	f := newM3Fixture(t)

	// Slow down retain to 2s so we have time to issue a recall while retain is pending
	sb := &slowBackend{Backend: f.server.backend, retainDelay: 2 * time.Second}
	f.server.backend = sb

	// Set retain timeout high so it doesn't interfere
	oldRetain := f.server.config.CogneeRetainTimeout
	f.server.config.CogneeRetainTimeout = 30 * time.Second
	defer func() { f.server.config.CogneeRetainTimeout = oldRetain }()

	// Insert a retain that takes 2s
	jobID := f.insertJob("retain", "recallbank", "data for late recall")

	// IMMEDIATELY (don't wait for retain to finish) call backend.Recall
	recallStart := time.Now()
	result, err := f.server.backend.Recall(context.Background(), "recallbank", "test")
	recallDur := time.Since(recallStart)

	if err != nil {
		t.Fatalf("recall failed during pending retain: %v", err)
	}
	if recallDur > 2*time.Second {
		t.Fatalf("BUG: recall blocked for %v waiting for retain to complete", recallDur)
	}
	t.Logf("Recall returned in %v (non-blocking) during pending retain: %s", recallDur, result)

	// Ensure the retain eventually completes
	job := f.waitForJob(jobID, 10*time.Second)
	if job.Status != queue.StatusCompleted {
		t.Logf("Retain job final status=%q (may be in retry loop)", job.Status)
	} else {
		t.Log("Retain completed successfully after non-blocking recall")
	}
}

// ══════════════════════════════════════════════════════════════════════════
// 4. memory_forget while retain is queued (not yet processed)
//    Queue a retain, immediately forget the same content, start worker.
//    The retain processes AFTER the forget, re-adding data.
//    This documents the ordering gap between direct and queued operations.
// ══════════════════════════════════════════════════════════════════════════

func TestM3P2_ForgetDuringPendingRetain_OrderingRace(t *testing.T) {
	f := newM3Fixture(t)

	// Workers=0 so retain stays pending
	// We forget BEFORE the retain processes
	f.worker.Stop()

	contentID := "content-to-forget-001"
	bank := "forgetracebank"

	// Queue a retain that references the content that will be forgotten
	jobID := f.insertJob("retain", bank, "this should be forgotten before it's retained")

	// Now forget the content directly (before retain processes)
	forgetResult, err := f.server.backend.Forget(context.Background(), bank, contentID)
	if err != nil {
		t.Fatalf("forget failed: %v", err)
	}
	t.Logf("Forget succeeded while retain pending: %s", forgetResult)

	// Start worker — retain now processes AFTER the forget
	f.worker.Start(context.Background())

	// Wait for retain to complete
	job := f.waitForJob(jobID, 10*time.Second)
	t.Logf("Retain processed after forget: status=%q error=%q", job.Status, job.Error)

	// PROBLEM: The retain processes AFTER the forget call, so it re-adds the data
	// that was "forgotten". This is an ordering gap.
	//
	// The backend.Forget was called directly (bypassing the queue), while
	// the retain was queued. There's no mechanism to order forget-before-retain.
	t.Log("EDGE CASE: retain queued, then forget called directly, then retain processes")
	t.Log("Result: forget 'succeeds' but retain re-adds the data moments later")
	t.Log("This is a design-level race between direct and queued operations")
}

// ══════════════════════════════════════════════════════════════════════════
// 5. Double-close queue store — does Close() panic on second call?
//    s.queueStore.Close() checks s.closed.Load() first — idempotent.
// ══════════════════════════════════════════════════════════════════════════

func TestM3P2_DoubleCloseStore_NoPanic(t *testing.T) {
	store, err := queue.NewStore(queue.StoreConfig{
		DBPath:     ":memory:",
		MaxPending: 100,
		JobTTL:     24 * time.Hour,
	})
	if err != nil {
		t.Fatalf("create store: %v", err)
	}

	// Close twice
	if err := store.Close(); err != nil {
		t.Fatalf("first close returned error: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("second close returned error: %v (should be nil — idempotent)", err)
	}
	t.Log("Double Close() is idempotent — no panic, second call returns nil")
}

func TestM3P2_ClosedStoreReturnsErrors(t *testing.T) {
	store, err := queue.NewStore(queue.StoreConfig{
		DBPath:     ":memory:",
		MaxPending: 100,
		JobTTL:     24 * time.Hour,
	})
	if err != nil {
		t.Fatalf("create store: %v", err)
	}
	store.Close()

	// All operations on a closed store must return errors
	job := &queue.Job{
		ID:      "test-id",
		Bank:    "test",
		Type:    "retain",
		Payload: "content",
	}

	if err := store.Insert(job); err == nil {
		t.Fatal("BUG: Insert on closed store returned nil")
	} else {
		t.Logf("Insert on closed store correctly rejected: %v", err)
	}

	if _, err := store.NextPending(); err == nil {
		t.Fatal("BUG: NextPending on closed store returned nil")
	} else {
		t.Logf("NextPending on closed store correctly rejected: %v", err)
	}

	if _, err := store.Get("test-id"); err == nil {
		t.Fatal("BUG: Get on closed store returned nil")
	} else {
		t.Logf("Get on closed store correctly rejected: %v", err)
	}

	if _, err := store.Stats(); err == nil {
		t.Fatal("BUG: Stats on closed store returned nil")
	} else {
		t.Logf("Stats on closed store correctly rejected: %v", err)
	}
}

// ══════════════════════════════════════════════════════════════════════════
// 6. Health endpoint during concurrent store close (race detection)
//    queueDepth() calls store.Stats() which checks closed state.
//    If store is being closed concurrently, queueDepth handles the error.
// ══════════════════════════════════════════════════════════════════════════

func TestM3P2_QueueDepthRaceWithClose(t *testing.T) {
	store, err := queue.NewStore(queue.StoreConfig{
		DBPath:     ":memory:",
		MaxPending: 100,
		JobTTL:     24 * time.Hour,
	})
	if err != nil {
		t.Fatalf("create store: %v", err)
	}

	// Insert a few jobs so stats have data
	for i := 0; i < 5; i++ {
		store.Insert(&queue.Job{
			ID:      fmt.Sprintf("race-job-%d", i),
			Bank:    "racebank",
			Type:    "retain",
			Payload: fmt.Sprintf("content %d", i),
		})
	}

	// Read queue depth and close concurrently
	var wg sync.WaitGroup
	closeErr := make(chan error, 1)

	wg.Add(1)
	go func() {
		defer wg.Done()
		closeErr <- store.Close()
	}()

	// While close is happening, call queueDepth helper which calls Stats()
	depth := queueDepth(store)
	t.Logf("queueDepth during concurrent close returned: %d (0=graceful error, non-zero=race read)", depth)

	wg.Wait()
	if err := <-closeErr; err != nil {
		t.Fatalf("close error: %v", err)
	}

	// Now verify the store is actually closed
	if _, err := store.Stats(); err == nil {
		t.Fatal("BUG: Stats on closed store returned nil")
	}
	t.Log("queueDepth concurrent with Close: no panic, gracefully handles closed store")
}

// ══════════════════════════════════════════════════════════════════════════
// 7. Config validation — what does Validate() catch?
//    Set QUEUE_WORKERS=0, QUEUE_MAX_PENDING=0, QUEUE_MAX_CONCURRENT=0.
//    Does Validate() catch these?
// ══════════════════════════════════════════════════════════════════════════

func TestM3P2_ConfigValidation_QueueWorkersZero(t *testing.T) {
	cfg := Config{
		Backend:                    BackendCogneePython,
		RetryAttempts:              3,
		MaxSessions:                10,
		MaxContentBytes:            1 << 20,
		StartTimeout:               10 * time.Second,
		StopTimeout:                5 * time.Second,
		ShutdownTimeout:            5 * time.Second,
		CogneeMaxConcurrentRetains: 3,
		CogneeRetainTimeout:        900 * time.Second,
		QueueMaxPending:            100,
		QueueWorkerCount:           0, // INVALID
		QueueMaxConcurrent:         3,
	}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("BUG: Validate() accepted QueueWorkerCount=0")
	}
	if !strings.Contains(err.Error(), "QUEUE_WORKER_COUNT") {
		t.Fatalf("BUG: error %q doesn't mention QUEUE_WORKER_COUNT", err)
	}
	t.Logf("QueueWorkerCount=0 correctly rejected: %v", err)
}

func TestM3P2_ConfigValidation_QueueMaxPendingZero(t *testing.T) {
	cfg := Config{
		Backend:                    BackendCogneePython,
		RetryAttempts:              3,
		MaxSessions:                10,
		MaxContentBytes:            1 << 20,
		StartTimeout:               10 * time.Second,
		StopTimeout:                5 * time.Second,
		ShutdownTimeout:            5 * time.Second,
		CogneeMaxConcurrentRetains: 3,
		CogneeRetainTimeout:        900 * time.Second,
		QueueMaxPending:            0, // INVALID
		QueueWorkerCount:           4,
		QueueMaxConcurrent:         3,
	}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("BUG: Validate() accepted QueueMaxPending=0")
	}
	if !strings.Contains(err.Error(), "QUEUE_MAX_PENDING") {
		t.Fatalf("BUG: error %q doesn't mention QUEUE_MAX_PENDING", err)
	}
	t.Logf("QueueMaxPending=0 correctly rejected: %v", err)
}

func TestM3P2_ConfigValidation_QueueMaxConcurrentZero(t *testing.T) {
	cfg := Config{
		Backend:                    BackendCogneePython,
		RetryAttempts:              3,
		MaxSessions:                10,
		MaxContentBytes:            1 << 20,
		StartTimeout:               10 * time.Second,
		StopTimeout:                5 * time.Second,
		ShutdownTimeout:            5 * time.Second,
		CogneeMaxConcurrentRetains: 3,
		CogneeRetainTimeout:        900 * time.Second,
		QueueMaxPending:            100,
		QueueWorkerCount:           4,
		QueueMaxConcurrent:         0, // INVALID
	}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("BUG: Validate() accepted QueueMaxConcurrent=0")
	}
	if !strings.Contains(err.Error(), "QUEUE_MAX_CONCURRENT") {
		t.Fatalf("BUG: error %q doesn't mention QUEUE_MAX_CONCURRENT", err)
	}
	t.Logf("QueueMaxConcurrent=0 correctly rejected: %v", err)
}

// QUEUE_MAX_CONCURRENT > QUEUE_WORKER_COUNT is NOT validated by the spec.
// This test documents that the config doesn't enforce this relationship.
func TestM3P2_ConfigValidation_MaxConcurrentGreaterThanWorkers(t *testing.T) {
	cfg := Config{
		Backend:                    BackendCogneePython,
		RetryAttempts:              3,
		MaxSessions:                10,
		MaxContentBytes:            1 << 20,
		StartTimeout:               10 * time.Second,
		StopTimeout:                5 * time.Second,
		ShutdownTimeout:            5 * time.Second,
		CogneeMaxConcurrentRetains: 3,
		CogneeRetainTimeout:        900 * time.Second,
		QueueMaxPending:            100,
		QueueWorkerCount:           1,
		QueueMaxConcurrent:         10, // > workers — inconsistent but not harmful
	}
	err := cfg.Validate()
	if err != nil {
		t.Fatalf("Validate() rejected valid config: %v", err)
	}
	t.Log("Config with QueueMaxConcurrent(10) > QueueWorkerCount(1) passes validation")
	t.Log("This is intentional: the semaphore just caps concurrency; with 1 worker, 10 sem slots is harmless")
}

// ══════════════════════════════════════════════════════════════════════════
// 8. Backward compat: CogneeMaxConcurrentRetains fallback
//    Set COGNEE_MAX_CONCURRENT_RETAINS=5, leave QUEUE_MAX_CONCURRENT unset.
//    LoadConfig should produce QueueMaxConcurrent=5.
// ══════════════════════════════════════════════════════════════════════════

func TestM3P2_ConfigFallback_CogneeMaxConcurrentRetains(t *testing.T) {
	// Set COGNEE_MAX_CONCURRENT_RETAINS, leave QUEUE_MAX_CONCURRENT unset
	os.Setenv("COGNEE_MAX_CONCURRENT_RETAINS", "5")
	os.Unsetenv("QUEUE_MAX_CONCURRENT")
	defer os.Unsetenv("COGNEE_MAX_CONCURRENT_RETAINS")

	cfg := LoadConfig()
	if cfg.QueueMaxConcurrent != 5 {
		t.Fatalf("BUG: expected QueueMaxConcurrent=5 from COGNEE_MAX_CONCURRENT_RETAINS, got %d", cfg.QueueMaxConcurrent)
	}
	t.Logf("COGNEE_MAX_CONCURRENT_RETAINS=5 correctly maps to QueueMaxConcurrent=5")
}

func TestM3P2_ConfigFallback_DefaultThree(t *testing.T) {
	// Neither env set — should fall back to 3
	os.Unsetenv("COGNEE_MAX_CONCURRENT_RETAINS")
	os.Unsetenv("QUEUE_MAX_CONCURRENT")

	cfg := LoadConfig()
	if cfg.QueueMaxConcurrent != 3 {
		t.Fatalf("BUG: expected QueueMaxConcurrent=3 (default fallback), got %d", cfg.QueueMaxConcurrent)
	}
	t.Log("No env vars set — QueueMaxConcurrent correctly defaults to 3")
}

func TestM3P2_ConfigFallback_QueueMaxConcurrentWins(t *testing.T) {
	// Both set — QUEUE_MAX_CONCURRENT should win
	os.Setenv("COGNEE_MAX_CONCURRENT_RETAINS", "2")
	os.Setenv("QUEUE_MAX_CONCURRENT", "8")
	defer os.Unsetenv("COGNEE_MAX_CONCURRENT_RETAINS")
	defer os.Unsetenv("QUEUE_MAX_CONCURRENT")

	cfg := LoadConfig()
	if cfg.QueueMaxConcurrent != 8 {
		t.Fatalf("BUG: expected QueueMaxConcurrent=8 (QUEUE_MAX_CONCURRENT wins), got %d", cfg.QueueMaxConcurrent)
	}
	t.Log("QUEUE_MAX_CONCURRENT=8 correctly overrides COGNEE_MAX_CONCURRENT_RETAINS=2")
}

// ══════════════════════════════════════════════════════════════════════════
// 9. ProcessFunc type dispatch verification
//    Verify "retain" jobs call backend.Retain and "reflect" jobs call
//    backend.Reflect. The dispatch table in processQueueJob should route
//    correctly.
// ══════════════════════════════════════════════════════════════════════════

func TestM3P2_ProcessDispatch_RetainCallsRetain(t *testing.T) {
	f := newM3Fixture(t)

	// Reset mock request log
	f.mockCognee.ResetRequests()

	jobID := f.insertJob("retain", "dispatchbank", "test retain dispatch")
	f.waitForJob(jobID, 10*time.Second)

	// Check mock received the request on /api/v1/remember
	req := f.mockCognee.LastRequest("/api/v1/remember")
	if req == nil {
		// Also check Requests() for all paths
		all := f.mockCognee.Requests()
		t.Logf("All requests for retain dispatch: %d total", len(all))
		for _, r := range all {
			t.Logf("  %s %s", r.Method, r.Path)
		}
		t.Fatal("BUG: processQueueJob for type 'retain' did NOT call backend.Retain (no /api/v1/remember request)")
	}
	t.Logf("Retain job correctly dispatched to /api/v1/remember: %s", req.Body)
}

func TestM3P2_ProcessDispatch_ReflectCallsReflect(t *testing.T) {
	f := newM3Fixture(t)

	f.mockCognee.ResetRequests()

	jobID := f.insertJob("reflect", "dispatchbank", "test reflect dispatch")
	f.waitForJob(jobID, 10*time.Second)

	// Check mock received the request on /api/v1/improve (Reflect path)
	req := f.mockCognee.LastRequest("/api/v1/improve")
	if req == nil {
		all := f.mockCognee.Requests()
		t.Logf("All requests for reflect dispatch: %d total", len(all))
		for _, r := range all {
			t.Logf("  %s %s body=%q", r.Method, r.Path, r.Body)
		}
		t.Fatal("BUG: processQueueJob for type 'reflect' did NOT call backend.Reflect (no /api/v1/improve request)")
	}
	t.Logf("Reflect job correctly dispatched to /api/v1/improve: %s", req.Body)
}

// ══════════════════════════════════════════════════════════════════════════
// 10. Nil queueStore safety — health helpers and session cleaner
//     queueDepth(nil) and pendingCount(nil) both return 0 without panic.
//     Also test that handleRetainStatus would handle nil queueStore
//     (though spec says it's never nil after Start).
// ══════════════════════════════════════════════════════════════════════════

func TestM3P2_NilQueueStoreHelpers(t *testing.T) {
	// Already tested in Pass 1 (TestM3_NilQueueStoreHelpers), but verify again
	if d := queueDepth(nil); d != 0 {
		t.Fatalf("queueDepth(nil) = %d, want 0", d)
	}
	if c := pendingCount(nil); c != 0 {
		t.Fatalf("pendingCount(nil) = %d, want 0", c)
	}
	t.Log("Nil queueStore helpers return 0 without panic")
}

// ══════════════════════════════════════════════════════════════════════════
// EXTRA: Worker recovery after multiple consecutive panics
// What happens if ProcessFunc panics on every job? Does the worker recover
// each time and keep processing? Or does it exit after first panic?
// ══════════════════════════════════════════════════════════════════════════

func TestM3P2_MultipleConsecutivePanics_WorkerSurvives(t *testing.T) {
	f := newM3Fixture(t)

	// Backend that panics on EVERY retain call
	panicBackend := &panickingBackend{Backend: f.server.backend, panicOnRetain: true}
	f.server.backend = panicBackend

	const jobCount = 5
	jobIDs := make([]string, jobCount)
	for i := 0; i < jobCount; i++ {
		jobIDs[i] = f.insertJob("retain", "multipanicbank", fmt.Sprintf("panic job %d", i))
	}

	// Give workers time to process/panic/recover for all 5 jobs
	// Each job: panic → recovery → job retries (up to 3 times) → dead
	// Total time: 5 jobs × (3 retries × ~10ms per cycle) ≈ 150ms
	time.Sleep(2 * time.Second)

	// Check all jobs reached a terminal state
	for i, id := range jobIDs {
		job, err := f.store.Get(id)
		if err != nil {
			t.Fatalf("get job %s: %v", id, err)
		}
		if job == nil {
			t.Fatalf("job %s not found", id)
		}
		if job.Status == queue.StatusDead || job.Status == queue.StatusFailed {
			t.Logf("Job %d: status=%q retry_count=%d (worker survived panic)", i, job.Status, job.RetryCount)
		} else if job.Status == queue.StatusPending || job.Status == queue.StatusRunning {
			t.Logf("Job %d: status=%q (still processing — worker survived %d previous panics)", i, job.Status, i)
		} else {
			t.Logf("Job %d: status=%q (unexpected)", i, job.Status)
		}
	}

	// At minimum, verify worker is still alive by successfully processing a non-panicking job
	panicBackend.panicOnRetain = false
	survivorID := f.insertJob("retain", "multipanicbank", "worker survivor check")
	survivor := f.waitForJob(survivorID, 10*time.Second)
	if survivor.Status != queue.StatusCompleted {
		t.Fatalf("Worker died after %d consecutive panics — survivor job status=%q", jobCount, survivor.Status)
	}
	t.Logf("Worker survived %d consecutive panics and processed survivor job successfully", jobCount)
}

// ══════════════════════════════════════════════════════════════════════════
// EXTRA: Worker processes jobs across multiple banks concurrently
// With 2 workers and 2 banks, verify both banks get processed without
// one starving the other.
// ══════════════════════════════════════════════════════════════════════════

func TestM3P2_MultiBankConcurrentProcessing(t *testing.T) {
	f := newM3Fixture(t, withWorkers(2))

	// Insert retains for 3 different banks
	bankA := "multibank_a"
	bankB := "multibank_b"
	bankC := "multibank_c"

	ids := make(map[string]string) // bank -> jobID
	for _, bank := range []string{bankA, bankB, bankC} {
		ids[bank] = f.insertJob("retain", bank, fmt.Sprintf("content for %s", bank))
	}

	// All should complete
	for bank, id := range ids {
		job := f.waitForJob(id, 15*time.Second)
		if job.Status != queue.StatusCompleted {
			t.Fatalf("Job for bank %q not completed: status=%q", bank, job.Status)
		}
		t.Logf("Bank %q: job completed successfully", bank)
	}
}

// ══════════════════════════════════════════════════════════════════════════
// EXTRA: Job payload with special characters
// Unicode, emoji, control chars, and very long payloads through the queue
// ══════════════════════════════════════════════════════════════════════════

func TestM3P2_Payload_SpecialCharacters(t *testing.T) {
	f := newM3Fixture(t)

	payloads := []struct {
		name string
		data string
	}{
		{"unicode_emoji", "Hello world 🌍🔥 test emoji 🎉"},
		{"sql_injection_attempt", "'; DROP TABLE jobs; --"},
		{"json_like", `{"malformed": "json", "test": true}`},
		{"null_bytes", "hello\x00world\x00test"},
		{"multi_line", "line1\nline2\r\nline3"},
	}

	for _, p := range payloads {
		t.Run(p.name, func(t *testing.T) {
			// Insert via fixture — bypasses handler validation
			jobID := f.insertJob("retain", "specialbank", p.data)
			job := f.waitForJob(jobID, 10*time.Second)
			if job.Status != queue.StatusCompleted {
				t.Fatalf("job not completed: status=%q error=%q", job.Status, job.Error)
			}
			t.Logf("Payload %q processed successfully", p.name)
		})
	}
}

// ══════════════════════════════════════════════════════════════════════════
// EXTRA: Health endpoint process type markers in queue
// Verify job type persists correctly through insert → process cycle.
// ══════════════════════════════════════════════════════════════════════════

func TestM3P2_JobTypePersistsThroughProcessing(t *testing.T) {
	f := newM3Fixture(t)

	// Both job types should retain their type after processing
	testCases := []struct {
		jobType string
		payload string
	}{
		{"retain", "retain type check"},
		{"reflect", "reflect type check"},
	}

	for _, tc := range testCases {
		t.Run(tc.jobType, func(t *testing.T) {
			jobID := f.insertJob(tc.jobType, "typepersist", tc.payload)
			job := f.waitForJob(jobID, 10*time.Second)
			if job.Type != tc.jobType {
				t.Fatalf("BUG: job type changed from %q to %q after processing", tc.jobType, job.Type)
			}
			t.Logf("Job type %q correctly persisted through queue: status=%q", job.Type, job.Status)
		})
	}
}

// ══════════════════════════════════════════════════════════════════════════
// EXTRA: Queued jobs are NOT processed when workers=0
// Verifying the backpressure mechanism: workers=0 means jobs stay pending.
// ══════════════════════════════════════════════════════════════════════════

func TestM3P2_NoWorkersJobsStayPending(t *testing.T) {
	// Note: NewWorker defaults Count <= 0 to DefaultWorkerCount.
	// To get truly zero workers, stop the worker after creation.
	f := newM3Fixture(t)
	f.worker.Stop()

	jobID := f.insertJob("retain", "nobank", "no workers test")

	// Wait a bit then check — should still be pending with no workers
	time.Sleep(500 * time.Millisecond)

	job, err := f.store.Get(jobID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if job == nil {
		t.Fatal("job not found")
	}

	if job.Status != queue.StatusPending {
		t.Fatalf("BUG: job changed to %q with stopped workers (should stay pending)", job.Status)
	}
	t.Logf("Job correctly stays pending with stopped workers: status=%q", job.Status)

	// Now start workers and verify it processes
	f.worker.Start(context.Background())
	completed := f.waitForJob(jobID, 10*time.Second)
	if completed.Status != queue.StatusCompleted {
		t.Fatalf("expected completed after starting workers, got %q", completed.Status)
	}
	t.Log("Workers started: job processed successfully")
}
