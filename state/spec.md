# M5 Spec: Cleanup + Production Readiness

**Status:** FINAL
**Module:** M5 (Final)
**Dependencies:** M3 (queue), M4 (auto-reflect) must be complete
**Estimated scope:** ~200 lines net new code, ~300 lines doc changes, ~5 lines deleted

---

## Goal

M5 is the final polish and cleanup module. No new architectural components. Eight discrete sub-tasks that prepare the codebase for production: a debug endpoint, dead-letter webhook, env template finalization, doc rewrites, structured job logging, stale comment removal, a stale file deletion, and a final vet pass.

---

## Module Decomposition

Since all M5 tasks are independent and touch disjoint code areas, they are listed as a flat checklist. The coder implements all 8 tasks in sequence, and the tester verifies them all in a single batch.

| # | Task | Files Touched |
|---|------|---------------|
| 1 | GET /debug/queue endpoint | `main.go`, `handlers.go`, `server.go` |
| 2 | Dead-letter webhook on StatusDead | `queue/worker.go` |
| 3 | .env.example finalization | `.env.example` |
| 4 | Docs update | `docs/architecture.md`, `docs/deployment.md`, `docs/development.md` |
| 5 | Structured job logging | `handlers.go`, `server.go` (`processQueueJob`) |
| 6 | Stale comment removal | `config.go`, `backend/doRequest.go`, `auto_improve.go`, `session_cleaner.go` |
| 7 | Delete .anon_id | `.anon_id` |
| 8 | Final vet pass | `go vet ./...` |

---

## Task 1: GET /debug/queue Endpoint

### Purpose

Expose live queue state as JSON for operational monitoring. No authentication required (same as /health).

### Route

```
GET /debug/queue
```

Registered in `main.go` mux alongside existing routes.

### Response JSON Schema

```json
{
  "pending": 42,
  "running": 3,
  "completed_total": 1503,
  "failed_total": 12,
  "dead_total": 5,
  "oldest_pending_age_s": 127.5,
  "workers": 4,
  "max_concurrent": 3,
  "db_size_kb": 512
}
```

### Field Definitions

| Field | Type | Source |
|-------|------|--------|
| `pending` | int | `s.queueStore.Stats().Pending` |
| `running` | int | `s.queueStore.Stats().Running` |
| `completed_total` | int | `s.queueStore.Stats().Completed` |
| `failed_total` | int | `s.queueStore.Stats().Failed` |
| `dead_total` | int | `s.queueStore.Stats().Dead` |
| `oldest_pending_age_s` | float64 | `time.Now().Unix() - stats.OldestPending`, 0 if no pending |
| `workers` | int | `s.config.QueueWorkerCount` |
| `max_concurrent` | int | `s.config.QueueMaxConcurrent` |
| `db_size_kb` | int | `os.Stat(s.config.QueueDBPath).Size() / 1024`, 0 if missing |

### Null-Safety

If `s.queueStore` is nil (server starting/shutting down), return:
```json
{"pending":0,"running":0,"completed_total":0,"failed_total":0,"dead_total":0,"oldest_pending_age_s":0,"workers":0,"max_concurrent":0,"db_size_kb":0}
```

### DB Size Computation

Use `os.Stat(s.config.QueueDBPath)` with error handling. If stat fails (file missing, permissions), set `db_size_kb` to 0. Do nothing else — no error response, no logging.

### Implementation

Add a method `handleDebugQueue` on `*Server` in `handlers.go`. The method signature:

```go
func (s *Server) handleDebugQueue(w http.ResponseWriter, r *http.Request)
```

- Method check: only GET, return 405 otherwise
- Content-Type: `application/json`
- Build response using `map[string]interface{}`
- `json.NewEncoder(w).Encode(response)`
- No auth check — same as `/health`

Register in main.go:

```go
mux.HandleFunc("/debug/queue", srv.handleDebugQueue)
```

---

## Task 2: Dead-Letter Webhook on StatusDead

### Purpose

When a job transitions to `StatusDead` in `worker.go`, fire the error webhook so operators are notified of permanently failed jobs.

### Implementation

