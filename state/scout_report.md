# Scout Report: SQLite Queue + Hindsight Removal

## 1. IsSync() Call Site Map

The `Backend.IsSync()` interface method is defined at `backend/backend.go:29-32`. Two implementations:

| Backend | File | Line | Returns | Meaning |
|---------|------|------|---------|---------|
| HindsightBackend | `backend/hindsight.go` | 48 | `true` | Sync: worker pool path |
| CogneeBackend | `backend/cognee.go` | 63 | `false` | Async: goroutine+semaphore path |

### Call Site 1: `handlers.go:270` — memory_retain

```
if s.backend.IsSync() {
    // HINDSIGHT PATH: queue to worker pool
    r, err := s.queueJob(s.workers.retainJobs, bank, "retain", a.Content)
    s.mcpToolResult(sid, id, r.Data)
    return
}
// COGNEE PATH: goroutine-per-retain with semaphore
// Acquire semaphore, store jobTracker, spawn goroutine
// Goroutine: defer <-cogneeSemaphore, call s.backend.Retain(), update jobTracker
// Returns {"status":"queued","job_id":"..."}
```

**After removal**: Only Cognee path remains. `s.queueJob()` and entire Hindsight branch eliminated.

### Call Site 2: `handlers.go:365` — memory_reflect

```
if s.backend.IsSync() {
    // HINDSIGHT PATH: queue to worker pool
    r, err := s.queueJob(s.workers.reflectJobs, bank, "reflect", a.Query)
    s.mcpToolResult(sid, id, r.Data)
    return
}
// COGNEE PATH: goroutine, immediate response
s.cogneeWg.Add(1)
go func() { defer s.cogneeWg.Done(); s.backend.Reflect(...) }()
// Returns {"status":"queued","bank":"..."}
```

**After removal**: Only Cognee path remains.

### Call Site 3: `handlers.go:432` — toolsList()

```
if !s.backend.IsSync() {
    tools = append(tools, memory_forget, memory_retain_status)
}
```

**After removal**: Always append `memory_forget` and `memory_retain_status` (Cognee-only tools become universal).

### Call Site 4: `server.go:138` — NewServer() construction

```
if !s.backend.IsSync() {
    s.cogneeSemaphore = make(chan struct{}, config.CogneeMaxConcurrentRetains)
    s.jobTracker = newJobTracker(30 * time.Minute)
    s.cogneeCtx, s.cogneeCancel = context.WithCancel(context.Background())
    s.dataDir = getEnv("DATA_DIR", "./data")
    s.improveState = loadAutoImproveState(s.dataDir)
    go s.jobTrackerCleanup()
}
```

**After removal**: Always construct cognee infrastructure (semaphore, jobTracker, ctx, improveState).

### Call Site 5 (test): `auto_improve_test.go:159`

```
func (m *mockBackend) IsSync() bool { return false }
```

Stays unchanged — tests always test Cognee path.

---

## 2. Hindsight Dependency Map

### Source files directly referencing Hindsight:

| File | Nature of Dependency | Lines |
|------|---------------------|-------|
| `backend/hindsight.go` | Entire file — HindsightBackend struct, HTTP API calls | ~140 |
| `services.go` | `startHindsight()`, `BackendHindsight` case in `start()/stop()/monitor()/allHealthy()` | ~200 |
| `config.go` | `HindsightPort`, `HindsightPath`, all `HINDSIGHT_*` env vars, `HINDSIGHT_*` timeouts, `BackendHindsight` validation branch | ~120 |
| `handlers.go` | IsSync() branching (3 sites), `s.workers.retainJobs/reflectJobs` (only used by Hindsight path) | ~50 |
| `workers.go` | Hindsight worker pools (`retainPool`, `reflectPool`, `retainJobs`, `reflectJobs`, `queueJob()`) | ~150 |
| `server.go` | `memory_forget`, `memory_retain_status` gated on !IsSync() (toolsList, handler registration) | Implicit |
| `errors.go` | `errBinaryNotFound = errors.New("hindsight-api not found")` | 1 line |
| `pids.go` | `pids["hindsight"]` in savePids(), cleanupOrphans() | 2 lines |
| `circuit_breaker.go` | Type alias for `backend.CircuitBreaker` (exists for Hindsight but Cognee also uses circuit breaker) | ~10 |
| `main.go` | Comment: "Phase 1: Start internal services (llama.cpp, Hindsight, workers, health monitor)" | 1 line |
| `backend/backend.go` | `IsSync()` interface method comment references Hindsight | 3 lines |
| `types.go` | `BackendHindsight Backend = "hindsight"` constant | 1 line |

