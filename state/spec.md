# Spec: Module 2 — SQLite Job Queue Package

**Module**: M2 of the SQLite Queue + Hindsight Removal project
**Architect**: Principal Architect
**Date**: 2026-07-26
**Scope**: Create a self-contained `queue/` package with SQLite-backed job queue, worker pool, and startup recovery. Pure infrastructure — no wiring to handlers (M3).
**Prerequisites**: M1 complete (Hindsight removed, Cognee-only path).
**Approach**: Approach A — KISS/YAGNI. One flat package, one SQLite table, one worker pool type. No abstractions, no plugin systems, no future-proofing.

---

## 1. Goal

Create a `queue/` package that provides:

1. A `Job` type with a strict state machine (pending → running → completed|failed → dead).
2. A `Store` backed by `modernc.org/sqlite` (pure Go, no CGO) with WAL mode, startup recovery, TTL cleanup, and backpressure.
3. A `Worker` pool that dequeues jobs via NextPending(), processes them through a caller-supplied `ProcessFunc`, and gates concurrency with a semaphore channel.

The package compiles independently (`go build ./queue/...`), has zero imports of `mcp-memory` main, and is testable without Cognee or any external service running.

---

## 2. Package Layout

```
queue/
├── job.go      # Job type, Status enum, state machine helpers
├── store.go    # Store type, SQLite CRUD, pragmas, recovery, TTL cleanup
├── worker.go   # Worker type, worker loop, semaphore gating
└── *_test.go   # Coder + Tester delivered tests
```

### 2.1 Dependency

Add to `go.mod`:

```
require modernc.org/sqlite v1.40.1
```

The `modernc.org/sqlite` package is a pure-Go SQLite implementation. It compiles with `CGO_ENABLED=0` and has zero system dependencies.

### 2.2 Compile-time guard

Add at top of `queue/job.go` or `queue/store.go`:

```go
// This package compiles without CGO.
// Verify with: CGO_ENABLED=0 go build ./queue/...
```

Not a compile assertion — a comment. The real guard is the CI check: `CGO_ENABLED=0 go build ./queue/...` must pass.

---

## 3. Module 2a: `queue/job.go` — Types, Constants, State Machine

### 3.1 Status type

```go
type Status string

const (
    StatusPending   Status = "pending"
    StatusRunning   Status = "running"
    StatusCompleted Status = "completed"
    StatusFailed    Status = "failed"
    StatusDead      Status = "dead"
)
```

**Case sensitivity**: All values are lowercase ASCII. JSON and SQLite store these exact strings. No uppercase variants.

### 3.2 Job type

```go
type Job struct {
    ID         string `json:"id"`
    Bank       string `json:"bank"`
    Type       string `json:"type"`
    Payload    string `json:"payload"`
    Status     Status `json:"status"`
    RetryCount int    `json:"retry_count"`
    MaxRetries int    `json:"max_retries"`
    Result     string `json:"result,omitempty"`
    Error      string `json:"error,omitempty"`
    CreatedAt  int64  `json:"created_at"`
    UpdatedAt  int64  `json:"updated_at"`
}
```

**Concurrency safety**: `Job` is a plain data struct. Callers provide synchronization.

### 3.3 Job.Validate() method

```go
func (j *Job) Validate() error
```

Validation rules:

| Field | Rule | Error message |
|-------|------|---------------|
| ID | non-empty after trimming whitespace | `"job ID must not be empty"` |
| Bank | non-empty after trimming whitespace | `"bank must not be empty"` |
| Type | must be `"retain"` or `"reflect"` | `"job type must be 'retain' or 'reflect'"` |
| Payload | non-empty after trimming whitespace | `"payload must not be empty"` |
| Status | must be `StatusPending` or empty (defaults to pending in Store) | Validation passes — empty status is allowed at creation time |
| MaxRetries | 0 <= maxRetries <= 10. If 0, defaults to 3 in Store.Insert | `"max_retries must be between 0 and 10"` |

**IMPORTANT**: Validate() does NOT check Status for valid enum values beyond pending/empty — that's the Store's job. Validate() focuses on required-field presence.

### 3.4 Job.CanRetry() method

```go
func (j *Job) CanRetry() bool
```

Returns `true` if `j.Status == StatusFailed && j.RetryCount < j.MaxRetries`.

### 3.5 State Machine (Documentation & Helpers)

```
  ┌─────────┐
  │ pending  │──→ NextPending() picks up, sets running
  └─────────┘
       │
       ▼
  ┌─────────┐
  │ running  │──→ server crash → startup recovery → pending
  └─────────┘
       │
  ┌────┴────┐
  ▼         ▼
┌──────────┐ ┌────────┐
│completed │ │ failed │──→ CanRetry() → pending (retry)
└──────────┘ └────────┘
                  │
             !CanRetry()
                  │
                  ▼
             ┌────────┐
             │  dead  │
             └────────┘
```