In `queue/worker.go`, `processJob` method: after the `else` branch that does `UpdateStatus(job.ID, StatusDead, ...)`, add a call to fire the error webhook.

However, the `Worker` struct does not have access to the webhook. There are two approaches:

**Approach A (KISS — chosen):** Use a callback. Add an `OnDeadFunc` to `WorkerConfig`. When a job transitions to dead, call it. The `Server` provides a closure that calls `fireErrorWebhook`.

**Approach B (Scalable):** Add a webhook URL directly to WorkerConfig. Rejected — Worker is a generic queue package; webhook is application-specific.

### Concrete Changes

1. **Add `OnDeadFunc` to `WorkerConfig`:**

```go
// WorkerConfig — add field:
OnDead func(job *Job) // optional callback when job reaches StatusDead
```

Nil check before calling.

2. **Call OnDeadFunc in `processJob`:**

In `processJob`, after the `else` branch that sets `StatusDead`:

```go
// existing: w.store.UpdateStatus(job.ID, StatusDead, "", processErr.Error())

// NEW: notify via callback
if w.onDead != nil {
    w.onDead(job)
}
```

Store `onDead` as a field in `Worker` struct.

3. **Wire from Server:**

In `server.go` `Start()`, when creating Worker:

```go
worker, err := queue.NewWorker(queue.WorkerConfig{
    Store:   s.queueStore,
    Process: processFunc,
    Count:   s.config.QueueWorkerCount,
    SemSize: s.config.QueueMaxConcurrent,
    OnDead: func(job *queue.Job) {
        s.fireErrorWebhook(job.Bank, job.ID, job.Error, job.Type)
    },
})
```

### fireErrorWebhook Signature

Existing `fireErrorWebhook(bank, jobID, errMsg, operation string)` already accepts the needed parameters. The `operation` parameter gets `job.Type` ("retain" or "reflect").

---

## Task 3: .env.example Finalization

### Purpose

`.env.example` must reflect the current runtime config exactly. Add all M2-M4 config vars that are missing. Remove all Hindsight/reranker references.

### Variables to ADD

Under a new `# ============================================================================` section "Queue":

```
# ============================================================================
# Queue (SQLite-backed job queue for retain/reflect)
# ============================================================================
# Path to SQLite database file. Uses WAL mode, safe for concurrent access.
QUEUE_DB_PATH=./data/queue.db
# Maximum pending jobs before insertion is rejected.
QUEUE_MAX_PENDING=1000
# Number of worker goroutines polling for jobs.
QUEUE_WORKERS=4
# Maximum concurrent in-flight backend calls (semaphore).
QUEUE_MAX_CONCURRENT=3
# How long completed/failed/dead jobs are retained before TTL cleanup.
QUEUE_JOB_TTL=24h
# How often TTL cleanup runs.
QUEUE_TTL_INTERVAL=5m
```

Under a new `# ============================================================================` section "Auto-Improve":

```
# ============================================================================
# Auto-Improve (periodic graph optimization)
# ============================================================================
# Number of retains before triggering auto-improve. 0 = disabled.
AUTO_IMPROVE_AFTER_N=0
# Minimum time between auto-improve triggers.
AUTO_IMPROVE_COOLDOWN=120s
```

Under a new `# ============================================================================` section "Auto-Reflect":

```
# ============================================================================
# Auto-Reflect (periodic memory synthesis)
# ============================================================================
# Number of retains before triggering auto-reflect. 0 = disabled.
AUTO_REFLECT_AFTER_N=10
# Maximum time since last reflect before triggering. 0 = disabled.
AUTO_REFLECT_TIMEOUT=6h
```

Under the existing "Cognee" section (if one exists) or in a logical location, add:

```
# Error webhook URL — POSTed when jobs permanently fail (dead letter).
ERROR_WEBHOOK_URL=
```

### Variables to REMOVE

