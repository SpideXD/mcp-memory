# Spec: Module 3 — Wire Queue into Handlers

**Module**: M3 of the SQLite Queue + Hindsight Removal project
**Architect**: Principal Architect
**Date**: 2026-07-26
**Scope**: Wire `queue/` package (M2) into the MCP server handlers. Replace goroutine-per-retain/reflect with SQLite queue-backed async processing.
**Prerequisites**: M1 complete (Hindsight removed), M2 complete (queue/ package compiles and passes tests).
**Approach**: Approach A — KISS/YAGNI. One ProcessFunc closure. One Store. One Worker pool. Delete jobTracker, cogneeSemaphore, cogneeWg (for retain/reflect paths). Keep slim WaitGroup for auto-improve goroutines.

---

## 1. Goal

Replace the current goroutine-per-retain and goroutine-per-reflect patterns in `handlers.go` with SQLite queue-backed async processing using the `queue/` package. Delete the in-memory `jobTracker`, the `cogneeSemaphore`, and the `cogneeWg` (retain/reflect usage only). Wire the Worker pool lifecycle into `Server.Start()` and `Server.Stop()`. Update health endpoint and session cleaner to read real queue metrics.

---

## 2. Module Decomposition

This is a single, flat wiring module. All changes are in package `main`. No new subpackages. The `queue/` package is consumed as-is.

### 2.1 Files Modified

| File | Nature of Change | Lines Changed (est.) |
|------|-----------------|----------------------|
| `server.go` | Add queueStore/queueWorker fields; wire Start/Stop; delete cogneeSemaphore/jobTracker/cogneeWg; rewrite NewServer | ~80 |
| `handlers.go` | Replace goroutine-per-retain/reflect with queue.Insert; rewrite handleRetainStatus to use queue.Get; delete semaphore acquisition | ~100 |
| `session_cleaner.go` | Replace TODO(M3) + hardcoded 0 with queue.Stats().Pending | ~5 |
| `config.go` | Add QueueDBPath, QueueMaxPending, QueueJobTTL, QueueTTLInterval, QueueWorkerCount, QueueMaxConcurrent; map COGNEE_MAX_CONCURRENT_RETAINS as default | ~40 |
| `auto_improve.go` | Replace cogneeSemaphore idle check with queue.Stats(); replace cogneeWg with autoImproveWg | ~10 |

### 2.2 Files Deleted

| File | Reason |
|------|--------|
| `job_tracker.go` | Replaced by queue.Store. All call sites migrated to queue.Get / queue.Stats. |

### 2.3 Files Untouched (preserved)

- `backend/cognee.go`, `backend/backend.go`, `backend/circuit_breaker.go` — Cognee backend is unchanged
- `services.go` — Subprocess management unchanged
- `metrics/` package — unchanged
- SSE transport flow (`handleMCPSSE`, `handleMCPMessage`, `safeRouteMCP`) — unchanged
- `bankNamePattern`, `newSessionID()`, `newJobID()`, `initResponse()`, `checkAuth()` — unchanged
- `fireErrorWebhook()` — unchanged
- `types.go` — unchanged
- `errors.go` — unchanged
- All MCP tool names and schemas — unchanged
- `auto_improve.go` core logic — unchanged (only idle check and WaitGroup change)
- `internal/testutil/cogneemock/` — unchanged

---

## 3. Config: New Fields

### 3.1 Add to Config struct (`config.go`)

```go
// Queue
QueueDBPath        string        // QUEUE_DB_PATH, default "./data/queue.db"
QueueMaxPending    int           // QUEUE_MAX_PENDING, default 1000
QueueJobTTL        time.Duration // QUEUE_JOB_TTL, default 24h
QueueTTLInterval   time.Duration // QUEUE_TTL_INTERVAL, default 5m
QueueWorkerCount   int           // QUEUE_WORKER_COUNT, default 4
QueueMaxConcurrent int           // QUEUE_MAX_CONCURRENT, default = COGNEE_MAX_CONCURRENT_RETAINS, fallback 3
```

### 3.2 Defaults in LoadConfig()

```go
QueueDBPath:        getEnv("QUEUE_DB_PATH", "./data/queue.db"),
QueueMaxPending:    getEnvInt("QUEUE_MAX_PENDING", 1000),
QueueJobTTL:        getEnvDuration("QUEUE_JOB_TTL", 24*time.Hour),
QueueTTLInterval:   getEnvDuration("QUEUE_TTL_INTERVAL", 5*time.Minute),
QueueWorkerCount:   getEnvInt("QUEUE_WORKER_COUNT", 4),
QueueMaxConcurrent: getEnvInt("QUEUE_MAX_CONCURRENT", getEnvInt("COGNEE_MAX_CONCURRENT_RETAINS", 3)),
```

**Note**: `QueueMaxConcurrent` defaults to `COGNEE_MAX_CONCURRENT_RETAINS` for backward compatibility, with a final fallback of 3. This replaces the old semaphore size (was default 10). The new default of 3 is safer; users can increase via either env var. `CogneeMaxConcurrentRetains` field remains in Config but is no longer used — the value has been mapped to `QueueMaxConcurrent`. Kept for backward compat, marked deprecated in comments.

### 3.3 Validation in `Config.validateCognee()` or equivalent