### Config fields to remove:

```
HindsightPath    string    // env: HINDSIGHT_PATH
HindsightPort    string    // env: HINDSIGHT_PORT
LLMProvider      string    // env: HINDSIGHT_LLM_PROVIDER
LLMModel         string    // env: HINDSIGHT_LLM_MODEL
LLMAPIKey        string    // env: OPENROUTER_API_KEY (shared with Cognee)
LLMBaseURL       string    // env: OPENROUTER_BASE_URL (shared with Cognee)
EmbedProvider    string    // env: HINDSIGHT_EMBEDDINGS_PROVIDER
EmbedModel       string    // env: HINDSIGHT_EMBEDDINGS_MODEL
RerankerProvider string    // env: HINDSIGHT_RERANKER_PROVIDER
RerankerModel    string    // env: HINDSIGHT_RERANKER_MODEL
```

And related Hindsight timeout fields:
```
HindsightRetainTimeout  time.Duration  // env: HINDSIGHT_RETAIN_TIMEOUT
HindsightRecallTimeout  time.Duration  // env: HINDSIGHT_RECALL_TIMEOUT
HindsightReflectTimeout time.Duration  // env: HINDSIGHT_REFLECT_TIMEOUT
```

### Config fields for reranker that become orphaned (Cognee doesn't use reranker):

```
LlamaRerankerPort string   // env: LLAMA_RERANKER_PORT
CloudRerankerAPIKey string // env: CLOUD_RERANKER_API_KEY
CloudRerankerURL    string // env: CLOUD_RERANKER_URL
CloudRerankerModel  string // env: CLOUD_RERANKER_MODEL
RerankerModel       string // env: HINDSIGHT_RERANKER_MODEL
RerankerProvider    string // env: HINDSIGHT_RERANKER_PROVIDER
```

And related methods:
```
Config.IsCloudReranker() bool
```

### Config branch for Hindsight validation:
`config.go:338-378` — validates model file existence, cloud reranker fields.

### Process management:

- `services.go:455-477` — `startLlamaReranker()` — spawns second llama-server with `--reranking` flag
- `services.go:479-581` — `startHindsight()` — finds hindsight-api binary, spawns with env vars
- `services.go:126-137` — `stop()` case `BackendHindsight`: stops hindsightCmd and llamaRerankerCmd
- `services.go:163-174` — `monitor()` case `BackendHindsight`: monitors llama reranker + Hindsight
- `services.go:313-338` — `allHealthy()` case `BackendHindsight`: checks llama+reranker+hindsight

### Reranker model file:
- Path: `./model/bge-reranker-base-Q4_k_m.gguf` (209MB)
- Referenced in: `.env.example`, `config.go` (default), `docs/deployment.md`, `docs/Makefile.md`
- Only loaded by Hindsight (reranker llama-server instance)

### Test files referencing Hindsight:

| Test File | Nature |
|-----------|--------|
| `tester_pass2_boundary_test.go` | `allHealthy` tests, cloud modes, health URL, `startHindsight` env injection, port boundaries |
| `tester_cloud_adversarial_test.go` | Cloud embedding/reranker paths, `waitAllHealthy` cloud behavior |
| `tester_pass1_adversarial_test.go` | IsSync() guard test — asserts Hindsight path must exist |
| `tester_pass1_download_test.go` | `start()` skips llama when cloud, tries reranker/hindsight |
| `tester_pass2_venv_boundary_test.go` | `.venv/bin/hindsight-api` discovery tests, FIFO/socket/device file edge cases |
| `tester_pass2_autoimprove_boundary_test.go` | `TestMaybeAutoImprove_HindsightPathIsSync` |
| `stress/stress_test.go` | `TestStressChaos_KillHindsight` — kills hindsight-api process |