Delete any lines referencing:
- `HINDSIGHT_*` (all: PORT, PATH, LLM_PROVIDER, LLM_MODEL, EMBEDDINGS_PROVIDER, EMBEDDINGS_MODEL, RERANKER_PROVIDER, RERANKER_MODEL, RETAIN_TIMEOUT, RECALL_TIMEOUT, REFLECT_TIMEOUT, CIRCUIT_BREAKER_THRESHOLD, CIRCUIT_BREAKER_COOLDOWN)
- `RERANK_MODEL`
- `CLOUD_RERANKER_*` (API_KEY, URL, MODEL)
- `BACKEND`
- `MEMORY_RETAIN_WORKERS`, `MEMORY_REFLECT_WORKERS`, `MEMORY_JOB_BUFFER`, `MEMORY_QUEUE_PUSH_TIMEOUT`, `MEMORY_QUEUE_RESPONSE_TIMEOUT`
- `LLAMA_RERANKER_PORT`
- `COGNEE_MAX_CONCURRENT_RETAINS`

### Variables to KEEP

- All `MCP_*`, `LLAMA_*`, `CLOUD_EMBEDDING_*`, `COGNEE_*`, `OPENROUTER_*`, `HTTP_*`, `SERVICE_*`, `HEALTH_*`, `ALERT_*`

### Result

The final `.env.example` must contain exactly the env vars referenced by `config.go` `LoadConfig()` — no more, no less.

---

## Task 4: Docs Update

### 4a: `docs/architecture.md`

**Delete sections:**
- The ASCII diagram showing Hindsight + reranker columns — replace with simplified diagram showing only llama.cpp embedder + Cognee
- "Circuit Breaker (`hindsight.go`)" section — remove entirely
- "Exponential Backoff (`hindsight.go`)" section — remove entirely (backoff now lives in `backend/doRequest.go`)
- "Cloud Embedding/Reranker Support" — remove reranker half, keep only cloud embedding
- "Memory Budget" table — remove reranker + Hindsight rows, update to reflect current ~650MB total

**Replace sections:**
- "Worker Pools" — replace with "SQLite Queue (`queue/`)" describing queue architecture:
  - Store: SQLite with WAL, schema, startup recovery
  - Worker: pool of N goroutines, semaphore-bounded, panic-safe
  - State machine: pending -> running -> completed/failed/dead
  - Retry: failed jobs with retries left go back to pending
  - TTL: periodic cleanup of completed/failed/dead jobs

**Add section:**
- New "Debug Endpoints" section documenting `GET /debug/queue`

### 4b: `docs/deployment.md`

**Delete sections:**
- "Hindsight" config table
- "Hindsight API Timeouts" table
- "Circuit Breaker" table
- "Workers & Queue" table (old worker pool vars)
- "Cloud Reranker" section
- Reranker model from prerequisites
- `LLAMA_RERANKER_PORT` from llama table

**Add sections:**
- "Queue" config table with: `QUEUE_DB_PATH`, `QUEUE_MAX_PENDING`, `QUEUE_WORKERS`, `QUEUE_MAX_CONCURRENT`, `QUEUE_JOB_TTL`, `QUEUE_TTL_INTERVAL`
- "Auto-Improve" config table with: `AUTO_IMPROVE_AFTER_N`, `AUTO_IMPROVE_COOLDOWN`
- "Auto-Reflect" config table with: `AUTO_REFLECT_AFTER_N`, `AUTO_REFLECT_TIMEOUT`
- "Error Webhook" row: `ERROR_WEBHOOK_URL`
- Health example JSON update: `{"status":"running","llama":true,"cognee":true}`

**Update:**
- Troubleshooting table: remove Hindsight rows, add queue row: "jobs stuck pending" -> check worker logs
- RAM budget: ~650MB (no reranker, no Hindsight)

### 4c: `docs/development.md`

**Delete:**
- `hindsight.go` from project structure tree
- "New Hindsight operation" from Adding a New Feature
- Hindsight references in debug section (hindsight-crash.log, circuit breaker)
- `worker/` from project structure tree (if it still exists, but queue/ should be listed)

**Add:**
- `queue/` package to project structure tree: `queue/job.go`, `queue/store.go`, `queue/worker.go`
- "New queue behavior" to Adding a New Feature section