Add to validation block:
```go
if c.QueueMaxPending < 1 {
    return fmt.Errorf("QUEUE_MAX_PENDING must be >= 1, got %d", c.QueueMaxPending)
}
if c.QueueWorkerCount < 1 {
    return fmt.Errorf("QUEUE_WORKER_COUNT must be >= 1, got %d", c.QueueWorkerCount)
}
if c.QueueMaxConcurrent < 1 {
    return fmt.Errorf("QUEUE_MAX_CONCURRENT must be >= 1, got %d", c.QueueMaxConcurrent)
}
```

### 3.4 Deprecated field (keep but don't use)

`CogneeMaxConcurrentRetains` — **preserved in Config struct** but not read by M3 code. It is mapped to `QueueMaxConcurrent` at config load time. The Server no longer references `config.CogneeMaxConcurrentRetains`; it uses `config.QueueMaxConcurrent` instead.

---

## 4. Server Struct Changes (`server.go`)

### 4.1 Fields to ADD

```go
// Queue infrastructure
queueStore  *queue.Store    // SQLite-backed job store
queueWorker *queue.Worker   // Worker pool for async retain/reflect processing
autoImproveWg sync.WaitGroup // tracks in-flight auto-improve goroutines (replaces cogneeWg for this purpose)
```

### 4.2 Fields to REMOVE

```go
cogneeSemaphore chan struct{}   // REMOVED — replaced by Worker.sem (QueueMaxConcurrent)
jobTracker      *jobTracker     // REMOVED — replaced by queue.Store
cogneeWg        sync.WaitGroup  // REMOVED — replaced by queue.Worker.wg + autoImproveWg
cogneeCtx       context.Context     // KEPT — needed by ProcessFunc for detached context creation
cogneeCancel    context.CancelFunc  // KEPT — called from Stop()
```

**Preserved fields**: `cogneeCtx` and `cogneeCancel` remain. They are used by the ProcessFunc to create detached contexts with timeouts. They are cancelled during Stop() to signal the backend to abort in-progress operations.

### 4.3 Metrics changes

All metrics gauges remain. `semaphoreGauge` now reads from Worker pool semaphore (via queue.Stats().Running) instead of `len(cogneeSemaphore)`. `cogneePending` now reads from `queue.Stats().Pending` instead of `jobTracker.stats().Pending`.

### 4.4 field alignment: no reordering required

Add new fields after `improveState` and `dataDir`. Remove old fields from their current positions. The struct stays aligned without padding warnings.

---

## 5. NewServer() Rewrite (`server.go`)

### 5.1 Sequence

After `s.improveState = loadAutoImproveState(s.dataDir)`:

**REMOVE**:
```go
go s.jobTrackerCleanup()
```

**ADD**:
```go
// Queue infrastructure — constructed during NewServer, started in Start()
s.queueStore = nil   // populated in Start() after Cognee is healthy
s.queueWorker = nil  // populated in Start()
```

**REMOVE** the block:
```go
s.cogneeSemaphore = make(chan struct{}, config.CogneeMaxConcurrentRetains)
s.jobTracker = newJobTracker(30 * time.Minute)
```

### 5.2 Compile-time assertion

At package level in `server.go` (or `handlers.go`):

```go
// Verify ProcessFunc signature matches queue.ProcessFunc at compile time.
var _ = queue.ProcessFunc((*Server).processQueueJob)  // intentional: won't compile because
// method expression has extra receiver. Instead, use closure check in Start().
```

**Correction**: Use a top-level function for the compile-time check:

```go
// processQueueJob is the ProcessFunc called by queue.Worker for each dequeued job.
// Signature must match queue.ProcessFunc exactly.
func (s *Server) processQueueJob(ctx context.Context, job *queue.Job) error {
    // implementation in §7
}

// Compile-time assertion: verify signature matches.
func init() {
    var fn queue.ProcessFunc = queue.ProcessFunc(nil)
    // Cast a compatible closure to force signature check:
    fn = func(ctx context.Context, job *queue.Job) error { return nil }
    _ = fn
}
```

**Wait** — Go doesn't allow method-to-function assignment for interface satisfaction checks because `(*Server).processQueueJob` has type `func(*Server, context.Context, *queue.Job) error`, not `func(context.Context, *queue.Job) error`.

**Realistic compile-time check**: In `Start()`, where the ProcessFunc closure is created:

```go
// Compile-time check: the closure must match queue.ProcessFunc
var processFunc queue.ProcessFunc = func(ctx context.Context, job *queue.Job) error {
    return s.processQueueJob(ctx, job)
}
_ = processFunc
```

This is a runtime no-op that the compiler verifies. If `s.processQueueJob` changes signature, this line fails to compile.

---

## 6. Server.Start() Changes (`server.go`)

### 6.1 Sequence (replaces current Start())

The current flow after `s.svc.waitAllHealthy(s.config.StartTimeout)` succeeds:

```
1. Create queue Store: queue.NewStore(StoreConfig{...})
2. Create queue Worker: queue.NewWorker(WorkerConfig{...})
3. Run queue.Recover() to reset orphaned jobs from previous crash
4. Start worker pool: queueWorker.Start(ctx)
5. Start TTL cleanup: queueStore.StartTTLCleanup(ctx, interval)
```

### 6.2 Full pseudocode

```go
func (s *Server) Start() error {
    // ... existing startup through s.svc.start() and s.svc.waitAllHealthy() ...

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
    })
    if err != nil {
        s.queueStore.Close()
        return fmt.Errorf("queue worker: %w", err)
    }
    s.queueWorker = worker

    // Recover orphaned jobs (crashed before M3 — unlikely, but safe)
    recovered, _ := s.queueStore.Recover()
    if recovered > 0 {
        s.log.Info("queue: recovered orphaned jobs", "count", recovered)
    }

    // Start worker pool
    s.queueWorker.Start(context.Background())

    // Start TTL cleanup
    s.queueStore.StartTTLCleanup(context.Background(), s.config.QueueTTLInterval)

    s.mu.Lock()
    s.state = StateRunning
    s.mu.Unlock()
    return nil
}
```