Legal transitions (enforced by Store.UpdateStatus):

| From | To | Condition |
|------|----|-----------|
| pending | running | Only via NextPending() atomic claim |
| running | completed | Worker success |
| running | failed | Worker error |
| running | pending | Startup recovery (crash) |
| failed | pending | `CanRetry()` — retry |
| failed | dead | `!CanRetry()` — exhausted |
| completed | (terminal) | No further transitions |
| dead | (terminal) | No further transitions |

### 3.6 Job.Clone() method (for testing)

```go
func (j *Job) Clone() *Job
```

Deep copy. Used by tests to snapshot job state before worker processing.

### 3.7 Default constants

```go
const (
    DefaultMaxRetries = 3
    DefaultMaxPending = 1000
    DefaultJobTTL     = 24 * time.Hour
    DefaultWorkerCount = 4
    DefaultSemSize    = 3
    DefaultTTLInterval = 5 * time.Minute
)
```

---

## 4. Module 2b: `queue/store.go` — SQLite CRUD, Pragmas, Recovery, TTL

### 4.1 Store type

```go
type Store struct {
    db         *sql.DB
    mu         sync.Mutex
    maxPending int
    jobTTL     time.Duration
}
```

**Concurrency safety**: Safe for concurrent use. `mu` serializes Insert/NextPending to prevent races on pending count and atomic claim. SQLite's own locking handles concurrent reads.

### 4.2 StoreConfig type

```go
type StoreConfig struct {
    DBPath     string        // path to SQLite file (e.g., "./data/queue.db"). Use ":memory:" for tests.
    MaxPending int           // max pending jobs before Insert rejects (0 = use DefaultMaxPending)
    JobTTL     time.Duration // completed/failed/dead job retention (0 = use DefaultJobTTL, negative = forever)
}
```

### 4.3 SQLite Schema

```sql
CREATE TABLE IF NOT EXISTS jobs (
    id          TEXT PRIMARY KEY,
    bank        TEXT NOT NULL,
    type        TEXT NOT NULL,
    payload     TEXT NOT NULL,
    status      TEXT NOT NULL DEFAULT 'pending',
    retry_count INTEGER NOT NULL DEFAULT 0,
    max_retries INTEGER NOT NULL DEFAULT 3,
    result      TEXT NOT NULL DEFAULT '',
    error       TEXT NOT NULL DEFAULT '',
    created_at  INTEGER NOT NULL,
    updated_at  INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_jobs_status_created ON jobs(status, created_at);
```

**Note**: `result` and `error` have `NOT NULL DEFAULT ''` — SQLite does not enforce NOT NULL on TEXT columns without STRICT mode, but the DEFAULT ensures zero-value semantics. The Store always writes explicit values.

### 4.4 NewStore() — Constructor

```go
func NewStore(cfg StoreConfig) (*Store, error)
```

Sequence:

1. Apply defaults: MaxPending → DefaultMaxPending if 0. JobTTL → DefaultJobTTL if 0.
2. Open SQLite database with `modernc.org/sqlite` driver.
3. Apply pragmas (in this exact order):
   - `PRAGMA journal_mode=WAL` — write-ahead logging for concurrent reads
   - `PRAGMA busy_timeout=5000` — 5-second busy wait (milliseconds)
   - `PRAGMA cache_size=-8000` — 8MB page cache (negative = KB)
   - `PRAGMA mmap_size=67108864` — 64MB memory-mapped I/O
   - `PRAGMA foreign_keys=ON` — best practice (though no foreign keys yet)
   - `PRAGMA synchronous=NORMAL` — balance safety/performance for queued writes
   - `PRAGMA temp_store=MEMORY` — temp tables in memory
4. Create schema (`CREATE TABLE IF NOT EXISTS ...` + `CREATE INDEX IF NOT EXISTS ...`).
5. Run startup recovery (see §4.8).
6. Return Store.

**Error semantics**: If any step fails, return nil + error immediately. Do not return a half-initialized Store.

### 4.5 Insert()

```go
func (s *Store) Insert(job *Job) error
```

Sequence:

1. Acquire `s.mu`.
2. If `job.Status` is empty, set to `StatusPending`.
3. If `job.MaxRetries` is 0, set to `DefaultMaxRetries`.
4. Run `job.Validate()` — return error if invalid.
5. Count pending jobs: `SELECT COUNT(*) FROM jobs WHERE status = 'pending'`.
6. If count >= maxPending, return a **sentinel error** `ErrQueueFull` (defined in job.go: `var ErrQueueFull = errors.New("queue is full: too many pending jobs")`). Callers check with `errors.Is(err, ErrQueueFull)`.
7. Set `job.CreatedAt = time.Now().Unix()`, `job.UpdatedAt = job.CreatedAt`.
8. `INSERT INTO jobs (...) VALUES (...)`. Use prepared statements for safety.
9. Release `s.mu`.
10. Return nil.