### Doc files referencing Hindsight:

| File | Content |
|------|---------|
| `docs/hindsight.md` | Full Hindsight reference — versions, env vars, API endpoints, circuit breaker, known issues |
| `docs/development.md` | Architecture diagram, integration points, debugging |
| `docs/architecture.md` | — |
| `docs/deployment.md` | Config table |
| `docs/Makefile.md` | Reranker model reference |
| `docs/queue-design.md` | Removal plan (target doc) |

### Makefile:
- No direct Hindsight references (no `pip install hindsight-api-slim`)
- References: `RERANK_MODEL` in `docs/Makefile.md`

---

## 3. Preservation Map

### SSE Handler (`handlers.go:79-157`)
- **URL pattern**: `GET /mcp/sse?bank=<bank_name>`
- **Bank validation**: `bankNamePattern = regexp.MustCompile("^[a-zA-Z0-9:_-]{1,128}$")`
- **Session creation**: New session with `MCPSession{SessionID, Bank, SSEChannel, CreatedAt, LastActive}`
- **Session limit**: `s.config.MaxSessions` (default 100) under `sessionsMu.Lock()`
- **Endpoint message**: `event: endpoint\ndata: /mcp/message?session_id=<id>\n\n`
- **SSE loop**: reads from `sess.SSEChannel` via `select`, checks `r.Context().Done()`
- **Cleanup**: `defer delete(s.sessions, id)` in SSE handler goroutine
- **Bank URL decoding**: `url.QueryUnescape(raw)` followed by pattern match

### MCP Message Handler (`handlers.go:159-187`)
- **Route**: `POST /mcp/message?session_id=<id>`
- **Parsing**: JSON-RPC 2.0 (jsonrpc, id, method, params)
- **Dispatching**: `go s.safeRouteMCP(sid, method, id, params)` from HTTP 202 response

### MCP Tools (preserved list):

| Tool | Handler | Preserved? |
|------|---------|-----------|
| `memory_recall` | `handlers.go:231-249` | YES — sync call to backend.Recall |
| `memory_retain` | `handlers.go:251-327` | YES — rewired to SQLite queue |
| `memory_reflect` | `handlers.go:329-376` | YES — rewired to SQLite queue |
| `memory_forget` | `handlers.go:438-457` | YES — was Cognee-only, becomes universal |
| `memory_retain_status` | `handlers.go:459-481` | YES — was Cognee-only, becomes universal |

### Tool schemas (`handlers.go:422-436`):
```
memory_retain:       {content: string}
memory_recall:       {query: string}
memory_reflect:      {query: string} (optional)
memory_forget:       {content_id: string}
memory_retain_status: {job_id: string}
```

### Auto-improve (`auto_improve.go`)
- **State file**: `<dataDir>/improve_state.json` (per-bank counters)
- **Counter logic**: increment on each retain, threshold = `AUTO_IMPROVE_AFTER_N` (default 0 = disabled)
- **Cooldown**: `AUTO_IMPROVE_COOLDOWN` (default 120s)
- **Conditions**: threshold met + semaphore idle + no in-flight + cooldown elapsed
- **Persistence**: atomic write via temp + rename, `saveStateLocked()` under mutex
- **Goroutine**: spawned from `maybeAutoImprove(bank)` at end of Cognee retain goroutine
- **Panic recovery**: full chain of deferred recover()

### Cognee mock (`internal/testutil/cogneemock/`)
- **Purpose**: test helper that runs a httptest.Server simulating Cognee HTTP API
- **Endpoints**: `/health`, `/api/v1/remember`, `/api/v1/recall`, `/api/v1/improve`, `/api/v1/forget`
- **Features**: request capture, configurable responses, port extraction
- **Preserved**: fully — tests depend on it

