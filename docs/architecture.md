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
|  +----------+  +----------+  +--------------------+   |
|  | Sessions |  | Worker   |  | Health Monitor     |   |
|  | Map      |  | Pools    |  | (auto-restart)     |   |
|  | {id->bank}|  | retainx2 |  | singleflight      |   |
|  |          |  | reflectx2|  |                    |   |
|  +----+-----+  +----+-----+  +--------+-----------+   |
|       |              |                 |                |
|  +----+-----+  +----+-----+           |                |
|  | Circuit  |  | SSE Drop |           |                |
|  | Breaker  |  | Tracker  |           |                |
|  +----------+  +----------+           |                |
+-------+--------------+----------------+----------------+
        |              |                 |
        v              v                 v
+------------+  +------------+  +------------------+
| llama.cpp  |  | Hindsight  |  | llama.cpp        |
| embedder   |  | API        |  | reranker         |
| :8080      |  | :8888      |  | :8081            |
| q4_0 KV    |  | + pg0      |  | q8_0 KV          |
+------------+  +------------+  +------------------+
     OR (cloud)       |          OR (cloud)
+------------+        |          +------------------+
| Cloud API  |        |          | Cloud API        |
| (HTTP URL) |        |          | (HTTP URL)       |
+------------+        |          +------------------+
```

## Component Details

### Sessions (`handlers.go`)
- SSE connection creates session with `{id, bank, channel}`
- Bank parsed from URL, immutable after creation
- `sessionsMu` RWMutex protects concurrent access
- 30-min idle cleanup via `sessionCleaner()`
- Max 100 concurrent sessions (configurable)
- **TOCTOU fix:** Session limit enforcement is atomic under `sessionsMu.Lock()`

### Worker Pools (`workers.go`, `worker/`)
- `retainJobs` / `reflectJobs` -- buffered channels (100)
- `retainPool` / `reflectPool` -- 2 workers each (configurable)
- Workers pull from channels, call Hindsight, return result
- Panic recovery: error returned to caller, worker restarted
- **Context cancellation:** Workers respect `ctx.Done()` for clean shutdown during stop

### Health Monitor (`services.go`)
- Polls llama/Hindsight every 5s (configurable)
- Tracks consecutive failures independently per service
- Auto-restarts after 2 consecutive failures (configurable)
- Per-service recovery (doesn't restart everything)
- **Singleflight:** Concurrent health checks deduplicated via `singleflight.Group`
- **Health cache:** Results cached for 10s to avoid 3 HTTP requests per tool call
- **Process exit detection:** Monitors `cmd.ProcessState.Exited()` independently of HTTP health
- **Max restarts:** 5 per service per hour, then stops trying + alert

### Circuit Breaker (`hindsight.go`)
- Trips after `HINDSIGHT_CIRCUIT_BREAKER_THRESHOLD` consecutive failures (default: 5)
- Cooldown period: `HINDSIGHT_CIRCUIT_BREAKER_COOLDOWN` (default: 30s)
- While tripped: all Hindsight API calls fail fast with "circuit breaker open"
- Cooldown expires: allows one request through to test recovery
- Success resets failure count

### Exponential Backoff (`hindsight.go`)
- `doRequest` retries with exponential backoff: `delay * 2^attempt`
- Capped at `MCP_RETRY_MAX_DELAY` (default: 30s)
- Configurable attempts: `MCP_RETRY_ATTEMPTS` (default: 3)
- Per-request timeout: `context.WithTimeout` on each attempt

### Content Size Validation (`handlers.go`)
- `MAX_CONTENT_BYTES` limits input content size (default: 1MB)
- Rejects oversized content before queuing to workers
- Prevents memory exhaustion from large payloads

### Cloud Embedding/Reranker Support (`services.go`, `config.go`)
- `LLAMA_MODEL_PATH` and `HINDSIGHT_RERANKER_MODEL` accept HTTP/HTTPS URLs
- When URL detected: skips local llama.cpp process management
- Hindsight configured to use remote endpoint directly
- Useful for cloud embedding services (OpenAI, Cohere, etc.)

### Orphan Recovery (`pids.go`)
- `savePids()` runs after services start, writes `logs/.mcp-pids.json`
- `cleanupOrphans()` runs at startup, kills any surviving child processes
- `clearPids()` runs on graceful shutdown
- Survives `kill -9` crashes

## Data Flow

### memory_recall (fast path, ~300ms)
```
Agent -> POST /mcp/message -> goroutine -> s.recallAPI(bank, query)
  -> circuit breaker check
  -> HTTP POST /v1/default/banks/{bank}/recall (with timeout)
  -> Hindsight vector search -> result
  -> SSE response to agent
```

### memory_retain (slow path, ~6-30s)
```
Agent -> POST /mcp/message -> goroutine -> s.queueJob(retainJobs, ...)
  -> content size validation (MAX_CONTENT_BYTES)
  -> push MemoryJob to channel -> worker picks up
  -> s.retainAPI(bank, content)
  -> circuit breaker check
  -> HTTP POST /v1/default/banks/{bank}/memories (with timeout + retry)
  -> Hindsight LLM extraction -> pg0 write -> result
  -> job.Result channel -> SSE response to agent
```

### memory_reflect (slow path, ~5-10s)
```
Same as retain but through reflectJobs channel and /reflect endpoint.
```

## Memory Budget

| Component | RAM |
|-----------|-----|
| llama.cpp embedder (qwen3, q4_0) | ~600MB |
| llama.cpp reranker (bge, q8_0) | ~200MB |
| Hindsight + pg0 | ~200MB |
| MCP memory + workers | ~50MB |
| **Total** | **~1GB** |

KV cache quantization (q4_0/q8_0) saves ~3x vs default f16.

**Note:** Cloud embedding/reranker endpoints eliminate local llama.cpp RAM requirements.