**Update:**
- "Quick Reference" — `make setup` description: remove "install Hindsight"
- Conventions: replace "Worker pools: use worker.NewPool()" with "Queue: use queue.NewStore() + queue.NewWorker()"
- Debug section: replace `curl ... | jq '.hindsight'` with `curl ... | jq '.cognee'`
- Circuit breaker section: remove entirely

---

## Task 5: Structured Job Logging

### Purpose

Every state transition in the job lifecycle must emit a structured log line for observability. See Appendix F.

### Log Points (all via `s.log.Info`)

| Event | Message Key | Logged Where | Fields |
|-------|-------------|-------------|--------|
| Job queued (retain) | `job_queued` | `handlers.go` handleToolCall, retain case | `job_id`, `bank`, `type=retain` |
| Job queued (reflect) | `job_queued` | `handlers.go` handleToolCall, reflect case | `job_id`, `bank`, `type=reflect` |
| Job queued (auto-reflect) | `job_queued` | `auto_reflect.go` checkAutoReflect | `job_id`, `bank`, `type=reflect`, `trigger=(count\|timeout\|both)` |
| Job dequeued (worker picks up) | `job_dequeued` | `server.go` processQueueJob, top of function | `job_id`, `bank`, `type` |
| Job completed | `job_completed` | `server.go` processQueueJob, retain success path | `job_id`, `bank`, `type`, `duration_ms` |
| Job completed | `job_completed` | `server.go` processQueueJob, reflect success path | `job_id`, `bank`, `type`, `duration_ms` |
| Job failed (retryable) | `job_failed` | `queue/worker.go` processJob, after UpdateStatus(StatusFailed) and CanRetry() | NOT NEEDED — already logged by server processQueueJob error path |
| Job dead (permanent) | `job_dead` | `queue/worker.go` processJob, after UpdateStatus(StatusDead) | `job_id`, `bank`, `type`, `error`, `retry_count`, `max_retries` |

### Detailed Implementation

#### In handlers.go — retain case (already partially exists):

Current:
```go
s.log.Info("retain_queued", "bank", bank, "job_id", jobID)
```

Change to:
```go
s.log.Info("job_queued", "job_id", jobID, "bank", bank, "type", "retain")
```

#### In handlers.go — reflect case (already partially exists):

Current:
```go
s.log.Info("reflect_queued", "bank", bank, "job_id", jobID)
```

Change to:
```go
s.log.Info("job_queued", "job_id", jobID, "bank", bank, "type", "reflect")
```

#### In auto_reflect.go (already partially exists):

Current:
```go
s.log.Info("auto_reflect triggered", "bank", bank, "trigger", triggerReason(...))
```

Change to add job_id:
```go
s.log.Info("job_queued", "job_id", job.ID, "bank", bank, "type", "reflect", "trigger", triggerReason(countTrigger, timeoutTrigger))
```

And keep the "auto_reflect triggered" line as well (it contains trigger reason).

#### In server.go processQueueJob:

At function top (replaces existing):
```go
s.log.Info("job_dequeued", "job_id", job.ID, "type", job.Type, "bank", job.Bank)
```

Retain success path (modifies existing):
```go
s.log.Info("job_completed", "job_id", job.ID, "bank", job.Bank, "type", "retain", "duration_ms", duration.Milliseconds())
```

Reflect success path (modifies existing):
```go
s.log.Info("job_completed", "job_id", job.ID, "bank", job.Bank, "type", "reflect", "duration_ms", duration.Milliseconds())
```

#### In queue/worker.go processJob — dead path:

The `Worker` struct has no logger. Options:
1. Add a `Logger` field to WorkerConfig — over-engineered for KISS
2. Log in the `OnDead` callback — the callback runs in the Server context and has access to `s.log`

**Chosen: Log in OnDead callback.** In `server.go`:

```go
OnDead: func(job *queue.Job) {
    s.log.Error("job_dead", "job_id", job.ID, "bank", job.Bank, "type", job.Type,
        "error", job.Error, "retry_count", job.RetryCount, "max_retries", job.MaxRetries)
    s.fireErrorWebhook(job.Bank, job.ID, job.Error, job.Type)
},
```

This covers all required log messages without adding complexity to the queue package.

### Log Levels