### Date auto-stamp (`backend/cognee.go:100-103`)
- Regex: `yearRE = regexp.MustCompile(\b(19|20)\d{2}\b)`
- Logic: if content lacks 4-digit year, append " [YYYY-MM-DD]"
- Runs inside CogneeBackend.Retain()

### Metrics (preserved, all at `server.go:72-85` + `metrics/` package):

| Metric | Type | Purpose |
|--------|------|---------|
| `memory.recall` | Counter | recall calls |
| `memory.retain` | Counter | retain calls |
| `memory.reflect` | Counter | reflect calls |
| `memory.errors` | Counter | error calls |
| `memory.retain_duration` | Timer | retain latency |
| `memory.reflect_duration` | Timer | reflect latency |
| `memory.queue_depth` | Gauge | current queue depth |
| `memory.sessions` | Gauge | active sessions |
| `memory.sse_drops` | Counter | dropped SSE messages |
| `memory.retain_total` | Counter | spec-required |
| `memory.retain_errors` | Counter | spec-required |
| `memory.recall_total` | Counter | spec-required |
| `memory.reflect_total` | Counter | spec-required |
| `memory.improve_total` | Counter | spec-required |
| `memory.forget_total` | Counter | spec-required |
| `memory.semaphore_in_use` | Gauge | Cognee semaphore (Cognee only) |
| `memory.cognee_jobs_pending` | Gauge | Cognee pending jobs (Cognee only) |

### Health endpoint (`handlers.go:19-52`)
- **URL**: `GET /health`
- **Response fields**: `status`, `version`, `built`, `hindsight`, `llama`, `reranker`, `down`, `queue_depth`, `retain_workers`, `reflect_workers`, `retain_panics`, `reflect_panics`, `sessions`, `sse_drops`, `uptime`, `panics_total`, `metrics`
- **Computation**: `llama, reranker, hindsight := s.svc.allHealthy()`
- **Status**: `running` if healthy, `degraded` if services down
- **Down list**: lists names of unhealthy services

### Shutdown flow (`main.go:85-100`, `server.go:188-218`)
```
1. Stop HTTP server (stop accepting new connections)
2. Cancel monitor context (stopMonitor())
3. Close shutdown channel (signal sessionCleaner)
4. Cancel cogneeCtx -> cogneeWg.Wait()
5. workers.stop() -> drain pools, close channels
6. Close all sessions
7. svc.stop() -> kill subprocesses
8. clearPids()
9. Set state = StateStopped
```

---

## 4. Current Async Flow (retain, reflect, jobTracker, semaphores)

### Current Cognee retain flow (full trace):

```
handler (handlers.go:270-327):
  1. s.metrics.retainCalls.Inc()
  2. s.metrics.retainTotal.Inc()
  3. IsSync() check → false → COGNEE PATH
  4. jobID = newJobID() (crypto rand, 32 hex chars)
  5. Acquire semaphore: s.cogneeSemaphore <- struct{}{}
     → if full (default 10): return {"status":"rejected","reason":"too_many_concurrent_retains"}
  6. If jobTracker exists: s.jobTracker.store(jobID, bank)
  7. s.cogneeWg.Add(1)
  8. Spawn goroutine:
     - defer cogneeWg.Done()
     - defer <-cogneeSemaphore (release)
     - defer panic recovery (updates jobTracker)
     - Create detached context with CogneeRetainTimeout (900s default)
     - Call s.backend.Retain(detachedCtx, bank, content)
     - On success: jobTracker.complete(jobID, result)
     - On error: jobTracker.fail(jobID, err), fireErrorWebhook()
     - s.maybeAutoImprove(bank)
  9. Return {"status":"queued","bank":"...","job_id":"..."}
```

### Current Cognee reflect flow (full trace):

