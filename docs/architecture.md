# Architecture

## System Overview

```
+-----------------------------------------------------+
|                    pi.go Agent                         |
|  mcp.json -> http://localhost:8899/mcp/sse?bank=X      |
+-------------------------+----------------------------+
                          | SSE + JSON-RPC
                          v
+-----------------------------------------------------+
|                  MCP Memory Server (port 8899)          |
|                                                         |
|  +----------+  +--------------------+  +-------------+ |
|  | Sessions |  | SQLite Queue       |  | Health      | |
|  | Map      |  | (queue/store.go)   |  | Monitor     | |
|  | {id->bank}|  | pending->running-> |  | (auto-      | |
|  |          |  | completed/failed/  |  |  restart)   | |
|  +----+-----+  | dead               |  +------+------+ |
|       |        +----+---------------+         |         |
|       |             |                         |         |
|  +----+-----+  +----+-----+                   |         |
|  | Auto-    |  | Worker   |                   |         |
|  | Reflect  |  | Pool     |                   |         |
|  | (M4)     |  | (N goroutines)              |         |
|  +----------+  +----+-----+                   |         |
|                     |                         |         |
+---------------------+-------------------------+---------+
                      |                         |
                      v                         v
              +-------------+           +------------------+
              | Cognee API  |           | llama.cpp        |
              | (retain,    |           | embedder         |
              |  recall,    |           | :8080            |
              |  reflect)   |           | q4_0 KV          |
              +-------------+           +------------------+
                                              OR (cloud)
                                      +------------------+
                                      | Cloud Embedding  |
                                      | API              |
                                      +------------------+
```

## Component Details

### Sessions (`handlers.go`)
- SSE connection creates session with `{id, bank, channel}`
- Bank parsed from URL, immutable after creation
- `sessionsMu` RWMutex protects concurrent access
- 30-min idle cleanup via `sessionCleaner()`
- Max 100 concurrent sessions (configurable)
- **TOCTOU fix:** Session limit enforcement is atomic under `sessionsMu.Lock()`

### SQLite Queue (`queue/`)
- **Store** (`queue/store.go`): SQLite with WAL mode, single-connection pool, startup recovery
- **Worker** (`queue/worker.go`): Pool of N goroutines polling for jobs, semaphore-bounded concurrency
- **State machine:** `pending -> running -> completed/failed/dead`
- **Retry:** Failed jobs with retries remaining go back to `pending`
- **TTL cleanup:** Periodic deletion of completed/failed/dead jobs past retention period
- **Recovery:** On startup, orphaned `running` jobs are reset to `pending`; exhausted `failed` jobs become `dead`
- **Dead-letter callback:** `OnDead` function in `WorkerConfig` fires when a job transitions to `StatusDead`

### Auto-Reflect (`auto_reflect.go`)
- Per-bank trigger state tracking retain count and last reflect time
- **Count trigger:** Fires when retain count reaches `AUTO_REFLECT_AFTER_N`
- **Timeout trigger:** Fires when time since last reflect exceeds `AUTO_REFLECT_TIMEOUT`
- Inserts `_auto` payload reflect job into the queue

### Auto-Improve (`auto_improve.go`)
- Per-bank graph optimization triggered after successful retains
- Cooldown period prevents excessive re-optimization
- Runs asynchronously via `autoImproveWg` for graceful shutdown

### Health Monitor (`services.go`)
- Polls llama.cpp every 5s (configurable)
- Tracks consecutive failures independently per service
- Auto-restarts after 2 consecutive failures (configurable)
- Per-service recovery (doesn't restart everything)
- **Singleflight:** Concurrent health checks deduplicated via `singleflight.Group`
- **Health cache:** Results cached for 10s to avoid multiple HTTP requests per tool call
- **Process exit detection:** Monitors `cmd.ProcessState.Exited()` independently of HTTP health
- **Max restarts:** 5 per service per hour, then stops trying + alert

### Exponential Backoff (`backend/doRequest.go`)
- `doRequest` retries with exponential backoff: `delay * 2^attempt`
- Capped at `MCP_RETRY_MAX_DELAY` (default: 30s)
- Configurable attempts: `MCP_RETRY_ATTEMPTS` (default: 3)
- Per-request timeout: `context.WithTimeout` on each attempt

### Content Size Validation (`handlers.go`)
- `MAX_CONTENT_BYTES` limits input content size (default: 1MB)
- Rejects oversized content before queuing to workers
- Prevents memory exhaustion from large payloads

### Cloud Embedding Support (`services.go`, `config.go`)
- `LLAMA_MODEL_PATH` accepts HTTP/HTTPS URLs for cloud embedding
- When URL detected: `IsCloudEmbedding()` validates the 3 `CLOUD_EMBEDDING_*` env vars are set
- Required vars: `CLOUD_EMBEDDING_API_KEY`, `CLOUD_EMBEDDING_URL`, `CLOUD_EMBEDDING_MODEL`
- Skips local llama.cpp process management

### Debug Endpoints

#### GET /debug/queue
Returns live queue state as JSON for operational monitoring. No authentication required.

**Response:**
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

Returns all zeros if `queueStore` is nil (server starting/shutting down). `oldest_pending_age_s` is 0 when no pending jobs. `db_size_kb` is 0 when DB file is missing.

### Orphan Recovery (`pids.go`)
- `savePids()` runs after services start, writes `logs/.mcp-pids.json`
- `cleanupOrphans()` runs at startup, kills any surviving child processes
- `clearPids()` runs on graceful shutdown
- Survives `kill -9` crashes

## Data Flow

### memory_recall (fast path, ~300ms)
```
Agent -> POST /mcp/message -> goroutine -> s.backend.Recall(bank, query)
  -> HTTP POST to Cognee /recall endpoint (with timeout)
  -> result -> SSE response to agent
```

### memory_retain (queued, ~6-30s)
```
Agent -> POST /mcp/message -> goroutine -> content size validation
  -> queue.Store.Insert(job) -> SSE response {"status":"queued"}
  -> queue.Worker picks up job -> s.backend.Retain(bank, content)
  -> Cognee processing -> job completed/failed
  -> checkAutoReflect(bank) -> maybeAutoImprove(bank)
```

### memory_reflect (queued, ~5-10s)
```
Agent -> POST /mcp/message -> goroutine -> queue.Store.Insert(job)
  -> SSE response {"status":"queued"}
  -> queue.Worker picks up job -> s.backend.Reflect(bank, query)
  -> Cognee synthesis -> job completed/failed
```

### auto-reflect (background)
```
Successful retain -> checkAutoReflect(bank)
  -> count or timeout threshold met?
  -> Insert reflect job with "_auto" payload
  -> queue.Worker processes it
```

## Memory Budget

| Component | RAM |
|-----------|-----|
| llama.cpp embedder (qwen3, q4_0) | ~600MB |
| MCP memory + queue + workers | ~50MB |
| **Total** | **~650MB** |

KV cache quantization (q4_0) saves ~3x vs default f16.

**Note:** Cloud embedding endpoint eliminates local llama.cpp RAM requirement.