| Event | Level |
|-------|-------|
| `job_queued` | Info |
| `job_dequeued` | Info |
| `job_completed` | Info |
| `job_dead` | Error |

---

## Task 6: Stale Comment Removal

### Files and Lines to Change

#### `config.go`

Search for "hindsight" (case-insensitive) in comments. None found in current code — already cleaned in M3. No changes needed.

#### `backend/doRequest.go`

Line 13:
```go
// It returns the response body on success. Used by both Hindsight and Cognee backends.
```

Change to:
```go
// It returns the response body on success.
```

#### `session_cleaner.go`

Line 9:
```go
// Extracted from workers.go during M1 (Hindsight removal).
```

Change to:
```go
// Periodically closes and removes idle MCP sessions.
```

#### `auto_improve.go`

Search for "hindsight" — none found. Already clean from M3.

### Verification

After changes, run:
```bash
grep -ri "hindsight" *.go backend/ queue/ --include="*.go"
```
Must return zero results in non-test files. Test files may contain "hindsight" in test names — those are fine (they test the absence).

---

## Task 7: Delete .anon_id

### Action

```bash
rm .anon_id
```

### Verification

File must not exist after deletion. Add to `.gitignore` if not already there to prevent accidental re-creation.

---

## Task 8: Final Vet Pass

### Action

```bash
go vet ./...
```

Must return zero errors. If `go vet` reports any issues, fix them.

### Common Issues to Check

- Unreachable code (from removed IsSync branches)
- Unused imports
- Formatting (`gofmt -s -w .`)

---

## Acceptance Criteria

| ID | Criterion | Verification |
|----|-----------|-------------|
| AC-M5.01 | GET /debug/queue returns 200 with valid JSON | `curl http://localhost:8899/debug/queue` returns all 9 fields with correct types |
| AC-M5.02 | GET /debug/queue returns 405 for non-GET methods | `curl -X POST .../debug/queue` returns 405 |
| AC-M5.03 | GET /debug/queue works when queueStore is nil (returns all zeros) | Test with server in early startup |
| AC-M5.04 | `/debug/queue` Content-Type is `application/json` | Check response headers |
| AC-M5.05 | `oldest_pending_age_s` is 0 when no pending jobs | Validate after queue drains |
| AC-M5.06 | `db_size_kb` is 0 when DB file is missing | Delete queue.db, restart, check |
| AC-M5.07 | `workers` and `max_concurrent` match `QueueWorkerCount` and `QueueMaxConcurrent` config | Compare against config values |
| AC-M5.08 | Dead-letter webhook fires when job transitions to StatusDead | Insert job, exhaust retries, verify webhook endpoint receives POST |
| AC-M5.09 | Dead-letter webhook payload contains job_id, bank, error, operation | Inspect webhook body |
| AC-M5.10 | OnDead callback is nil-safe (no crash if not set) | Create Worker without OnDead, process a permanent failure |
| AC-M5.11 | `.env.example` contains QUEUE_DB_PATH, QUEUE_MAX_PENDING, QUEUE_WORKERS, QUEUE_MAX_CONCURRENT, QUEUE_JOB_TTL, QUEUE_TTL_INTERVAL | grep for each var |
| AC-M5.12 | `.env.example` contains AUTO_REFLECT_AFTER_N, AUTO_REFLECT_TIMEOUT | grep for each var |
| AC-M5.13 | `.env.example` contains AUTO_IMPROVE_AFTER_N, AUTO_IMPROVE_COOLDOWN | grep for each var |
| AC-M5.14 | `.env.example` contains ERROR_WEBHOOK_URL | grep |
| AC-M5.15 | `.env.example` does NOT contain any HINDSIGHT_* var, RERANK_MODEL, CLOUD_RERANKER_*, LLAMA_RERANKER_PORT, MEMORY_RETAIN_WORKERS, MEMORY_REFLECT_WORKERS, COGNEE_MAX_CONCURRENT_RETAINS, BACKEND | grep -v for each |
| AC-M5.16 | `docs/architecture.md` has no "Hindsight" references, no reranker references, no circuit breaker section | grep -i "hindsight\|reranker\|circuit breaker" returns 0 |
| AC-M5.17 | `docs/architecture.md` documents queue architecture, state machine, and debug endpoints | Visual inspection |
| AC-M5.18 | `docs/deployment.md` has no Hindsight/Reranker config tables, has Queue + Auto-Improve + Auto-Reflect tables | Visual inspection |
| AC-M5.19 | `docs/development.md` has no Hindsight references, has queue/ in project structure, has updated conventions | Visual inspection |
| AC-M5.20 | All `s.log.Info("job_queued", ...)` calls emit with `job_id`, `bank`, `type` in both handlers.go retain+reflect paths | Code grep |
| AC-M5.21 | `s.log.Info("job_dequeued", ...)` emitted at top of processQueueJob | Code grep |
| AC-M5.22 | `s.log.Info("job_completed", ...)` emitted with `job_id`, `bank`, `type`, `duration_ms` in retain AND reflect success paths | Code grep |
| AC-M5.23 | `s.log.Error("job_dead", ...)` emitted in OnDead callback | Code grep |
| AC-M5.24 | `grep -ri "hindsight" *.go backend/*.go queue/*.go auto_*.go session_*.go` returns zero results in non-test files | Shell command |
| AC-M5.25 | `backend/doRequest.go` comment no longer says "Used by both Hindsight and Cognee backends" | grep |
| AC-M5.26 | `session_cleaner.go` top comment no longer says "Extracted from workers.go during M1 (Hindsight removal)" | grep |
| AC-M5.27 | `.anon_id` file does not exist | `test -f .anon_id && echo FAIL || echo PASS` |
| AC-M5.28 | `go vet ./...` returns zero output, exit code 0 | Run command |