```
handler (handlers.go:329-376):
  1. s.metrics.reflectCalls.Inc()
  2. s.metrics.reflectTotal.Inc()
  3. IsSync() check → false → COGNEE PATH
  4. s.cogneeWg.Add(1)
  5. Spawn goroutine:
     - defer cogneeWg.Done()
     - defer panic recovery
     - Create detached context with BackendReflectTimeout
     - Call s.backend.Reflect(detachedCtx, bank, query)
     - On error: log + metrics.errorCalls.Inc()
  6. Return {"status":"queued","bank":"..."}  (no job_id!)
```

### jobTracker (`job_tracker.go`)
- **Type**: `map[string]*JobResult` with `sync.RWMutex`
- **TTL**: 30 minutes (configurable but hardcoded in NewServer)
- **TTL cleanup goroutine**: `s.jobTrackerCleanup()` runs every 5 minutes
- **States**: `pending` → `completed` or `failed`
- **Thread safety**: RWMutex on all operations
- **Currently**: only created in Cognee path via `server.go:138-145`

### Semaphore (`server.go:139`)
- **Channel**: `s.cogneeSemaphore = make(chan struct{}, config.CogneeMaxConcurrentRetains)`
- **Default size**: 10 (`COGNEE_MAX_CONCURRENT_RETAINS`)
- **Acquired**: in memory_retain handler before goroutine spawn
- **Released**: in goroutine defer `<-s.cogneeSemaphore`
- **Currently**: only created in Cognee path via `server.go:139`

### cogneeWg (`server.go:146`)
- **Tracks**: Cognee retain goroutines + Cognee reflect goroutines + auto-improve goroutines
- **Add/Done calls**: `handlers.go:289`, `handlers.go:349`, `auto_improve.go:160`
- **Waited**: `server.go:208` — `s.cogneeWg.Wait()` during Stop() after cancel

---

## 5. Deletion Checklist

### FILES TO DELETE (4 files):

| # | File | Lines | Content |
|---|------|-------|---------|
| 1 | `backend/hindsight.go` | ~140 | HindsightBackend struct, HTTP API calls, circuit breaker usage |
| 2 | `model/bge-reranker-base-Q4_k_m.gguf` | — | Reranker model file (209MB) |
| 3 | `.env.hindsight` (if exists) | — | Backend-specific env file |
| 4 | `docs/hindsight.md` | ~150 | Full Hindsight reference doc |

### FILES TO MODIFY (9 files):

| # | File | What to change |
|---|------|----------------|
| 1 | `handlers.go` | Remove 3 IsSync() branches, remove Hindsight path code, make Cognee path unconditional |
| 2 | `services.go` | Remove `startHindsight()`, `BackendHindsight` cases from `start()/stop()/monitor()/allHealthy()`, remove `startLlamaReranker()`, remove `hindsightCmd`, `hindsightFails` fields, simplify services struct |
| 3 | `workers.go` | Remove `retainPool`, `reflectPool`, `retainJobs`, `reflectJobs`, `queueJob()`, `sessionCleaner()` — workers.go becomes much smaller or disappears |
| 4 | `config.go` | Remove all Hindsight env vars, reranker fields, `BackendHindsight` validation branch, simplify to only Cognee config |
| 5 | `server.go` | Make cognee infrastructure unconditional, update comments, simplify Stop() |
| 6 | `backend/backend.go` | Remove `IsSync()` from interface OR keep it but return false always |
| 7 | `pids.go` | Remove `pids["hindsight"]` reference |
| 8 | `errors.go` | Remove `errBinaryNotFound` |
| 9 | `main.go` | Update Phase 1 comment |

### .env.example to MODIFY:
- Remove Hindsight config section (lines 38-47)
- Remove cloud embedding/reranker comments (lines 53-66)
- Update worker pool comment (line 91 — "Each worker makes one Hindsight API call")

### Doc files to MODIFY:
| File | Change |
|------|--------|
| `docs/development.md` | Remove Hindsight references, update architecture diagram |
| `docs/deployment.md` | Remove Hindsight config entries |
| `docs/Makefile.md` | Remove reranker model reference |
| `docs/architecture.md` | Remove Hindsight from architecture |