### 6.3 Error handling

If any queue step fails:
- If store created but worker fails: close store before returning error
- Do NOT leave `s.state = StateRunning` — return error so caller (handleStart) reports failure
- Existing sessionCleaner goroutine from earlier in Start() still runs — safe, it handles nil queueStore

---

## 7. ProcessFunc Design: `processQueueJob()`

### 7.1 Location

New method on `*Server` in `server.go` (or `handlers.go` — coder's choice).

### 7.2 Signature

```go
func (s *Server) processQueueJob(ctx context.Context, job *queue.Job) error
```

Matches `queue.ProcessFunc = func(ctx context.Context, job *queue.Job) error`.

### 7.3 Pseudocode

```go
func (s *Server) processQueueJob(ctx context.Context, job *queue.Job) error {
    s.log.Info("queue: processing job", "job_id", job.ID, "type", job.Type, "bank", job.Bank)
    startTime := time.Now()

    switch job.Type {
    case "retain":
        // Create detached context with CogneeRetainTimeout.
        // Detached from ctx so shutdown doesn't abort long-running retain.
        // ctx is only used by the worker loop for pre-acquisition cancellation.
        detachedCtx, cancel := context.WithTimeout(context.Background(), s.config.CogneeRetainTimeout)
        defer cancel()

        result, err := s.backend.Retain(detachedCtx, job.Bank, job.Payload)
        duration := time.Since(startTime)
        s.metrics.retainDur.Record(duration)

        if err != nil {
            s.log.Error("queue: retain failed", "job_id", job.ID, "bank", job.Bank, "duration", duration, logger.Error(err))
            s.metrics.errorCalls.Inc()
            s.metrics.retainErrors.Inc()
            s.fireErrorWebhook(job.Bank, job.ID, err.Error(), "retain")
            return err
        }

        // Store result in job for UpdateStatus to persist
        job.Result = result
        s.log.Info("queue: retain completed", "job_id", job.ID, "bank", job.Bank, "duration", duration)

        // Trigger auto-improve after successful retain
        s.maybeAutoImprove(job.Bank)

        return nil

    case "reflect":
        detachedCtx, cancel := context.WithTimeout(context.Background(), s.config.BackendReflectTimeout)
        defer cancel()

        _, err := s.backend.Reflect(detachedCtx, job.Bank, job.Payload)
        duration := time.Since(startTime)
        s.metrics.reflectDur.Record(duration)

        if err != nil {
            s.log.Error("queue: reflect failed", "job_id", job.ID, "bank", job.Bank, "duration", duration, logger.Error(err))
            s.metrics.errorCalls.Inc()
            return err
        }

        s.log.Info("queue: reflect completed", "job_id", job.ID, "bank", job.Bank, "duration", duration)
        return nil

    default:
        return fmt.Errorf("unknown job type: %s", job.Type)
    }
}
```

### 7.4 Key design decisions

| Decision | Rationale |
|----------|-----------|
| Detached context from `context.Background()` | `ctx` from worker is cancelled during Stop(). Long-running retains (up to 900s) must not be aborted by shutdown mid-operation. The worker's `ctx.Done()` is only checked in the worker loop's semaphore-acquire select, not passed to ProcessFunc. |
| `job.Result` set before return nil | Worker calls `UpdateStatus(job.ID, StatusCompleted, job.Result, "")`. The ProcessFunc must populate `job.Result` so the result is persisted. |
| `s.maybeAutoImprove(job.Bank)` after retain | Preserves existing auto-improve behavior — triggered on successful retain only. |
| No panic recovery in ProcessFunc | Worker.workerLoop already has `defer recover()` wrapping the process call (AC-M2.27). Double-wrapping would hide bugs. |
| `fireErrorWebhook` on retain failure | Preserves existing webhook behavior. |

### 7.5 Context Cancellation During Shutdown

When `Stop()` is called:
1. `s.cogneeCancel()` is called — this cancels `s.cogneeCtx`, which is used by... nothing in M3 since ProcessFunc uses `context.Background()`.
2. `s.queueWorker.Stop()` is called — this cancels the worker's internal context, causing worker loops to exit after finishing current jobs.

**Issue**: The detached context from `context.Background()` means the Cognee API call continues even after server shutdown begins. This is intentional — cutting off a Cognee LLM call mid-flight would waste work and potentially leave Cognee in an inconsistent state. The Worker.Stop() waits for in-flight jobs to complete (via semaphore drain + wg.Wait). The 900s timeout ensures they don't hang forever.

**If Cognee API supports cancellation**: In a future M4, we could pass `s.cogneeCtx` instead of `context.Background()` and let Cognee abort. Not in M3 scope.

---

## 8. Handlers Rewrite (`handlers.go`)

### 8.1 `memory_retain` handler (replaces lines ~256-327)

**Before** (current pattern):
```go
// Acquire semaphore, store jobTracker, spawn goroutine with defer sem release
// Goroutine: call backend.Retain(), update jobTracker, maybeAutoImprove
// Return {"status":"queued","bank":"...","job_id":"..."}
```

**After**:
```go
case "memory_retain":
    // ... validation unchanged (content required, MaxContentBytes check) ...
    s.metrics.retainCalls.Inc()
    s.metrics.retainTotal.Inc()

    jobID := newJobID()
    job := &queue.Job{
        ID:         jobID,
        Bank:       bank,
        Type:       "retain",
        Payload:    a.Content,
        MaxRetries: 0, // use default (3)
    }

    if err := s.queueStore.Insert(job); err != nil {
        if errors.Is(err, queue.ErrQueueFull) {
            s.mcpToolResult(sid, id, `{"status":"rejected","reason":"queue_full"}`)
            logReq("", err)
            return
        }
        s.mcpError(sid, id, -32000, "failed to queue job")
        s.metrics.errorCalls.Inc()
        logReq("", err)
        return
    }

    s.metrics.cogneePending.Set(int64(pendingCount(s.queueStore)))
    s.log.Info("retain_queued", "bank", bank, "job_id", jobID)
    s.mcpToolResult(sid, id, fmt.Sprintf(`{"status":"queued","bank":"%s","job_id":"%s"}`, bank, jobID))
    logReq("ok", nil)
```

**Key changes**:
- No semaphore acquisition in handler. Backpressure is via `ErrQueueFull`.
- No goroutine spawn. Worker pool handles async processing.
- Response is immediate: `{"status":"queued","bank":"...","job_id":"..."}`.
- `cogneePending` gauge updated from `CountByStatus(pending)`.

### 8.2 `memory_reflect` handler (replaces lines ~329-376)

**Before**: spawn goroutine, no job_id, no tracking.

**After**:
```go
case "memory_reflect":
    // ... validation unchanged ...
    s.metrics.reflectCalls.Inc()
    s.metrics.reflectTotal.Inc()

    jobID := newJobID()
    job := &queue.Job{
        ID:         jobID,
        Bank:       bank,
        Type:       "reflect",
        Payload:    a.Query, // empty query is valid for reflect
        MaxRetries: 0,
    }

    if err := s.queueStore.Insert(job); err != nil {
        if errors.Is(err, queue.ErrQueueFull) {
            s.mcpToolResult(sid, id, `{"status":"rejected","reason":"queue_full"}`)
            logReq("", err)
            return
        }
        s.mcpError(sid, id, -32000, "failed to queue job")
        s.metrics.errorCalls.Inc()
        logReq("", err)
        return
    }

    s.log.Info("reflect_queued", "bank", bank, "job_id", jobID)
    s.mcpToolResult(sid, id, fmt.Sprintf(`{"status":"queued","bank":"%s","job_id":"%s"}`, bank, jobID))
    logReq("ok", nil)
```

**Key change**: reflect NOW has a job_id and is tracked via queue.Store. The old `{"status":"queued","bank":"..."}` response (without job_id) is replaced with `job_id` included.

### 8.3 `handleRetainStatus` (replaces lines ~459-481)

**Before**: reads from in-memory `jobTracker`.

**After**:
```go
func (s *Server) handleRetainStatus(sid string, id interface{}, args json.RawMessage, logReq func(string, error)) {
    var a struct{ JobID string `json:"job_id"` }
    if err := json.Unmarshal(args, &a); err != nil || a.JobID == "" {
        s.mcpError(sid, id, -32602, "job_id is required")
        logReq("", fmt.Errorf("missing job_id"))
        return
    }

    job, err := s.queueStore.Get(a.JobID)
    if err != nil {
        s.mcpError(sid, id, -32000, "failed to query job status")
        logReq("", err)
        return
    }
    if job == nil {
        s.mcpToolResult(sid, id, `{"status":"not_found"}`)
        logReq("not_found", nil)
        return
    }

    // Map queue.Job to JSON response compatible with existing API
    response := map[string]interface{}{
        "job_id":     job.ID,
        "bank":       job.Bank,
        "status":     string(job.Status), // "pending","running","completed","failed","dead"
        "created_at": job.CreatedAt,
        "updated_at": job.UpdatedAt,
    }
    if job.Result != "" {
        response["result"] = job.Result
    }
    if job.Error != "" {
        response["error"] = job.Error
    }
    if job.Status == queue.StatusFailed || job.Status == queue.StatusDead {
        response["retry_count"] = job.RetryCount
        response["max_retries"] = job.MaxRetries
    }

    data, _ := json.Marshal(response)
    s.mcpToolResult(sid, id, string(data))
    logReq("ok", nil)
}
```

**Key changes**:
- No nil check on `jobTracker` — queueStore always exists (constructed unconditionally)
- Status values are queue package values: "pending", "running", "completed", "failed", "dead"
- Adds `retry_count` and `max_retries` fields for failed/dead jobs
- Removes `status":"not_found"` check on tracker nil — always available

### 8.4 Health endpoint (`handleHealth`)

Replace:
```go
"queue_depth": 0,
```

With:
```go
"queue_depth": queueDepth(s.queueStore),
```

Where `queueDepth()` is a helper that returns stats.Pending if store is non-nil, 0 otherwise:

```go
func queueDepth(store *queue.Store) int {
    if store == nil {
        return 0
    }
    stats, err := store.Stats()
    if err != nil {
        return 0
    }
    return stats.Pending
}
```

Also update the `"down"` list logic if needed (no change expected — `allHealthy` only checks llama+cognee).

Remove any remaining "hindsight" and "reranker" fields from health response if not already removed in M1. Verify with scout report §6 risk 7: `allHealthy()` already returns 2 values (llama, cognee) post-M1.

### 8.5 `pendingCount()` helper

Small helper used by retain and reflect handlers to update `cogneePending` gauge:

```go
func pendingCount(store *queue.Store) int64 {
    if store == nil {
        return 0
    }
    count, err := store.CountByStatus(queue.StatusPending)
    if err != nil {
        return 0
    }
    return int64(count)
}
```

---

## 9. Server.Stop() Changes (`server.go`)

### 9.1 Sequence (replaces current Stop())

**Before** (current):
```
1. Cancel stopMonitor
2. Close shutdown channel (sessionCleaner)
3. Cancel cogneeCtx → cogneeWg.Wait()
4. Close sessions
5. svc.stop()
6. Set state = StateStopped
```

**After**:
```
1. Cancel stopMonitor
2. Close shutdown channel (sessionCleaner)
3. Stop queue worker pool: queueWorker.Stop() — drains in-flight jobs
4. Wait for auto-improve goroutines: autoImproveWg.Wait()
5. Cancel cogneeCtx (signals any remaining detached contexts)
6. Close queue store: queueStore.Close()
7. Close sessions
8. svc.stop()
9. clearPids()
10. Set state = StateStopped
```

### 9.2 Full pseudocode

```go
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

    // Signal session cleaner goroutine to exit
    s.shutdownOnce.Do(func() { close(s.shutdown) })

    // ★ M3: Stop queue workers and drain in-flight jobs
    if s.queueWorker != nil {
        s.log.Info("stopping queue workers...")
        s.queueWorker.Stop()
        s.log.Info("queue workers stopped")
    }

    // ★ M3: Wait for auto-improve goroutines (may overlap with queue worker finish)
    s.autoImproveWg.Wait()

    // Cancel Cognee context (belt-and-suspenders for any remaining detached work)
    if s.cogneeCancel != nil {
        s.cogneeCancel()
    }

    // ★ M3: Close queue store
    if s.queueStore != nil {
        if err := s.queueStore.Close(); err != nil {
            s.log.Error("queue store close error", logger.Error(err))
        }
    }

    // Close all sessions
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
```

### 9.3 Drain semantics

`queueWorker.Stop()`:
1. Cancels worker loop context → workers stop dequeuing new jobs
2. `wg.Wait()` blocks until all worker goroutines exit
3. Workers holding the semaphore finish their current job before exiting

Total drain time: bounded by `CogneeRetainTimeout` (900s default) for the worst case — a retain just started when Stop is called.

---

## 10. Session Cleaner Fix (`session_cleaner.go`)

Replace:
```go
// TODO(M3): read queue depth from SQLite queue store
s.metrics.queueGauge.Set(0)
```

With:
```go
// Read queue depth from SQLite queue store
s.metrics.queueGauge.Set(pendingCount(s.queueStore))
```

The `pendingCount()` helper handles nil queueStore gracefully.

---

## 11. Auto-Improve Semaphore Idle Check Fix (`auto_improve.go`)

### 11.1 Replace cogneeSemaphore idle check

**Before**:
```go
idleCheck := len(s.cogneeSemaphore) <= 1
```

**After**:
```go
// Check if queue is idle: at most 1 job currently running
// (the caller's job may still show as running during this check)
stats, err := s.queueStore.Stats()
idleCheck := err == nil && stats.Running <= 1
```

### 11.2 Replace cogneeWg with autoImproveWg

**All occurrences** of `s.cogneeWg.Add(1)` → `s.autoImproveWg.Add(1)`
**All occurrences** of `s.cogneeWg.Done()` → `s.autoImproveWg.Done()`

These are in `auto_improve.go` only (the auto-improve goroutine spawn).

### 11.3 Nil-safety

In the nil-safety block of the auto-improve goroutine, capture `s.queueStore`:
```go
queueStore := s.queueStore
```

The idle check uses `queueStore` which may be nil before Start() completes. Since auto-improve fires only after a successful retain (which requires Start() to have completed), queueStore will be non-nil at check time. The `err != nil` fallback handles edge cases.

---

## 12. Goroutine Inventory (Post-M3)

Every goroutine spawned by the application, with creation point and panic recovery status:

| ID | Goroutine | Spawned By | Panic Recovery | Exit Signal |
|----|-----------|-----------|---------------|-------------|
| G1 | HTTP server | main.go (net/http) | net/http built-in | Server.Shutdown() |
| G2 | SSE handler xN | handleMCPSSE | net/http built-in | r.Context().Done() |
| G3 | MCP message dispatcher xN | handleMCPMessage → safeRouteMCP | YES (defer recover) | Goroutine exit after response |
| G4 | Session cleaner | Server.Start() → sessionCleaner | YES (defer recover) | s.shutdown channel |
| G5 | Service monitor | Server.Start() → monitor | YES (built into monitor) | stopMonitor cancel |
| G6-G9 | Queue workers (default 4) | queue.Worker.Start() | YES (defer recover per worker) | ctx.Done() from Worker.Stop() |
| G10 | Queue TTL cleanup | queue.Store.StartTTLCleanup() | YES (defer recover) | ctx.Done() from Stop() |
| G11 | Auto-improve xN | maybeAutoImprove() → goroutine | YES (defer recover) | Goroutine exit after completion |
| G12 | Error webhook xN | fireErrorWebhook() → goroutine | YES (defer recover) | Goroutine exit after completion |
| G13 | Llama.cpp/Cognee subprocess | services.start() | exec.Cmd built-in | svc.stop() kills process |
| G14 | jobTrackerCleanup | **DELETED** | — | — |

**Total before M3**: G1-G5 + G12-G13 + jobTrackerCleanup + cogneeWg-tracked goroutines (retain, reflect, auto-improve, webhook).
**Total after M3**: G1-G13 (jobTrackerCleanup removed, retain/reflect goroutines replaced by G6-G9 workers).

**Panic recovery status**: ALL goroutines with application code have panic recovery. HTTP server goroutines have net/http built-in recovery.

---

## 13. Lock Ordering (Post-M3)

Complete lock ordering across all goroutines:

| # | Lock | Type | Guards | Held By |
|---|------|------|--------|---------|
| 1 | `s.sessionsMu` | `sync.RWMutex` | sessions map | SSE handler, MCP dispatch, session cleaner |
| 2 | `s.mu` | `sync.RWMutex` | server state | Start(), Stop(), handleHealth |
| 3 | `s.improveState.mu` | `sync.Mutex` | auto-improve bank state | maybeAutoImprove, auto-improve goroutine |
| 4 | `s.queueStore.mu` | `sync.Mutex` | SQLite write serialization | Insert (HTTP handler), NextPending/UpdateStatus (worker) |
| 5 | `s.queueWorker.mu` | `sync.Mutex` | cancel field | Worker.Start(), Worker.Stop() |

**Lock ordering rule**: If multiple locks are acquired, acquire in order 1→5. No goroutine acquires a lower-numbered lock while holding a higher-numbered lock.

**Critical paths**:

- **HTTP retain handler**: holds no locks during `queueStore.Insert()` (which acquires #4 internally). Returns response without holding any server locks.
- **Worker goroutine**: acquires #4 internally via NextPending/UpdateStatus. Never acquires #1-#3.
- **Auto-improve**: acquires #3 → calls `queueStore.Stats()` (which does NOT acquire #4 — Stats doesn't use store.mu). No cycle.
- **Session cleaner**: acquires #1 only. Reads `queueStore.Stats()` (no lock needed).
- **Stop()**: acquires #2 (briefly to check state) → calls Worker.Stop() (acquires #5 internally) → calls queueStore.Close() (acquires #4 internally). Order: 2→5→4. **This violates 1→5 ordering!** But since #2 is released before #5 and #4 are acquired, there's no hold-and-wait. The actual held-at-once sets are: {#2} then release, {#5} then release, {#4}.

**No ABBA deadlock possible**. Store.mu and Worker.mu are never held simultaneously by any goroutine (per M2 spec §7). The session cleaner and worker goroutines operate on entirely disjoint lock sets.

---

## 14. Backpressure Summary

| Layer | Mechanism | Response |
|-------|-----------|----------|
| Queue full (MaxPending reached) | `queue.ErrQueueFull` | HTTP handler returns `{"status":"rejected","reason":"queue_full"}` |
| Worker semaphore full | Worker goroutine blocks in `select { case sem <-: ... }` | Backpressure is invisible to HTTP — request is already queued |
| Cognee overload | ProcessFunc creates detached context with timeout | Worker marks job as failed, retries up to MaxRetries |
| Worker pool drained during shutdown | Worker.Stop() → cancel ctx → workers reject new semaphore acquires → finish current jobs → exit | Shutdown waits up to job timeout |

---

## 15. Error Handling Table

| Scenario | Handler Behavior | Worker Behavior |
|----------|-----------------|-----------------|
| queueStore.Insert() returns ErrQueueFull | Return `{"status":"rejected","reason":"queue_full"}` | N/A |
| queueStore.Insert() returns other error | Return MCP error -32000 | N/A |
| queueStore is nil (before Start()) | Nil check in pendingCount/queueDepth returns 0 | N/A — workers only started after store is non-nil |
| ProcessFunc returns error (retain failed) | N/A | Worker marks job failed, webhook fires, auto-retry |
| ProcessFunc returns error (reflect failed) | N/A | Worker marks job failed, auto-retry, NO webhook |
| ProcessFunc panics | N/A | Worker defer recover logs and continues |
| queueStore.UpdateStatus fails | N/A | Worker logs error, job stuck in running → recovery on restart |
| Cognee API call exceeds timeout | N/A | context.WithTimeout cancels, ProcessFunc returns error |
| Stop() called with in-flight jobs | N/A | Worker.Stop() blocks until jobs complete or timeout |
| memory_retain_status on non-existent job_id | Return `{"status":"not_found"}` | N/A |
| memory_retain_status on dead job | Return status="dead" with error and retry info | N/A |

---

## 16. Acceptance Criteria (M3)

All ACs are testable with Cognee mock backend (cogneemock) and :memory: queue store.

### 16.1 Retain Path

| AC# | Description | Verification |
|-----|-------------|-------------|
| AC-M3.1 | `memory_retain` returns `{"status":"queued","bank":"...","job_id":"..."}` immediately (no blocking) | POST memory_retain → response in <100ms, contains job_id |
| AC-M3.2 | `memory_retain` returns `{"status":"rejected","reason":"queue_full"}` when MaxPending reached | Insert MaxPending jobs, next retain returns rejection |
| AC-M3.3 | Queued retain job is processed by worker pool (CogneeBackend.Retain called) | Insert job, wait, verify Cognee mock received retain request |
| AC-M3.4 | Job transitions: pending → running → completed on retain success | Query job by ID after completion → status="completed" |
| AC-M3.5 | Job transitions: pending → running → failed → pending (retry) on retain failure with retries left | Mock returns error, verify retry_count increments and status goes back to pending |
| AC-M3.6 | Job transitions to dead after MaxRetries exhausted | Mock returns error 3+ times consecutively → status="dead" |
| AC-M3.7 | `job.Result` is persisted on retain success | Query completed job → result field contains Cognee response |
| AC-M3.8 | `job.Error` is persisted on retain failure | Query failed job → error field contains error message |
| AC-M3.9 | `fireErrorWebhook` is called on retain failure | Mock webhook URL, verify POST received with job_id and error |
| AC-M3.10 | `maybeAutoImprove` is called after successful retain | Mock auto-improve, verify backend.Reflect(bank, "") is called |
| AC-M3.11 | `semaphoreGauge` reflects queue worker concurrency | With SemSize=3 and 10 concurrent retains, gauge never exceeds 3 |

### 16.2 Reflect Path

| AC# | Description | Verification |
|-----|-------------|-------------|
| AC-M3.12 | `memory_reflect` returns `{"status":"queued","bank":"...","job_id":"..."}` with job_id | POST memory_reflect → response contains job_id (new!) |
| AC-M3.13 | `memory_reflect` returns `{"status":"rejected","reason":"queue_full"}` when MaxPending reached | Insert MaxPending jobs, next reflect returns rejection |
| AC-M3.14 | Queued reflect job is processed (CogneeBackend.Reflect called) | Insert reflect job, wait, verify Cognee mock received reflect request |
| AC-M3.15 | Reflect job status pollable via `memory_retain_status` | Insert reflect job, poll status → returns job state |

### 16.3 Status Endpoint

| AC# | Description | Verification |
|-----|-------------|-------------|
| AC-M3.16 | `memory_retain_status` returns job data from SQLite queue | Insert job, call memory_retain_status → returns job with correct fields |
| AC-M3.17 | `memory_retain_status` returns `{"status":"not_found"}` for non-existent job_id | Query with invalid job_id → not_found |
| AC-M3.18 | `memory_retain_status` returns `retry_count` and `max_retries` for failed jobs | Query failed job → response includes retry fields |
| AC-M3.19 | `memory_retain_status` returns status="dead" for exhausted retries | Query dead job → status="dead" |
| AC-M3.20 | `memory_retain_status` works without nil-check (queueStore always constructed) | Verify no `"job tracking not available"` error in code path |

### 16.4 Health Endpoint

| AC# | Description | Verification |
|-----|-------------|-------------|
| AC-M3.21 | `GET /health` returns real `queue_depth` from SQLite | Insert 5 pending jobs → health endpoint shows queue_depth=5 |
| AC-M3.22 | `GET /health` returns `queue_depth=0` before any jobs queued | Fresh start → queue_depth=0 (not hardcoded, actually reads store) |

### 16.5 Lifecycle

| AC# | Description | Verification |
|-----|-------------|-------------|
| AC-M3.23 | `Server.Start()` creates queue.Store and queue.Worker after Cognee is healthy | Check logs: "queue workers started" after Cognee health check |
| AC-M3.24 | `Server.Start()` calls `queue.Recover()` and logs count if >0 | Manually reset running jobs before start → log shows recovered count |
| AC-M3.25 | `Server.Start()` starts TTL cleanup goroutine | Verify TTL goroutine exists after start |
| AC-M3.26 | `Server.Stop()` calls `queueWorker.Stop()` and waits for drain | Start retain job, immediately call Stop → Stop blocks until job completes |
| AC-M3.27 | `Server.Stop()` calls `queueStore.Close()` | Verify SQLite file handle released after stop |
| AC-M3.28 | `Server.Stop()` waits for auto-improve goroutines via `autoImproveWg` | Trigger auto-improve, call Stop → Stop blocks until auto-improve completes |
| AC-M3.29 | `Server.Stop()` is safe when queue store/worker are nil (stop before start) | Stop without Start → no panic |
| AC-M3.30 | `Server.Start()` then `Server.Stop()` then `Server.Start()` — queue reinitializes cleanly | Start→Stop→Start cycle → second Start has fresh workers |

### 16.6 Deletion Verification

| AC# | Description | Verification |
|-----|-------------|-------------|
| AC-M3.31 | `job_tracker.go` file is deleted | File does not exist |
| AC-M3.32 | `cogneeSemaphore` field removed from Server struct | Grep for cogneeSemaphore → zero results (except auto_improve.go which gets replaced) |
| AC-M3.33 | `jobTracker` field removed from Server struct | Grep for jobTracker in server.go → zero results |
| AC-M3.34 | `cogneeWg` usage removed from retain/reflect paths | Grep handlers.go for cogneeWg → zero results |
| AC-M3.35 | `jobTrackerCleanup()` goroutine removed | Grep for jobTrackerCleanup → zero results |
| AC-M3.36 | `// TODO(M3)` comment removed from session_cleaner.go | Grep for TODO(M3) → zero results |

### 16.7 Session Cleaner

| AC# | Description | Verification |
|-----|-------------|-------------|
| AC-M3.37 | Session cleaner reads real queue depth from SQLite | Insert pending jobs while cleaner runs → `queueGauge` updates to correct value |

### 16.8 Compile-Time & Concurrency

| AC# | Description | Verification |
|-----|-------------|-------------|
| AC-M3.38 | Compile-time assertion that ProcessFunc matches queue.ProcessFunc | Change processQueueJob signature → compile error |
| AC-M3.39 | `go test -race -timeout 240s ./...` passes with zero races | Exit code 0, no race output |
| AC-M3.40 | `go build ./...` compiles without errors | Exit code 0 |

### 16.9 Config

| AC# | Description | Verification |
|-----|-------------|-------------|
| AC-M3.41 | `QUEUE_MAX_PENDING` env var sets MaxPending | Set QUEUE_MAX_PENDING=5, insert 6 jobs → 6th returns queue_full |
| AC-M3.42 | `QUEUE_WORKER_COUNT` env var controls worker count | Set QUEUE_WORKER_COUNT=2 → exactly 2 worker goroutines |
| AC-M3.43 | `QUEUE_MAX_CONCURRENT` env var controls semaphore size | Set QUEUE_MAX_CONCURRENT=1 → only 1 concurrent ProcessFunc call |
| AC-M3.44 | `COGNEE_MAX_CONCURRENT_RETAINS` still works as fallback for QUEUE_MAX_CONCURRENT | Set COGNEE_MAX_CONCURRENT_RETAINS=5, leave QUEUE_MAX_CONCURRENT unset → sem size=5 |

---

## 17. Edge Cases & Risk Mitigation

### 17.1 Nil queueStore during early shutdown

| Risk | Likelihood | Mitigation |
|------|-----------|-----------|
| Stop() called before Start() → queueStore is nil | Medium (test scenario) | All accessor helpers (`pendingCount`, `queueDepth`) check for nil store. `Stop()` checks `s.queueWorker != nil` and `s.queueStore != nil` before calling methods. |

### 17.2 Auto-improve idle check with nil queueStore

| Risk | Likelihood | Mitigation |
|------|-----------|-----------|
| `maybeAutoImprove` called before Start() completes | Very Low | Auto-improve fires only after a retain goroutine completes, which requires Start() to have finished. The `err != nil` fallback in idle check handles any edge case. |

### 17.3 memory_retain_status API compatibility

| Risk | Likelihood | Mitigation |
|------|-----------|-----------|
| Old clients expect exact JSON shape from jobTracker | Medium | New response includes all old fields (`job_id`, `bank`, `status`, `error`, `result`, `created_at`, `updated_at`) plus new fields (`retry_count`, `max_retries`). Old clients ignore unknown fields. Status values are strings — "completed" still means success. |

### 17.4 Queue DB file in working directory

| Risk | Likelihood | Mitigation |
|------|-----------|-----------|
| Default `./data/queue.db` in CWD conflicts with deployment | Low | Same pattern as `./data/improve_state.json` and `./logs/memory.log`. Consistent with existing file layout. Overridable via `QUEUE_DB_PATH`. |

### 17.5 WAL file accumulation during TTL cleanup

| Risk | Likelihood | Mitigation |
|------|-----------|-----------|
| Large number of dead jobs creates large WAL file | Medium | TTL cleanup runs every 5 minutes (configurable). SQLite WAL auto-checkpoints after 1000 pages. Queue TTL is 24h by default. |

### 17.6 ProcessFunc timeout vs Worker semaphore timeout

| Risk | Likelihood | Mitigation |
|------|-----------|-----------|
| ProcessFunc hangs 900s holding semaphore slot, blocking other workers | Low | The `context.WithTimeout` in ProcessFunc enforces a hard deadline. The semaphore is released via `defer` in `processWithSemaphore`. If ProcessFunc truly hangs past timeout (e.g., network partition), the Worker still recovers because ProcessFunc returns error on timeout. |

---

## 18. Handoff Notes for Coder

1. **Delete `job_tracker.go` entirely** — remove the file from the repository.
2. **All new code in package `main`** — no new packages. Imports: add `"mcp-memory/queue"` to server.go and handlers.go.
3. **Import `"errors"` in handlers.go** — needed for `errors.Is(err, queue.ErrQueueFull)`.
4. **`autoImproveWg` field** — add to Server struct. Replace ALL `s.cogneeWg.Add/Done` calls in `auto_improve.go` only. The cogneeWg field is removed from Server struct.
5. **Keep `cogneeCtx` and `cogneeCancel`** — they are still created in NewServer and cancelled in Stop. ProcessFunc uses `context.Background()` for the detached context, not `cogneeCtx`. cogneeCtx cancellation in Stop is belt-and-suspenders.
6. **Do NOT remove `CogneeMaxConcurrentRetains` from Config** — keep it but mark it `// Deprecated: use QueueMaxConcurrent`. The env var still works as a fallback.
7. **`queue.Stats()` does NOT acquire store.mu** — Stats uses a raw SQL query without the mutex. This is safe for approximate readings in health/session_cleaner paths.
8. **`cogneePending` gauge** — update from `pendingCount()` helper after every Insert in retain and reflect handlers.
9. **`semaphoreGauge` gauge** — update from `queue.Stats().Running` in session cleaner or a periodic goroutine. Alternatively, remove `semaphoreGauge` and replace with `memory.queue_running` gauge if appropriate. For M3: update in session cleaner alongside queue depth.
10. **`memory_retain_duration` and `memory_reflect_duration` timers** — record in ProcessFunc (not in handler) since the async processing time is what matters.
11. **Test strategy** — use `cogneemock` for Cognee backend and `:memory:` queue store. Set `QueueMaxConcurrent=1` for deterministic ordering in tests. Use `QueueWorkerCount=1` for single-worker tests.
12. **No existing test files need modification** unless they reference `jobTracker`, `cogneeSemaphore`, or `cogneeWg` directly. Check `auto_improve_test.go`, `tester_pass1_adversarial_test.go`, `deep_test.go`, and any integration tests.