---

## Edge Cases

### EC-M5.01: db_size_kb on missing DB file
`os.Stat` returns `os.ErrNotExist` for `:memory:` and fresh installs. `db_size_kb` must be 0, no log spam.

### EC-M5.02: OnDead callback panic
If `OnDead` panic, worker goroutine already has defer-recover. The panic is caught and job stays dead. Ensure the dead-job UpdateStatus already happened before OnDead is called, so recovery doesn't resurrect the job.

### EC-M5.03: .env.example merge conflicts
If the user has already partially updated .env.example, the coder must read the current file and surgically add/remove lines. Do not blindly overwrite.

### EC-M5.04: queue/worker.go OnDead field
Adding OnDead to Worker struct means test code in queue/store_test.go, queue/tester_pass2_test.go, queue/tester_pass3_test.go, queue/tester_adversarial_test.go that constructs `queue.WorkerConfig{}` must still compile. The field is optional (nil default = no-op).

### EC-M5.05: Job dead log contains full error
`job.Error` may be long. `s.log.Error` handles this — structured logging doesn't truncate. No additional truncation needed.

### EC-M5.06: /debug/queue and CORS
Same as /health — no CORS headers needed. This is an internal debug endpoint.

---

## New Goroutines

None. No new goroutines are spawned by any M5 task.

- Task 1: synchronous HTTP handler, same as /health
- Task 2: OnDead callback runs synchronously inside existing worker goroutine
- Task 5: structured logging runs synchronously inside existing flow

---

## Non-Goals (Out of Scope for M5)

- Adding authentication to /debug/queue
- Adding Prometheus metrics for queue
- Adding a /debug/queue HTML dashboard
- Changing any existing API response shape
- Adding a queue admin API (pause/resume/flush)
- Removing `circuit_breaker.go` compatibility shim (it's harmless and tests may reference it)
- Reformatting or restructuring existing code beyond the 8 tasks

---

## Verification Checklist (for Tester)

1. `go build ./...` compiles with zero errors
2. `go vet ./...` returns zero output, exit code 0
3. All 28 ACs pass
4. `grep -ri "hindsight" --include="*.go" . | grep -v "_test.go" | grep -v "queue-design.md"` returns zero non-test hits
5. `.anon_id` file is deleted
6. `.env.example` diff shows only additions of queue/auto vars, removal of Hindsight/reranker vars
7. `/debug/queue` endpoint returns correct data with running server
8. Dead-letter webhook fires on permanent job failure