**Important**: The pending count check and INSERT are protected by the same mutex hold — no TOCTOU race.

### 4.6 NextPending()

```go
func (s *Store) NextPending() (*Job, error)
```

Atomically claims the oldest pending job and sets it to running.

Sequence:

1. Acquire `s.mu`.
2. Begin transaction.
3. `SELECT * FROM jobs WHERE status = 'pending' ORDER BY created_at ASC LIMIT 1`.
4. If no rows, return nil, nil (not an error — empty queue).
5. `UPDATE jobs SET status = 'running', retry_count = retry_count + 1, updated_at = ? WHERE id = ?`.
   **Wait** — retry_count should NOT increment on first run from pending. Retry_count increments when the worker fails and the Store retries (via UpdateStatus back to pending). The initial grab stays retry_count=0 on first run.
   
   **Correction**: `UPDATE jobs SET status = 'running', updated_at = ? WHERE id = ? AND status = 'pending'`.
6. Check rows affected. If 0, another worker claimed it — return nil, nil. (Optimistic locking guard.)
7. Commit transaction.
8. Release `s.mu`.
9. Scan the row into a `Job` struct and return it.

The `retry_count` field tracks *completed attempts*, not *attempts remaining*. First run: retry_count=0. After first failure→pending: retry_count=1. After second run: still pending→running transitions, retry_count stays at 1 until next failure.

**Correction v2 on retry_count semantics**: Let's clarify the retry_count lifecycle:

- Job created: retry_count=0
- First NextPending() claim: retry_count unchanged (0) — sets status=running  
- Worker fails: UpdateStatus → status=failed, retry_count stays 0 (counts completed attempts after failure)
  
Wait, this is getting confusing. Let me define it clearly:

**retry_count = number of times the job has been attempted (completed + 1 on failure)**. OR...

**Simpler approach**: retry_count = number of failed attempts. NextPending does NOT increment retry_count. UpdateStatus to failed increments retry_count.

Let me re-define:

| Action | retry_count change |
|--------|-------------------|
| Insert | 0 |
| NextPending → running | no change |
| Worker succeeds → completed | no change |
| Worker fails → failed | increment by 1 |
| Startup recovery: running→pending | no change |
| failed→pending (retry) | no change (already incremented at fail time) |
| failed→dead | no change |

And `CanRetry()` checks: `retry_count < max_retries`.

**Revised NextPending()**:

```
1. BEGIN IMMEDIATE
2. SELECT * FROM jobs WHERE status = 'pending' ORDER BY created_at ASC LIMIT 1
3. If no row: ROLLBACK, return nil, nil
4. UPDATE jobs SET status = 'running', updated_at = ? WHERE id = ? AND status = 'pending'
5. If rows_affected == 0: ROLLBACK, return nil, nil  (raced with another worker)
6. COMMIT
7. Scan row → Job, return
```

### 4.7 UpdateStatus()

```go
func (s *Store) UpdateStatus(id string, status Status, result string, errStr string) error
```

Sequence:

1. Acquire `s.mu`.
2. `UPDATE jobs SET status = ?, result = ?, error = ?, updated_at = ? WHERE id = ?`.
3. If status is `StatusFailed`, also `UPDATE jobs SET retry_count = retry_count + 1 WHERE id = ?`.
4. Release `s.mu`.
5. Return nil.

**Legal transitions enforced by the caller (Worker)**, not by UpdateStatus itself. The store does NOT validate state transitions — the Worker is responsible for calling UpdateStatus with valid transitions. This keeps the Store simple and testable.

### 4.8 Recover() — Startup Recovery

```go
func (s *Store) Recover() (int, error)
```

Called once during `NewStore()`. Returns count of recovered jobs.

Sequence:

1. Acquire `s.mu`.
2. `UPDATE jobs SET status = 'pending', updated_at = ? WHERE status = 'running'` — orphaned running jobs go back to pending. Count rows affected.
3. `UPDATE jobs SET status = 'pending', updated_at = ? WHERE status = 'failed' AND retry_count < max_retries` — retriable failures go back to pending. Count rows affected.
4. `UPDATE jobs SET status = 'dead', updated_at = ? WHERE status = 'failed' AND retry_count >= max_retries` — exhausted failures → dead. Count rows affected.
5. Release `s.mu`.
6. Return total rows affected.