### Test files to MODIFY/DELETE:

| Test File | Lines | Action |
|-----------|-------|--------|
| `tester_pass1_adversarial_test.go` | ~787-803 | Remove `TestHindsight_ReflectPathUnchanged` — asserts IsSync() guard exists |
| `tester_pass2_autoimprove_boundary_test.go` | ~1186-1196 | Remove `TestMaybeAutoImprove_HindsightPathIsSync` |
| `tester_pass2_boundary_test.go` | ~170-226 | Remove allHealthy hindsight tests, startHindsight tests, port boundary tests |
| `tester_pass2_boundary_test.go` | ~696-720 | Remove `TestCloud_allHealthy_hindsightOnlyWhenBothCloud` |
| `tester_pass2_boundary_test.go` | ~725-870 | Remove `TestCloud_startHindsight_envVarInjection`, port boundary |
| `tester_pass2_boundary_test.go` | ~1143-1175 | Remove `TestCloud_allHealthy_onlyHindsightChecked_race` |
| `tester_pass2_venv_boundary_test.go` | ~350-1025 | Remove all `.venv/bin/hindsight-api` discovery tests (10+ tests) |
| `tester_cloud_adversarial_test.go` | Many | Remove startHindsight, waitAllHealthy cloud behavior tests |
| `tester_pass1_download_test.go` | ~500-570 | Remove reranker/hindsight skip logic tests |
| `stress/stress_test.go` | ~1450-1577 | Remove `TestStressChaos_KillHindsight` |
| `deep_test.go` | ~918 | Update health field check — remove "hindsight" and "reranker" from required fields |
| `auto_improve_test.go` | ~159 | Update `mockBackend.IsSync()` — already returns false, no change needed |

### NEW FILES to create (3 files):

| File | Lines | Content |
|------|-------|---------|
| `queue/job.go` | ~80 | Types: JobStatus enum, Job struct, state machine |
| `queue/store.go` | ~120 | SQLite CRUD: Init, Insert, NextPending, UpdateStatus, StartupRecovery |
| `queue/worker.go` | ~60 | Worker loop: select from SQLite, acquire semaphore, call backend |

---

## 6. Risks & Edge Cases

### Risk 1: IsSync() removal breaks backend.Backend interface contract
- `backend.go:29-32` documents `IsSync() true = worker pool (Hindsight)`. After removal, interface method is dead but still compiles. 
- **Fix**: Keep `IsSync()` but `CogneeBackend` always returns false. Or remove it entirely from interface and update all callers.

### Risk 2: toolsList() gate must flip
- `handlers.go:432`: `if !s.backend.IsSync() { append memory_forget, memory_retain_status }`
- After removal: these tools are always available. But `memory_forget` requires `CogneeBackend.Forget()` — if CogneeBackend doesn't support it, will return `ErrNotSupported`. The handler must handle this gracefully.
- **Fix**: Always append cognee-only tools. Remove IsSync check.

### Risk 3: handleRetainStatus() requires jobTracker
- `handlers.go:466-467`: checks `if s.jobTracker == nil` and returns error
- After removal: jobTracker always exists because it's constructed unconditionally. This nil check becomes dead code but is safe to keep.

### Risk 4: memory_retain returns "rejected" when semaphore full
- `handlers.go:280-283`: if semaphore full, returns `{"status":"rejected","reason":"too_many_concurrent_retains"}`
- With SQLite queue, this rejection should be replaced with queue insertion. The semaphore is still needed for worker-side rate limiting, but the HTTP handler always returns "queued".

### Risk 5: Cognee reflect has no job_id
- `handlers.go:369-374`: Cognee reflect returns `{"status":"queued","bank":"..."}` without a job_id
- If SQLite queue is used for both retain AND reflect, reflect should also get a job_id. Currently reflect has no jobTracker entry, no polling mechanism.