**Rationale**: Server just started → nothing is actively running → all `running` jobs were orphaned. Jobs with retries remaining get another chance. Exhausted failures are terminal.

### 4.9 Get()

```go
func (s *Store) Get(id string) (*Job, error)
```

Simple `SELECT * FROM jobs WHERE id = ?`. Returns nil, nil if not found. No mutex — SQLite handles concurrent reads.

### 4.10 CountByStatus()

```go
func (s *Store) CountByStatus(status Status) (int, error)
```

`SELECT COUNT(*) FROM jobs WHERE status = ?`. No mutex needed.

### 4.11 StartTTLCleanup() — Background Goroutine

```go
func (s *Store) StartTTLCleanup(ctx context.Context, interval time.Duration)
```

Spawns a single goroutine that periodically deletes expired jobs.

Sequence:

1. If `s.jobTTL <= 0`, return immediately (TTL disabled).
2. Spawn goroutine:
   - **defer recover** — log and return (no crash). Use `log.Printf` (stdlib log) for logging — the queue package has no logger dependency.
   - Ticker at `interval` (default: `DefaultTTLInterval`).
   - On each tick: `DELETE FROM jobs WHERE status IN ('completed', 'failed', 'dead') AND updated_at < ?` where `? = time.Now().Unix() - jobTTL.Seconds()`.
   - Exit on `ctx.Done()`.

**CRITICAL**: `StartTTLCleanup` must NOT be called before `NewStore()` returns. It is called by the consumer (M3 server.go) after Store is ready.

### 4.12 Close()

```go
func (s *Store) Close() error
```

Closes the SQLite database. Safe to call multiple times (subsequent calls are no-ops — check `db != nil`).

### 4.13 Store sentinel errors

```go
var (
    ErrQueueFull   = errors.New("queue is full: too many pending jobs")
    ErrJobNotFound = errors.New("job not found")
)
```

`ErrJobNotFound` is returned by UpdateStatus when no row matched the ID.

---

## 5. Module 2c: `queue/worker.go` — Worker Pool, Semaphore Gating

### 5.1 Worker type

```go
type Worker struct {
    store   *Store
    sem     chan struct{}
    count   int
    process ProcessFunc
    wg      sync.WaitGroup
    cancel  context.CancelFunc
    mu      sync.Mutex   // protects cancel during Stop
}
```

**Concurrency safety**: Safe for concurrent use. Start() and Stop() are callable from different goroutines. `mu` protects `cancel` assignment during Start/Stop race.

### 5.2 ProcessFunc type

```go
type ProcessFunc func(ctx context.Context, job *Job) error
```

The function receives a context that is cancelled when the worker pool shuts down. Return nil for success (job → completed), non-nil for failure (job → failed).

### 5.3 WorkerConfig

```go
type WorkerConfig struct {
    Store    *Store       // required
    Process  ProcessFunc  // required, called for each dequeued job
    Count    int          // number of worker goroutines (0 = DefaultWorkerCount)
    SemSize  int          // max concurrent process calls across all workers (0 = DefaultSemSize)
}
```

**Validation**: `Store` and `Process` must be non-nil. If Count <= 0, use DefaultWorkerCount. If SemSize <= 0, use DefaultSemSize. SemSize may be larger than Count — this allows future scaling without changing WorkerCount.

### 5.4 NewWorker()

```go
func NewWorker(cfg WorkerConfig) *Worker
```

Pure struct construction. No goroutines spawned. No side effects.

### 5.5 Start()

```go
func (w *Worker) Start(ctx context.Context)
```

Sequence:

1. Acquire `w.mu`. If `w.cancel != nil`, release and return (already started — idempotent). Create `workerCtx, w.cancel = context.WithCancel(ctx)`. Release `w.mu`.
2. Spawn `w.count` goroutines, each running `w.workerLoop(workerCtx, i)` (i = 0..count-1 for logging).
3. Add to `w.wg` before each goroutine.

### 5.6 workerLoop()

```go
func (w *Worker) workerLoop(ctx context.Context, id int)
```

Sequence:

1. `w.wg.Add(1)` — called by Start() before spawning.
2. `defer w.wg.Done()`.
3. **Defer recover** — if panic, log via `log.Printf` and return. Do NOT attempt to restart.
4. Loop:
   - Acquire semaphore: `select { case w.sem <- struct{}{}: ; case <-ctx.Done(): return }`.
   - `defer func() { <-w.sem }()` — release on return.
   - Call `w.store.NextPending()`.
   - If nil job (empty queue): release sem, sleep 100ms, continue.
   - Create per-job context with timeout (hardcoded 900s for retain, configurable later).
   - Call `w.process(jobCtx, job)`.
   - If process returns nil: `w.store.UpdateStatus(job.ID, StatusCompleted, job.Result, "")`.
   - If process returns error:
     - Increment `job.RetryCount` (though UpdateStatus handles this at DB level — the DB is the source of truth). 
     
     **Wait** — let me re-think. The ProcessFunc doesn't modify the job. The worker determines retry logic.

     Revised logic:
     - If `job.CanRetry()`: `w.store.UpdateStatus(job.ID, StatusFailed, "", err.Error())` — this increments retry_count in DB. Then `w.store.UpdateStatus(job.ID, StatusPending, "", "")` — re-queue.
     
     **Actually**, that's two UPDATEs. Let me simplify: the Store.UpdateStatus with StatusFailed increments retry_count. Then the worker checks CanRetry() and either re-queues to pending or marks as dead.

     Revised:
     - `w.store.UpdateStatus(job.ID, StatusFailed, "", processErr.Error())` — sets status=failed, increments retry_count.
     - Re-read job: `job, _ = w.store.Get(job.ID)`.
     - If `job.CanRetry()`: `w.store.UpdateStatus(job.ID, StatusPending, "", "")`.
     - Else: `w.store.UpdateStatus(job.ID, StatusDead, "", "")`.

  **Even simpler**: Have a single UpdateStatus that handles the transition and the retry_count increment atomically. The worker just calls UpdateStatus with the result and then checks CanRetry.

  Let me simplify further. The worker's logic:

  ```
  job, err := w.store.NextPending()   // pending→running, returns job
  if job == nil { sleep 100ms; continue }
  
  processErr := w.process(ctx, job)
  
  if processErr == nil {
      w.store.UpdateStatus(job.ID, StatusCompleted, "", "")
  } else {
      // Mark as failed (increments retry_count in DB)
      w.store.CompleteAttempt(job.ID, processErr.Error())
      // Re-read to get updated retry_count
      job, _ = w.store.Get(job.ID)
      if job.CanRetry() {
          w.store.UpdateStatus(job.ID, StatusPending, "", "")
      } else {
          w.store.UpdateStatus(job.ID, StatusDead, "", "")
      }
  }
  ```

  Hmm, this needs a separate `CompleteAttempt` method. Let me just make UpdateStatus smart enough:

  Actually, let me KISS. The worker does:

  ```
  if processErr == nil {
      w.store.UpdateStatus(job.ID, StatusCompleted, "", "")
  } else {
      w.store.UpdateStatus(job.ID, StatusFailed, "", processErr.Error())
      // Re-read to get updated retry_count from DB
      job, _ = w.store.Get(job.ID)
      if job.CanRetry() {
          w.store.UpdateStatus(job.ID, StatusPending, "", "")
      } else {
          w.store.UpdateStatus(job.ID, StatusDead, "", "")
      }
  }
  ```

  UpdateStatus with StatusFailed increments retry_count. That's its behavior. Then worker reads back and decides retry or dead.

5. Loop back to step 4 until `ctx.Done()`.

### 5.7 Stop()

```go
func (w *Worker) Stop()
```

Sequence:

1. Acquire `w.mu`. If `w.cancel == nil`, release and return (never started — idempotent). Call `w.cancel()`. Release `w.mu`.
2. `w.wg.Wait()` — block until all workers exit.
3. Close the semaphore channel. Actually no — sem is a buffered channel used as semaphore; don't close it, just let GC collect it.

**Wait on semaphore**: During Stop, worker goroutines may be blocked on `w.sem <- struct{}{}`. The `ctx.Done()` case in the select handles this — the worker exits instead of acquiring the semaphore. Workers that already hold the semaphore finish their current job (respecting context cancellation in the process call). 

The `wg.Wait()` ensures all workers have exited before Stop returns.

---

## 6. Goroutine Inventory

Every goroutine spawned by the queue package, with creation point and panic recovery status:

| ID | Goroutine | Spawned By | Panic Recovery | Exit Signal |
|----|-----------|-----------|---------------|-------------|
| QG1-QGN | workerLoop x N | Worker.Start() | YES (defer recover at top of workerLoop) | ctx.Done() from Stop() |
| QGCleanup | TTL cleanup | Store.StartTTLCleanup() | YES (defer recover at top of goroutine) | ctx.Done() from M3 caller |

**Total: N+1 goroutines**, where N = WorkerConfig.Count (default 4). All have panic recovery.

---

## 7. Lock Ordering

The queue package has exactly two locks:

| Lock | Type | Guards |
|------|------|--------|
| `Store.mu` | `sync.Mutex` | Serializes Insert/NextPending/UpdateStatus/Recover |
| `Worker.mu` | `sync.Mutex` | Protects `cancel` during Start/Stop race |

**Lock ordering**: `Worker.mu` → `Store.mu` (Worker.Stop acquires Worker.mu, workerLoop acquires Store.mu indirectly via NextPending/UpdateStatus). However, Worker.mu is never held while waiting on Store.mu — they are acquired in separate call chains:

- Start() acquires Worker.mu, releases it, then spawns goroutines.
- Stop() acquires Worker.mu, calls cancel(), releases Worker.mu, then wg.Wait().
- workerLoop never acquires Worker.mu — it only calls Store methods which acquire Store.mu.

**No ABBA deadlock possible within this package.** The two mutexes are never held simultaneously by any goroutine.

--- 

## 8. Configuration Surface (Environment Variables — M3 wiring)

These env vars will be read by M3's config.go and passed to the queue package as StoreConfig/WorkerConfig. Listed here for completeness:

| Env Var | Default | Dest |
|---------|---------|------|
| `QUEUE_DB_PATH` | `./data/queue.db` | StoreConfig.DBPath |
| `QUEUE_MAX_PENDING` | `1000` | StoreConfig.MaxPending |
| `QUEUE_JOB_TTL` | `24h` | StoreConfig.JobTTL |
| `QUEUE_TTL_INTERVAL` | `5m` | StartTTLCleanup interval |
| `QUEUE_WORKER_COUNT` | `4` | WorkerConfig.Count |
| `QUEUE_SEM_SIZE` | `3` | WorkerConfig.SemSize |

**None of these are read by the queue package directly.** The queue package takes explicit config structs. M3's config.go reads env vars and passes values.

---

## 9. Acceptance Criteria (M2 Only)

All ACs are testable independently of handlers, Cognee, or any external service.