### Risk 6: Reranker fields as orphaned config
- Config fields for reranker (`LlamaRerankerPort`, `RerankerModel`, `CloudReranker*`, `IsCloudReranker()`) are only used by Hindsight. After removal, they become orphaned config with no runtime effect.
- **Fix**: Remove them entirely, or keep in Config struct but mark deprecated.

### Risk 7: `allHealthy()` `healthCache` array size
- `services.go:40`: `healthCache [3]bool` — llama, reranker, hindsight/cognee
- After reranker removal: need to reduce to `[2]bool` or update indexing.
- The `allHealthy()` function's `switch BackendCognee*` branch already handles Cognee (sets `r = true` for reranker). But the array size [3] is a trap.

### Risk 8: Worker pools (`workers.go`) become dead code
- `retainPool`, `reflectPool`, `retainJobs`, `reflectJobs`, `queueJob()`, `sessionCleaner()` — all only used by Hindsight IsSync path.
- After removal: the entire `workers.go` file becomes dead code. Delete it entirely.
- But `sessionCleaner()` merges clean functionality. Move session clean logic to a separate goroutine or keep it.

### Risk 9: `queueJob()` returns blocking result to HTTP handler
- Currently `queueJob()` blocks the HTTP handler until the worker completes (via `<-job.Result`). This means the handler only responds when the backend API call finishes.
- With SQLite queue, the handler should return immediately with job_id, not block. The new pattern is "queue and return" for HTTP, then async worker processes SQLite.

### Risk 10: Hindsight config validation branch dead
- `config.go:338-378` — `BackendHindsight` case validates model file existence and cloud reranker fields. After removal of the BackendHindsight enum value, this branch becomes compile-time dead.
- But tests may use `BackendHindsight` as a constant. Remove the constant to force all code to use Cognee.

### Risk 11: services struct still has hindsight-specific fields
- `services.go:24-36`: `hindsightCmd`, `hindsightFails`, `backendName`
- `services.go:40`: `healthCache [3]bool` 
- Remove `hindsightCmd`, `hindsightFails`. Simplify monitor loop to only Cognee + llama.

### Risk 12: Metrics `semaphoreGauge` and `cogneePending` always created
- `server.go:83-84`: These gauges are created unconditionally in `serverMetrics`. After removal, they will always report live values instead of being zero/N/A. This is backward-compatible (clients can handle them).

### Risk 13: Stress test for kill-Hindsight crash scenario
- `stress/stress_test.go` has `TestStressChaos_KillHindsight` which kills the Hindsight API process. After removal, this test must be rewritten as `TestStressChaos_KillCognee` or removed.

### Risk 14: Auto-improve references cogneeSemaphore for idle check
- `auto_improve.go:113`: `idleCheck := len(s.cogneeSemaphore) <= 1`
- After removal, cogneeSemaphore is always created. This check still works.

### Risk 15: Bank validation still uses `queueJob` for Hindsight
- QueueJob pattern requires `MemoryJob` channel + `worker.Pool`. After SQLite queue, `queueJob()` and `MemoryJob`/`MemoryResult` types in `types.go` become unused. Remove them.

### Risk 16: `healthURL()` function used with HindsightPort
- `services.go:320-332`: `healthURL(svc.config.HindsightPort)` → after removal, CogneePort replaces it.
- `healthURL(svc.config.LlamaRerankerPort)` → remove this call.

### Risk 17: Cognee `healthCache[2]` for hindsight/cognee slot
- `allHealthy()` stores Cognee health in `healthCache[2]` (field index 2). After removal, Cognee becomes `h=healthCache[1]` or simpler — just use `h = result[1]` with a [2]array.
- The 3-slot array `[3]bool{llama, reranker, hindsight}` must be restructured.

### Risk 18: `env.example` worker pool comments
- `.env.example:91`: "Each worker makes one Hindsight API call at a time." → update for Cognee

### Risk 19: `savePids()` references hindsightCmd
- `pids.go:22`: `pids["hindsight"] = svc.hindsightCmd.Process.Pid` — remove this line.