| AC# | Description | Verification Method |
|-----|-------------|-------------------|
| AC-M2.1 | `queue/` package compiles with `CGO_ENABLED=0 go build ./queue/...` | Exit code 0 |
| AC-M2.2 | `queue/` package has zero imports of `mcp-memory` main or backend | `go list -deps ./queue/...` excludes non-stdlib + modernc.org/sqlite |
| AC-M2.3 | `Store` opens an in-memory SQLite DB (`:memory:`) without error | `NewStore(StoreConfig{DBPath: ":memory:"})` returns non-nil Store, nil error |
| AC-M2.4 | `Store.Insert()` inserts a job, then `Store.Get()` retrieves it with all fields matching | Insert job with known values → Get → deep-equal check |
| AC-M2.5 | `Store.Insert()` returns `ErrQueueFull` when pending count >= MaxPending | Insert MaxPending+1 jobs, last one returns ErrQueueFull |
| AC-M2.6 | `Store.NextPending()` returns oldest pending job and transitions it to running (optimistic locking) | Insert 3 jobs, NextPending 3 times → verify FIFO order and status=running |
| AC-M2.7 | `Store.NextPending()` returns nil, nil when queue is empty | Call on empty store |
| AC-M2.8 | `Store.NextPending()` is safe under concurrent callers (no duplicate claims) | 10 goroutines x 10 NextPending calls with 5 jobs → exactly 5 claims, 5 nil returns |
| AC-M2.9 | `Store.UpdateStatus()` transitions job to completed and sets result field | UpdateStatus(id, StatusCompleted, "result", "") → Get confirms |
| AC-M2.10 | `Store.UpdateStatus()` with StatusFailed increments retry_count by 1 | Insert job, NextPending, UpdateStatus failed → Get shows retry_count=1 |
| AC-M2.11 | `Store.Recover()` resets running→pending (orphaned jobs) | Manually INSERT a running job, call Recover → status is pending |
| AC-M2.12 | `Store.Recover()` resets failed→pending when retry_count < max_retries | INSERT failed job with retry_count=1, max_retries=3 → Recover → pending |
| AC-M2.13 | `Store.Recover()` sets failed→dead when retry_count >= max_retries | INSERT failed job with retry_count=3, max_retries=3 → Recover → dead |
| AC-M2.14 | `Store.StartTTLCleanup()` deletes completed jobs older than TTL | Insert completed job with updated_at = now-TTL-1s, run TTL → job gone |
| AC-M2.15 | `Store.StartTTLCleanup()` does NOT delete completed jobs newer than TTL | Insert completed job with updated_at = now, run TTL → job still present |
| AC-M2.16 | `Store.StartTTLCleanup()` goroutine exits when ctx is cancelled | Start TTL, cancel ctx, verify goroutine exits within 1s |
| AC-M2.17 | `Store.StartTTLCleanup()` is a no-op when JobTTL <= 0 | Create store with JobTTL=-1, call StartTTLCleanup → no goroutine spawned |
| AC-M2.18 | `Worker.Start()` spawns exactly `Count` goroutines | Count=4 → 4 workerLoop goroutines running |
| AC-M2.19 | `Worker` picks up jobs and calls ProcessFunc with correct job data | Insert job with known payload, ProcessFunc records payload → matches |
| AC-M2.20 | `Worker` transitions job to completed on ProcessFunc success | ProcessFunc returns nil → job status=completed |
| AC-M2.21 | `Worker` transitions job to pending (retry) on ProcessFunc failure when CanRetry() | ProcessFunc returns error, retry_count=0, max_retries=3 → job status=pending after retry |
| AC-M2.22 | `Worker` transitions job to dead when retries exhausted | ProcessFunc returns error 3 times consecutively → job status=dead |
| AC-M2.23 | `Worker` semaphore gates concurrent process calls | SemSize=1, 2 workers → only 1 process call at a time |
| AC-M2.24 | `Worker.Stop()` causes all workers to exit (no goroutine leak) | Start workers, Stop, verify goroutine count returns to baseline |
| AC-M2.25 | `Worker.Stop()` is idempotent (calling twice does not panic) | Stop x2, no panic |
| AC-M2.26 | `Worker.Start()` is idempotent (calling twice does not double-spawn) | Start x2, verify exactly Count goroutines |
| AC-M2.27 | Worker panic in ProcessFunc does NOT crash worker (recovery + continue) | ProcessFunc panics → worker logs and continues with next job |
| AC-M2.28 | Worker panic in workerLoop itself does NOT crash other workers | One worker panics → other workers unaffected, pool still functional |
| AC-M2.29 | `Job.Validate()` rejects empty ID | Validate() on Job{ID: ""} → error containing "ID" |
| AC-M2.30 | `Job.Validate()` rejects empty Bank | Validate() on Job{Bank: ""} → error containing "bank" |
| AC-M2.31 | `Job.Validate()` rejects invalid Type | Validate() on Job{Type: "invalid"} → error containing "type" |
| AC-M2.32 | `Job.Validate()` rejects empty Content | Validate() on Job{Payload: ""} → error containing "payload" |
| AC-M2.33 | `Job.Validate()` rejects MaxRetries > 10 | Validate() on Job{MaxRetries: 11} → error containing "max_retries" |
| AC-M2.34 | `Job.CanRetry()` returns true when StatusFailed and retry_count < max_retries | status=failed, retry_count=0, max_retries=3 → true |
| AC-M2.35 | `Job.CanRetry()` returns false when retry_count >= max_retries | status=failed, retry_count=3, max_retries=3 → false |
| AC-M2.36 | `Job.CanRetry()` returns false when status is not failed | status=pending, retry_count=0 → false |
| AC-M2.37 | SQLite WAL mode is enabled after NewStore | `PRAGMA journal_mode` returns "wal" |
| AC-M2.38 | All 6 pragmas are applied (WAL, busy_timeout, cache_size, mmap_size, foreign_keys, synchronous) | Query each pragma after NewStore → all match expected values |
| AC-M2.39 | `Store.Close()` can be called multiple times without panic | Close x3, no panic |
| AC-M2.40 | `Store.Close()` prevents further operations (db closed error, not panic) | Close then Insert → returns error (not panic) |
| AC-M2.41 | `go test -race -timeout 240s ./queue/...` passes with zero races | Exit code 0, no race output |
| AC-M2.42 | `CountByStatus(StatusPending)` returns correct count after Insert | Insert 5 jobs → CountByStatus(pending) = 5 |
| AC-M2.43 | `StoreConfig` defaults: MaxPending=0 → DefaultMaxPending, JobTTL=0 → DefaultJobTTL | Create store with zero-config → verify defaults applied |
| AC-M2.44 | NextPending uses `BEGIN IMMEDIATE` (not deferred) to prevent SQLITE_BUSY on concurrent claims | Code review of NextPending SQL |
| AC-M2.45 | TTL cleanup goroutine recovers from panic (doesn't crash the program) | Inject panic into TTL cleanup via test → goroutine exits gracefully |

---

## 10. Edge Cases & Risk Mitigation

### 10.1 SQLite Concurrency

| Risk | Likelihood | Mitigation |
|------|-----------|-----------|
| SQLITE_BUSY on concurrent NextPending | Medium | WAL mode allows concurrent reads. `BEGIN IMMEDIATE` prevents concurrent write conflicts. `busy_timeout=5000` gives 5s grace. |
| SQLITE_LOCKED on TTL cleanup during Insert | Low | WAL mode: reads (TTL DELETE) don't block writes (Insert). `busy_timeout` handles transient locks. |
| Database file not writable | Low | NewStore returns error immediately. No partial state. |

### 10.2 Worker Edge Cases

| Risk | Likelihood | Mitigation |
|------|-----------|-----------|
| Worker exits during long ProcessFunc | Low | ProcessFunc receives ctx — should respect cancellation. Worker's defer recovers panics. |
| ProcessFunc never returns (hangs forever) | Low | The ProcessFunc is caller-supplied. Worker does not add its own timeout (M3 wiring may add one). Documented in godoc comments. |
| Semaphore starvation | Low | First-come-first-served via channel select. Single channel for all workers. |

### 10.3 Recovery Edge Cases

| Risk | Likelihood | Mitigation |
|------|-----------|-----------|
| Recover called twice (NewStore called twice on same DB) | Low | Recover is idempotent — running→pending on an already-pending row is a no-op. |
| Orphaned running jobs with updated_at years ago | Low | Recover handles them same as recent orphans. TTL cleanup will eventually delete them if they complete. |
| Jobs with retry_count > max_retries (data corruption) | Very Low | Recover's WHERE clause `retry_count >= max_retries` catches them → dead. |

### 10.4 Backpressure Edge Cases

| Risk | Likelihood | Mitigation |
|------|-----------|-----------|
| MaxPending=0 → no jobs accepted | Low | DefaultMaxPending=1000 applies when config value is 0. Config value must be explicitly set to a negative value to disable. Document this. Actually, simpler: 0 → DefaultMaxPending. Negative → no limit (but document as "use at your own risk"). |
| CountByStatus slow under 100K pending jobs | Low | SQLite with index on status handles SELECT COUNT efficiently. If this becomes a bottleneck in M3, add periodic caching. |

---

## 11. Test Strategy (Guidance for Coder & Tester)

### 11.1 Coder-delivered tests (queue/*_test.go)

The coder must deliver at minimum:

1. **store_test.go**: TestInsert, TestNextPending, TestNextPendingConcurrent, TestUpdateStatus, TestRecover, TestTTLCleanup, TestErrQueueFull, TestDefaults, TestClose, TestPragmaVerification.
2. **worker_test.go**: TestWorkerStartStop, TestWorkerProcessSuccess, TestWorkerRetry, TestWorkerDeadAfterRetries, TestWorkerSemaphoreGate, TestWorkerPanicRecovery, TestWorkerIdempotentStartStop, TestWorkerEmptyQueue.
3. **job_test.go**: TestValidate, TestCanRetry, TestClone.

All tests must use `:memory:` database. No filesystem dependency.

### 11.2 Tester-created tests

The tester will write adversarial tests in separate test files. These are NOT required for M2 pass but are expected for full SDLC:

- 1000 concurrent Insert + NextPending race
- Worker pool under sustained retry load
- TTL cleanup during active processing
- Store operations after Close (panic-free error)
- NextPending optimistic locking under extreme contention (50 goroutines)

---

## 12. Handoff Notes for Coder

1. **Package is self-contained**: Do NOT import `mcp-memory` main, backend, config, handlers, logger, or metrics. Use `log.Printf` for logging, `fmt.Errorf` for errors.
2. **modernc.org/sqlite**: Import as `_ "modernc.org/sqlite"` in store.go for driver registration. Use `database/sql` standard interface with driver name `"sqlite"`.
3. **BEGIN IMMEDIATE**: Use `db.Begin()` — Go's `database/sql` doesn't directly support `BEGIN IMMEDIATE`. Instead, use `db.Exec("BEGIN IMMEDIATE")` before the SELECT+UPDATE in a raw transaction, or use `db.Exec()` for the whole operation with the mutex providing serialization. Since Store.mu already serializes NextPending calls, `BEGIN IMMEDIATE` is a safety net, not the sole protection. Use `db.Exec("BEGIN IMMEDIATE")` pattern.
4. **Prepared statements**: Use `db.Prepare()` for Insert (reused per Insert call). QueryRow for Get/NextPending.
5. **retry_count semantics**: UpdateStatus with StatusFailed `UPDATE jobs SET retry_count = retry_count + 1, ...`. This is the ONLY place retry_count increments.
6. **Semaphore**: `w.sem` is `make(chan struct{}, semSize)`. Workers acquire with `select { case w.sem <- struct{}{}: case <-ctx.Done(): return }` and release with `<-w.sem` in defer.
7. **No external config reading**: All config comes from StoreConfig/WorkerConfig structs. The M3 config.go package reads env vars.
8. **Error wrapping**: Use `fmt.Errorf("...: %w", err)` for underlying SQLite errors. Sentinel errors (ErrQueueFull, ErrJobNotFound) are equality-checkable.
9. **Zero dependencies beyond stdlib + modernc.org/sqlite**: No ORM, no migration framework, no third-party logger.
10. **Test isolation**: Each test creates its own `:memory:` Store. No shared state between tests.
