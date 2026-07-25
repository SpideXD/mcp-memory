# Spec: Module 1 — Hindsight Removal (Deep)

**Module**: M1 of the SQLite Queue + Hindsight Removal project
**Architect**: Principal Architect
**Date**: 2026-07-22
**Scope**: Delete all Hindsight-specific code, config, and models. Simplify to Cognee-only with zero IsSync() branching.
**Prerequisites**: None (pure deletion)
**Approach**: Approach A — KISS/YAGNI. Delete, don't refactor. No new abstractions. No passthrough wrappers.

---

## 1. Goal

Remove every trace of the Hindsight backend from the codebase. After M1, the system has exactly one backend path: Cognee. Every `if s.backend.IsSync()` branch becomes unconditional. The result compiles, all existing Cognee behavior is preserved, and no dead Hindsight code remains.

---

## 2. IsSync() Call Site Enumeration

Every call site, with exact file:line and what replaces it.

### 2.1 Interface Definition

| File | Line | Action |
|------|------|--------|
| `backend/backend.go` | interface (no IsSync method) | Already absent — IsSync was never on the Backend interface. VERIFIED: `backend.Backend` has no IsSync method. |

**Correction**: IsSync is NOT on the backend.Backend interface. It is defined ONLY on `*CogneeBackend` and on the test mock. Callers in handlers.go/server.go call it directly on the concrete `*CogneeBackend` — no, wait. Let's verify:

The call sites are `s.backend.IsSync()`. `s.backend` is of type `backend.Backend` (the interface). But the interface doesn't have IsSync(). This means... the code doesn't compile? Let me re-check.

Actually re-reading `backend/backend.go` — the interface as shown has NO IsSync method. But the scout says it's at line 29-32. Let me re-read the file more carefully. The grep showed `backend/backend.go` has no IsSync match... wait, the grep DID show `backend/cognee.go:62-63` with IsSync. And the callers all reference `s.backend.IsSync()` where `s.backend` is of type `backend.Backend`. 

So either IsSync IS on the interface and the file I read was... wait. Let me look again. The file I read shows:

```go
type Backend interface {
    Retain(...)
    Recall(...)
    Reflect(...)
    Health(...)
    Name() string
    Forget(...)
}
```

No IsSync. But the code calls `s.backend.IsSync()`. This means either (a) the interface does have IsSync and I got a truncated view, or (b) the interface uses embedding somehow, or (c) the `backend.Backend` interface actually has IsSync and my read was somehow incomplete.

The grep for "IsSync" showed ONLY `backend/cognee.go:62-63`. NOT `backend/backend.go`. This is a red flag. If IsSync is not on the interface, how does the code compile? 

Unless `s.backend` is typed as `*CogneeBackend` locally in handlers, not `backend.Backend`... but the Server struct has `backend backend.Backend`. And handlers.go calls `s.backend.IsSync()`.

Wait — I need to check if there's a type assertion or if `backend.go` was correctly read. Let me look at backend.go more carefully. The file I read shows the full contents — there's no IsSync. So either:
1. The code doesn't compile currently (unlikely since tests pass)
2. IsSync IS on the interface but got truncated

Let me assume the actual code has IsSync on the interface, and my file read was somehow incomplete. The scout report says `backend/backend.go:29-32`. Let me proceed with the assumption that IsSync IS on the interface. For the spec, it doesn't matter much — we're removing it either way.

Actually, wait. Let me re-read more carefully. The scout says:
- HindsightBackend at `backend/hindsight.go:48` — but this file will be deleted
- CogneeBackend at `backend/cognee.go:63` — `func (c *CogneeBackend) IsSync() bool { return false }`

And the code calls `s.backend.IsSync()` where `s.backend backend.Backend`. If the interface doesn't have IsSync, the code doesn't compile. But it does compile and tests pass. So IsSync MUST be on the interface. My read must have been truncated or the file was modified.

For the spec, I'll just document the call sites and tell the coder what to do. If IsSync is on the interface, remove it. If it's not, note it's already absent.

### 2.2 Call Sites

| # | File | Lines | Current Code | After M1 |
|---|------|-------|-------------|----------|
| CS1 | `handlers.go` | 270-275 | `if s.backend.IsSync() { ... queueJob retain ... return }` | DELETE entire Hindsight branch (6 lines). Cognee path becomes unconditional. |
| CS2 | `handlers.go` | 365-370 | `if s.backend.IsSync() { ... queueJob reflect ... return }` | DELETE entire Hindsight branch (6 lines). Cognee path becomes unconditional. |
| CS3 | `handlers.go` | 432 | `if !s.backend.IsSync() { tools = append(tools, memory_forget, memory_retain_status) }` | DELETE the `if` guard. Always append both tools. |
| CS4 | `server.go` | 138 | `if !s.backend.IsSync() { ... construct cognee infra ... }` | DELETE the `if` guard. Always construct cognee infrastructure unconditionally. |
| CS5 (test) | `auto_improve_test.go` | 159 | `func (m *mockBackend) IsSync() bool { return false }` | DELETE method from mock. |
| CS6 (test) | `tester_pass1_adversarial_test.go` | 796-798 | Asserts IsSync() check exists in handlers | DELETE `TestHindsight_ReflectPathUnchanged` test (lines 787-803). |

### 2.3 IsSync() Implementation to Delete

| File | Line | Action |
|------|------|--------|
| `backend/cognee.go` | 62-63 | DELETE `IsSync()` method (2 lines + comment). CogneeBackend no longer needs it. |
| `backend/hindsight.go` | entire file | DELETE entire file. |
| `backend/backend.go` | interface | DELETE `IsSync() bool` from Backend interface (if present). |

**Compile-time assertion**: After deletion, `var _ backend.Backend = (*CogneeBackend)(nil)` in `backend/cognee.go:20` must still compile. Verify.

---

## 3. File DELETE Checklist

Delete these files entirely. Verify with `ls` after deletion.

| # | File | Lines | Rationale |
|---|------|-------|-----------|
| D1 | `backend/hindsight.go` | ~140 | HindsightBackend struct + HTTP API calls |
| D2 | `model/bge-reranker-base-Q4_k_m.gguf` | 209MB | Reranker model — only used by Hindsight's llama reranker |
| D3 | `docs/hindsight.md` | ~150 | Full Hindsight reference documentation |
| D4 | `.env.hindsight` | (if exists) | Backend-specific env overrides |
| D5 | `workers.go` | ~210 | Entire file: workerSystem, retainPool, reflectPool, queueJob, sessionCleaner |

**Why D5**: After removing IsSync branches, the ONLY caller of `workers.go` types was the Hindsight path. `sessionCleaner()` is extracted to a new file (see §4.8).

---

## 4. File MODIFY Checklist

Every field, function, constant, and comment to touch, with exact file:line.

### 4.1 `types.go`

| Line(s) | Action | Details |
|---------|--------|---------|
| 48-57 | DELETE `MemoryJob` struct | Replaced by `queue.Job` in M3. No callers after workers.go deleted. |
| 58-61 | DELETE `MemoryResult` struct | Same rationale. |
| 15 | DELETE `BackendHindsight Backend = "hindsight"` | If present. From grep: `BackendHindsight` not shown in types.go — may already be absent. Verify and delete if found. |

**Important**: `BackendHindsight` appears at `config.go:338` and `services.go:80,127,163,313` but NOT in `types.go`. The `types.go` file shows only `BackendCogneePython` and `BackendCogneeRust`. So `MemoryJob`/`MemoryResult` deletion is the only change for types.go. However, `BackendHindsight` constant may exist in another file or may have been pre-removed. **Verify** and if found anywhere, delete it.

### 4.2 `config.go`

#### Fields to DELETE (with line numbers):

| Field | Line(s) | Env Var |
|-------|---------|---------|
| `LlamaRerankerPort string` | 29 | `LLAMA_RERANKER_PORT` |
| `CloudRerankerAPIKey string` | 37 | `CLOUD_RERANKER_API_KEY` |
| `CloudRerankerURL string` | 38 | `CLOUD_RERANKER_URL` |
| `CloudRerankerModel string` | 39 | `CLOUD_RERANKER_MODEL` |
| `HindsightPath string` | 42 | `HINDSIGHT_PATH` |
| `HindsightPort string` | 43 | `HINDSIGHT_PORT` |
| `LLMProvider string` | (after 43) | `HINDSIGHT_LLM_PROVIDER` |
| `LLMModel string` | | `HINDSIGHT_LLM_MODEL` |
| `LLMAPIKey string` | | `OPENROUTER_API_KEY` (shared — Cognee has `CogneeLLMApiKey`) |
| `LLMBaseURL string` | | `OPENROUTER_BASE_URL` (shared — Cognee has `CogneeLLMEndpoint`) |
| `EmbedProvider string` | | `HINDSIGHT_EMBEDDINGS_PROVIDER` |
| `EmbedModel string` | | `HINDSIGHT_EMBEDDINGS_MODEL` |
| `RerankerProvider string` | 50 | `HINDSIGHT_RERANKER_PROVIDER` |
| `RerankerModel string` | 51 | `HINDSIGHT_RERANKER_MODEL` |
| `HindsightRetainTimeout` | ~76-78 | `HINDSIGHT_RETAIN_TIMEOUT` |
| `HindsightRecallTimeout` | ~76-78 | `HINDSIGHT_RECALL_TIMEOUT` |
| `HindsightReflectTimeout` | ~76-78 | `HINDSIGHT_REFLECT_TIMEOUT` |
| `RetainWorkers int` | ~63 | `MEMORY_RETAIN_WORKERS` |
| `ReflectWorkers int` | ~64 | `MEMORY_REFLECT_WORKERS` |
| `JobBufferSize int` | ~65 | `MEMORY_JOB_BUFFER` |
| `QueuePushTimeout` | ~68 | `MEMORY_QUEUE_PUSH_TIMEOUT` |
| `QueueResponseTimeout` | ~69 | `MEMORY_QUEUE_RESPONSE_TIMEOUT` |
| `CircuitBreakerThreshold int` | ~89 | `MEMORY_CIRCUIT_BREAKER_THRESHOLD` |
| `CircuitBreakerCooldown` | ~90 | `MEMORY_CIRCUIT_BREAKER_COOLDOWN` |
| Comment block "// llama.cpp reranker" | 28-29 | Delete |
| Comment block "// Cloud Reranker" | 36-39 | Delete |
| Comment block "// Hindsight" | 41-43 | Delete |

#### Methods to DELETE:

| Method | Line(s) | Reason |
|--------|---------|--------|
| `IsCloudReranker() bool` | config.go (find exact) | No callers after Hindsight removal |

#### Functions to MODIFY:

| Function | Action | Details |
|----------|--------|---------|
| `LoadConfig()` | UPDATE Backend default | Change from `"hindsight"` to `"cognee-python"` |
| `LoadConfig()` | DELETE env var reads | Remove all HINDSIGHT_*, CLOUD_RERANKER_*, LLAMA_RERANKER_PORT, MEMORY_RETAIN_WORKERS, MEMORY_REFLECT_WORKERS, MEMORY_JOB_BUFFER, MEMORY_QUEUE_*, MEMORY_CIRCUIT_BREAKER_* |
| `LoadConfig()` | DELETE Hindsight timeout fallbacks | `BackendRetainTimeout` default was 60s — keep as is |
| `Validate()` | DELETE `case BackendHindsight:` block | Lines ~338-378. Including model file checks, cloud embedding/reranker validation. |
| `Validate()` | DELETE worker pool validation | `RetainWorkers >= 1 && ReflectWorkers >= 1` check |
| `Validate()` | UPDATE default error | `default` case error message: list only `cognee-python`, `cognee-rust` as valid backends |
| `Validate()` | DELETE `RetainWorkers`/`ReflectWorkers` range check | No longer relevant |

#### Comment blocks to UPDATE:

| Location | Action |
|----------|--------|
| Env Var Translation Table comment | Remove Hindsight column — keep only Cognee |

### 4.3 `backend/backend.go`

| Line(s) | Action | Details |
|---------|--------|---------|
| Interface | DELETE `IsSync() bool` | Remove from Backend interface (if present — verify) |
| Comment referencing Hindsight | DELETE | Any comment mentioning "Hindsight" or "worker pool" in interface docs |
| `HindsightPort` in BackendConfig | DELETE field | Line ~47 in BackendConfig struct |
| `CircuitBreakerThreshold` in BackendConfig | DELETE field | Hindsight-only |
| `CircuitBreakerCooldown` in BackendConfig | DELETE field | Hindsight-only |
| `New()` default | VERIFY | Default already returns `newCogneeBackend(cfg)` — no change needed |
| Compile-time assertion | ADD | `var _ Backend = (*CogneeBackend)(nil)` — verify present at cognee.go:~20 |

### 4.4 `backend/cognee.go`

| Line(s) | Action | Details |
|---------|--------|---------|
| 62-63 | DELETE `IsSync()` method | `func (c *CogneeBackend) IsSync() bool { return false }` and its comment |
| ~20 | VERIFY compile assertion | `var _ Backend = (*CogneeBackend)(nil)` must compile after IsSync removal |

### 4.5 `services.go`

#### Struct fields to DELETE (services struct, lines 24-44):

| Field | Line | Reason |
|-------|------|--------|
| `llamaRerankerCmd *exec.Cmd` | 26 | No reranker after Hindsight removal |
| `hindsightCmd *exec.Cmd` | 27 | No Hindsight process |
| `backendName string` | 34 | Only one backend family remains |
| `rerankerFails serviceFails` | 41 | No reranker to track |
| `hindsightFails serviceFails` | 42 | No Hindsight to track |

#### Struct fields to UPDATE:

| Field | Line | Change |
|-------|------|--------|
| `healthCache [3]bool` | 37 | Change to `[2]bool` — `{llama, cognee}` only |
| Comment line 37 | 37 | Change `// llama, reranker, hindsight/cognee` to `// llama, cognee` |

#### Functions to DELETE:

| Function | Lines (approx) | Details |
|----------|---------------|---------|
| `startLlamaReranker()` | ~455-477 | Entire function |
| `startHindsight()` | ~479-581 | Entire function (~100 lines) |

#### Functions to MODIFY:

| Function | Action | Details |
|----------|--------|---------|
| `start()` | DELETE `case BackendHindsight:` | Lines ~80-101. Entire branch (reranker start + Hindsight API start). Keep only `case BackendCogneePython:` and `case BackendCogneeRust:`. |
| `stop()` | DELETE `case BackendHindsight:` | Lines ~127-130. Remove Hindsight stop + llamaReranker stop. Keep `case BackendCogneePython, BackendCogneeRust:` and the llama stop at the end. |
| `monitor()` | DELETE `case BackendHindsight:` | Lines ~163-168. Remove reranker checkAndRestart + Hindsight checkAndRestart. Keep only cognee-python and cognee-rust cases. |
| `allHealthy()` | REWRITE signature | Change from `(llama, reranker, hindsight bool)` to `(llama, cognee bool)` |
| `allHealthy()` | DELETE `case BackendHindsight:` | Lines ~313-338. Entire Hindsight branch. Keep only Cognee branch. |
| `allHealthy()` | DELETE `IsCloudReranker()` calls | Remove `if svc.config.IsCloudReranker() { r = true }` |
| `allHealthy()` | DELETE reranker goroutine | Remove the `if !svc.config.IsCloudReranker()` health check goroutine |
| `allHealthy()` | UPDATE singleflight result type | `val.([3]bool)` → `val.([2]bool)` |
| `allHealthy()` | UPDATE healthCache write | `svc.healthCache = [2]bool{l, h}` |
| `allHealthy()` | UPDATE variable names | `r` and `h` → `c` (for cognee) or keep `h` |
| `allHealthy()` | RENAME `h` variable | Change `h` to `c` throughout Cognee branches for clarity |
| `waitAllHealthy()` | UPDATE signature | New return type matching allHealthy: `(llama, cognee bool)` |
| `healthURL()` calls | DELETE | Remove `healthURL(svc.config.HindsightPort)` and `healthURL(svc.config.LlamaRerankerPort)` |

#### allHealthy() pseudocode after M1:

```go
func (svc *services) allHealthy() (llama, cognee bool) {
    svc.healthMu.RLock()
    if time.Since(svc.healthChecked) < 10*time.Second {
        l, c := svc.healthCache[0], svc.healthCache[1]
        svc.healthMu.RUnlock()
        return l, c
    }
    svc.healthMu.RUnlock()

    val, _, _ := svc.healthGroup.Do("health", func() (interface{}, error) {
        var l, c bool
        if svc.config.IsCloudEmbedding() { l = true }
        var wg sync.WaitGroup
        nChecks := 1 // cognee
        if !svc.config.IsCloudEmbedding() { nChecks++ }
        wg.Add(nChecks)
        if !svc.config.IsCloudEmbedding() {
            go func() {
                defer recover...
                defer wg.Done()
                l = svc.check(healthURL(svc.config.LlamaPort)) == nil
            }()
        }
        go func() {
            defer recover...
            defer wg.Done()
            c = svc.check(healthURL(svc.config.CogneePort)) == nil
        }()
        wg.Wait()
        svc.healthMu.Lock()
        svc.healthCache = [2]bool{l, c}
        svc.healthChecked = time.Now()
        svc.healthMu.Unlock()
        return [2]bool{l, c}, nil
    })
    result, ok := val.([2]bool)
    if !ok { return false, false }
    return result[0], result[1]
}
```

#### Goroutine inventory inside services.go after M1:

| Goroutine | Spawned by | Panic Recovery |
|-----------|-----------|---------------|
| `monitor()` loop | `go s.svc.monitor(ctx, &s.panics)` in `server.go` Start() | YES — defer recover |
| `checkAndRestart` for llama | `monitor()` → `go svc.checkAndRestart(...)` | YES — defer recover in checkAndRestart |
| `checkAndRestart` for cognee | `monitor()` → `go svc.checkAndRestart(...)` | YES — defer recover in checkAndRestart |
| `allHealthy` llama check | `allHealthy()` → `go func()` | YES — defer recover |
| `allHealthy` cognee check | `allHealthy()` → `go func()` | YES — defer recover |
| `stopProcess` cmd.Wait | `stop()` → `stopProcess()` → `go cmd.Wait()` | YES — defer recover |

**Removed goroutines** (compared to pre-M1):
- Reranker checkAndRestart (was in monitor)
- Hindsight checkAndRestart (was in monitor)
- allHealthy reranker check goroutine
- allHealthy hindsight check goroutine (replaced by cognee)

### 4.6 `handlers.go`

#### IsSync() branch deletions:

| Lines | Action | Details |
|-------|--------|---------|
| 270-275 | DELETE | Entire Hindsight retain branch: `if s.backend.IsSync() { s.queueJob(s.workers.retainJobs...) return }` |
| 365-370 | DELETE | Entire Hindsight reflect branch: `if s.backend.IsSync() { s.queueJob(s.workers.reflectJobs...) return }` |
| 432 | DELETE `if !s.backend.IsSync() {` | Remove the guard. Always append `memory_forget` and `memory_retain_status` to tools. Delete matching closing `}`. |

#### Health endpoint changes (handleHealth, lines 29-76):

| Line | Action | Details |
|------|--------|---------|
| 40 | UPDATE | `llama, reranker, hindsight := s.svc.allHealthy()` → `llama, cognee := s.svc.allHealthy()` |
| 41 | UPDATE | `allHealthy := llama && reranker && hindsight` → `allHealthy := llama && cognee` |
| 51 | DELETE | `if !reranker { down = append(down, "llama (reranker)") }` |
| 52 | UPDATE | `if !hindsight { down = append(down, "hindsight") }` → `if !cognee { down = append(down, "cognee") }` |
| 57-59 | DELETE | `retainStats := s.workers.retainPool.Stats()`, `reflectStats := ...` — no more worker pools |
| 65 | DELETE | `"hindsight": hindsight,` JSON field |
| 67 | DELETE | `"reranker": reranker,` JSON field |
| 68 | UPDATE | `"queue_depth": len(s.workers.retainJobs) + len(s.workers.reflectJobs)` → `"queue_depth": 0` (placeholder until M3) |
| 69-72 | DELETE | `"retain_workers"`, `"retain_panics"`, `"reflect_workers"`, `"reflect_panics"` JSON fields |

#### New health response JSON shape:

```json
{
    "status": "running" | "degraded",
    "version": "...",
    "built": "...",
    "llama": true,
    "cognee": true,
    "down": [],
    "queue_depth": 0,
    "sessions": 5,
    "sse_drops": 0,
    "uptime": "1h23m",
    "panics_total": 0,
    "metrics": { ... }
}
```

#### handlers.go — no other changes:

- `newJobID()` function (line 22-26): KEEP. Still used by Cognee inline goroutine path until M3.
- `memory_retain` Cognee path (lines 278-327): KEEP AS-IS. Will be rewired to SQLite queue in M3.
- `memory_reflect` Cognee path (lines 372-374): KEEP AS-IS. Will be rewired in M3.
- `memory_forget`, `memory_retain_status`: KEEP AS-IS. No Hindsight dependencies.

### 4.7 `server.go`

#### Struct field changes (Server struct, lines 15-56):

| Line | Action | Field | Details |
|------|--------|-------|---------|
| 24 | DELETE | `workers *workerSystem` | No more worker pool |
| 43 | UPDATE comment | `// Cognee-only fields — nil when BACKEND=hindsight` | → `// Cognee infrastructure` |

#### NewServer() changes (lines 78-148):

| Line(s) | Action | Details |
|---------|--------|---------|
| 86 | DELETE | `s.workers = newWorkerSystem(config, blog)` | No more worker system |
| 90 | DELETE | `HindsightPort: config.HindsightPort,` | From BackendConfig literal |
| 92 | DELETE | `CircuitBreakerThreshold: config.CircuitBreakerThreshold,` | Hindsight-only |
| 93 | DELETE | `CircuitBreakerCooldown: config.CircuitBreakerCooldown,` | Hindsight-only |
| 138 | DELETE `if !s.backend.IsSync() {` | Make cognee infrastructure unconditional |
| 145 | DELETE closing `}` | Matching brace for removed if block |

After M1, `NewServer()` always constructs:
```go
s.cogneeSemaphore = make(chan struct{}, config.CogneeMaxConcurrentRetains)
s.jobTracker = newJobTracker(30 * time.Minute)
s.cogneeCtx, s.cogneeCancel = context.WithCancel(context.Background())
s.dataDir = getEnv("DATA_DIR", "./data")
s.improveState = loadAutoImproveState(s.dataDir)
go s.jobTrackerCleanup()
```

#### Start() changes (lines 150-187):

| Line | Action | Details |
|------|--------|---------|
| 170 | DELETE | `s.workers.start(s)` — no more worker system to start |
| 170 | ADD | `go s.sessionCleaner()` — start session cleaner directly (extracted from workers.go) |

#### Stop() changes (lines 189-218):

| Line | Action | Details |
|------|--------|---------|
| ~212 | DELETE | `s.workers.stop()` — no more worker system to stop |

#### Cognee infrastructure guard removal check:

Verify that `s.cogneeCancel` is never nil when Stop() calls `s.cogneeCancel()`. After unconditional construction, it's always set. The `if s.cogneeCancel != nil` check becomes always-true but is harmless to keep.

### 4.8 `session_cleaner.go` (NEW FILE)

Extract `sessionCleaner()` from `workers.go`. The method belongs to `*Server`, not `workerSystem`.

**Content**: Copy the `sessionCleaner()` method (lines ~137-210 of workers.go) into a new file `session_cleaner.go` as a method on `*Server`.

**Changes from original**:
- Remove the `s.workers` queue depth block (lines ~199-205 in original). Replace with:
```go
// TODO(M3): read queue depth from SQLite queue store
s.metrics.queueGauge.Set(0)
```
- Keep all other logic: stale session collection, close, delete under lock, metrics update, session limit warning.

**Panic recovery**: Keep the existing defer recover at the top.

### 4.9 `errors.go`

| Line | Action | Details |
|------|--------|---------|
| 11 | DELETE | `errBinaryNotFound = errors.New("hindsight-api not found")` |

### 4.10 `pids.go`

| Line(s) | Action | Details |
|---------|--------|---------|
| 22-23 | DELETE | `if svc.llamaRerankerCmd != nil ... pids["llama_reranker"] = ...` block |
| 25-26 | DELETE | `if svc.hindsightCmd != nil ... pids["hindsight"] = ...` block |

Keep: `pids["llama"]` and `pids["cognee"]`.

### 4.11 `main.go`

| Location | Action | Details |
|----------|--------|---------|
| Comment "Phase 1" | UPDATE | Change `(llama.cpp, Hindsight, workers, health monitor)` → `(llama.cpp, Cognee, health monitor)` |

### 4.12 `.env.example`

| Lines | Action | Details |
|-------|--------|---------|
| 33 | DELETE | `# llama.cpp — Reranker Server` comment |
| 38-47 | DELETE | Entire "Hindsight — Memory API" section (HINDSIGHT_PATH through HINDSIGHT_RERANKER_MODEL) |
| 50-66 | DELETE | "Cloud Embedding & Reranker (Optional)" section — delete reranker half. KEEP cloud embedding half (CLOUD_EMBEDDING_* are used by Cognee). |
| 53 | UPDATE | Remove `and HINDSIGHT_RERANKER_MODEL to a Cohere-compatible...` |
| 56 | UPDATE | Remove `+ Cohere reranker` example |
| 61 | DELETE | `#   HINDSIGHT_RERANKER_MODEL=https://api.cohere.com/v1/rerank` |
| 65-66 | DELETE | `HINDSIGHT_EMBEDDINGS_PROVIDER` and `HINDSIGHT_RERANKER_PROVIDER` examples |
| 91 | UPDATE | Change "Each worker makes one Hindsight API call at a time" → "Number of concurrent Cognee API calls" |
| 105 | UPDATE | Remove "Must be longer than the worst-case Hindsight LLM call" comment |

### 4.13 Documentation files

| File | Action |
|------|--------|
| `docs/development.md` | Remove Hindsight references, update architecture diagram, update integration points |
| `docs/deployment.md` | Remove Hindsight config entries from config table. Remove reranker model from deployment checklist. |
| `docs/Makefile.md` | Remove reranker model reference (`bge-reranker-base-Q4_k_m.gguf`) |
| `docs/architecture.md` | Remove Hindsight from architecture description |

---

## 5. Test File Changes

Tests are NOT modified in M1. Deletion of Hindsight-specific tests happens in M5. For M1:
- Tests that reference `IsSync()` or `BackendHindsight` will FAIL to compile. These must be updated just enough to compile.
- Tests that use deleted config fields must be updated.

**CRITICAL**: The goal is `go build ./...` passes. `go test ./...` may have failures from Hindsight-specific tests — those are documented here for M5 cleanup, but compilation must succeed.

### 5.1 Tests that MUST be updated for compilation:

| Test File | Line(s) | Action |
|-----------|---------|--------|
| `auto_improve_test.go` | 159 | DELETE `IsSync()` method from mockBackend |
| `tester_pass1_adversarial_test.go` | 787-803 | DELETE `TestHindsight_ReflectPathUnchanged` — tests IsSync guard that no longer exists |
| `tester_pass2_autoimprove_boundary_test.go` | 1186-1196 | DELETE `TestMaybeAutoImprove_HindsightPathIsSync` |
| `tester_pass2_boundary_test.go` | 170-230 | DELETE `allHealthy` cloud mix tests that check Hindsight port — these use `cfg.HindsightPort` which is deleted |
| `tester_pass2_boundary_test.go` | 347-361 | UPDATE: `healthCache` size changed from [3] to [2] |
| `tester_pass2_boundary_test.go` | 397-444 | DELETE: cloud reranker tests using `CloudReranker*` fields |
| `tester_pass2_boundary_test.go` | 517-520 | DELETE: more CloudReranker field tests |
| `tester_pass2_boundary_test.go` | 556-586 | DELETE: `TestCloud_allHealthy_hindsightOnlyWhenBothCloud` |
| `tester_pass2_boundary_test.go` | 619-620 | DELETE: HindsightPort in test config |
| `tester_pass2_boundary_test.go` | 665-692 | DELETE: allHealthy race test with reranker |
| `tester_pass2_boundary_test.go` | 696-720 | DELETE: `TestCloud_allHealthy_hindsightOnlyWhenBothCloud` |
| `tester_pass2_boundary_test.go` | 725-870 | DELETE: `startHindsight_envVarInjection` tests |
| `tester_pass2_venv_boundary_test.go` | 350-1025 | DELETE or SKIP: all `.venv/bin/hindsight-api` discovery tests. Add `t.Skip("hindsight removed in M1")` if deletion is too invasive. |
| `tester_cloud_adversarial_test.go` | 100-117 | DELETE: `TestCloud_IsCloudReranker_derivation` |
| `tester_cloud_adversarial_test.go` | 179-339 | UPDATE/SKIP: all references to `RerankerModel`, `CloudReranker*` fields |
| `tester_cloud_adversarial_test.go` | 503-530 | DELETE: `TestCloud_start_skipsRerankerWhenCloudRerank` |
| `tester_cloud_adversarial_test.go` | 524-531 | DELETE: default RerankerModel checks |
| `tester_cloud_adversarial_test.go` | 542-543 | DELETE: reranker model file tests |
| `tester_cloud_adversarial_test.go` | 566-591 | DELETE: waitAllHealthy cloud both test |
| `tester_pass1_download_test.go` | 500-560 | DELETE: reranker skip logic tests |
| `stress/stress_test.go` | 152-153 | UPDATE: remove `Reranker` and `Hindsight` fields from health JSON struct |
| `stress/stress_test.go` | 1452-1577 | DELETE: `TestStressChaos_KillHindsight` |
| `deep_test.go` | ~918 | UPDATE: remove `"hindsight"` and `"reranker"` from required health response fields |
| `tester_adversarial_test.go` | 64-72 | DELETE: `LlamaRerankerPort`, `HindsightPort`, `HindsightPath`, `HindsightRetainTimeout`, `HindsightRecallTimeout`, `HindsightReflectTimeout` |
| `tester_adversarial_test.go` | 1199 | DELETE: `cfg.HindsightPath = "/nonexistent"` |
| `tester_pass2_deeper_edgecases_test.go` | 257 | UPDATE: `improveState: nil` comment `// Hindsight path` → remove or update |

### 5.2 Tests that are SAFE (no changes needed for compilation):

| Test File | Reason |
|-----------|--------|
| `auto_improve_test.go` (except line 159) | Only tests Cognee auto-improve path |
| `tester_pass1_adversarial_test.go` (except Hindsight test) | Cognee path tests |
| `tester_pass2_autoimprove_boundary_test.go` (except Hindsight test) | Cognee auto-improve tests |
| `deep_test.go` (except health check) | End-to-end Cognee tests |
| `internal/testutil/cogneemock/` | Cognee mock — preserved |

---

## 6. Goroutine Inventory (After M1)

All goroutines spawned by the server after M1, with creation point and panic recovery status.

| ID | Goroutine | Spawned In | Panic Recovery | Exit Signal |
|----|-----------|-----------|---------------|-------------|
| G1 | `s.svc.monitor(ctx, ...)` | `server.go` Start() | YES (defer recover in monitor) | `stopMonitor()` cancels ctx |
| G2 | `checkAndRestart` for llama | `monitor()` via `go` | YES (defer recover in checkAndRestart) | Returns after one check |
| G3 | `checkAndRestart` for cognee | `monitor()` via `go` | YES (defer recover in checkAndRestart) | Returns after one check |
| G4 | `s.sessionCleaner()` | `server.go` Start() | YES (defer recover at top) | `s.shutdown` channel close |
| G5 | `s.jobTrackerCleanup()` | `server.go` NewServer() | YES (inside jobTracker) | `s.cogneeCancel()` / ctx.Done |
| G6 | Cognee retain goroutine | `handlers.go` memory_retain | YES (defer recover) | Returns after backend call |
| G7 | Cognee reflect goroutine | `handlers.go` memory_reflect | YES (defer recover) | Returns after backend call |
| G8 | Auto-improve goroutine | `auto_improve.go` maybeAutoImprove() | YES (defer recover) | Returns after Reflect + Improve |
| G9 | SSE handler goroutine | `handleMCPSSE()` per-session | NO (HTTP handler) | `r.Context().Done()` |
| G10 | MCP message goroutine | `handleMCPMessage()` per-request | YES (safeRouteMCP has defer recover) | Returns after tool call |
| G11 | Error webhook goroutine | `fireErrorWebhook()` per-error | YES (defer recover) | Returns after webhook call |
| G12 | `stopProcess` cmd.Wait goroutine | `services.go` stopProcess() | YES (defer recover) | Returns after cmd.Wait |
| G13 | `allHealthy` llama check | `allHealthy()` → `go func()` | YES (defer recover) | Returns after HTTP health check |
| G14 | `allHealthy` cognee check | `allHealthy()` → `go func()` | YES (defer recover) | Returns after HTTP health check |

### Removed goroutines (present pre-M1, absent post-M1):

| Pre-M1 Goroutine | Removed Because |
|-----------------|-----------------|
| Reranker checkAndRestart | No reranker process |
| Hindsight checkAndRestart | No hindsight process |
| allHealthy reranker check | No reranker to check |
| allHealthy hindsight check (replaced by cognee) | Was checking HTTP health of Hindsight API |
| worker pool retain workers (x RetainWorkers) | workers.go deleted |
| worker pool reflect workers (x ReflectWorkers) | workers.go deleted |
| sessionCleaner via workers.start() | Now started directly in server.go Start() |

---

## 7. Migration Checklist — Config Field Removal

Every field removal traceable to `config.go`. Fields removed from Config struct:

| # | Field | Type | Default | Env Var | Used By (pre-M1) |
|---|-------|------|---------|---------|-----------------|
| 1 | `LlamaRerankerPort` | string | `"19091"` | `LLAMA_RERANKER_PORT` | services.go: startLlamaReranker, allHealthy, monitor |
| 2 | `CloudRerankerAPIKey` | string | `""` | `CLOUD_RERANKER_API_KEY` | services.go: startHindsight env injection |
| 3 | `CloudRerankerURL` | string | `""` | `CLOUD_RERANKER_URL` | config.go: IsCloudReranker, Validate |
| 4 | `CloudRerankerModel` | string | `""` | `CLOUD_RERANKER_MODEL` | config.go: Validate |
| 5 | `HindsightPath` | string | `"hindsight-api"` | `HINDSIGHT_PATH` | services.go: startHindsight binary lookup |
| 6 | `HindsightPort` | string | `"8888"` | `HINDSIGHT_PORT` | server.go: BackendConfig, services.go: healthURL, monitor, allHealthy |
| 7 | `LLMProvider` | string | `"openrouter"` | `HINDSIGHT_LLM_PROVIDER` | services.go: startHindsight env injection |
| 8 | `LLMModel` | string | — | `HINDSIGHT_LLM_MODEL` | services.go: startHindsight env injection |
| 9 | `LLMAPIKey` | string | — | `OPENROUTER_API_KEY` | services.go: startHindsight env injection |
| 10 | `LLMBaseURL` | string | — | `OPENROUTER_BASE_URL` | services.go: startHindsight env injection |
| 11 | `EmbedProvider` | string | — | `HINDSIGHT_EMBEDDINGS_PROVIDER` | services.go: startHindsight env injection |
| 12 | `EmbedModel` | string | — | `HINDSIGHT_EMBEDDINGS_MODEL` | services.go: startHindsight env injection |
| 13 | `RerankerProvider` | string | — | `HINDSIGHT_RERANKER_PROVIDER` | services.go: startHindsight env injection |
| 14 | `RerankerModel` | string | `./model/bge-reranker-base-Q4_k_m.gguf` | `HINDSIGHT_RERANKER_MODEL` | config.go: IsCloudReranker, Validate; services.go: startLlamaReranker, startHindsight |
| 15 | `HindsightRetainTimeout` | time.Duration | 60s | `HINDSIGHT_RETAIN_TIMEOUT` | services.go: startHindsight env injection |
| 16 | `HindsightRecallTimeout` | time.Duration | 10s | `HINDSIGHT_RECALL_TIMEOUT` | services.go: startHindsight env injection |
| 17 | `HindsightReflectTimeout` | time.Duration | 60s | `HINDSIGHT_REFLECT_TIMEOUT` | services.go: startHindsight env injection |
| 18 | `RetainWorkers` | int | 4 | `MEMORY_RETAIN_WORKERS` | workers.go: Pool size |
| 19 | `ReflectWorkers` | int | 2 | `MEMORY_REFLECT_WORKERS` | workers.go: Pool size |
| 20 | `JobBufferSize` | int | 100 | `MEMORY_JOB_BUFFER` | workers.go: chan buffer |
| 21 | `QueuePushTimeout` | time.Duration | 5s | `MEMORY_QUEUE_PUSH_TIMEOUT` | workers.go: queueJob |
| 22 | `QueueResponseTimeout` | time.Duration | 60s | `MEMORY_QUEUE_RESPONSE_TIMEOUT` | workers.go: queueJob |
| 23 | `CircuitBreakerThreshold` | int | 5 | `MEMORY_CIRCUIT_BREAKER_THRESHOLD` | backend/hindsight.go |
| 24 | `CircuitBreakerCooldown` | time.Duration | 10s | `MEMORY_CIRCUIT_BREAKER_COOLDOWN` | backend/hindsight.go |

**Total: 24 Config fields removed.**

---

## 8. Migration Checklist — Server Struct Field Removal

| # | Field | Type | Pre-M1 Purpose |
|---|-------|------|---------------|
| 1 | `workers *workerSystem` | pointer | Hindsight worker pool. Deleted. |

---

## 9. Migration Checklist — services Struct Field Removal

| # | Field | Type | Pre-M1 Purpose |
|---|-------|------|---------------|
| 1 | `llamaRerankerCmd *exec.Cmd` | pointer | Reranker subprocess. Deleted. |
| 2 | `hindsightCmd *exec.Cmd` | pointer | Hindsight subprocess. Deleted. |
| 3 | `backendName string` | string | Backend dispatch. Deleted. |
| 4 | `rerankerFails serviceFails` | struct | Reranker fail tracking. Deleted. |
| 5 | `hindsightFails serviceFails` | struct | Hindsight fail tracking. Deleted. |

---

## 10. Acceptance Criteria (M1 Only)

All ACs are testable individually. No ACs from M2-M5 are included.

| AC# | Description | Verification Method |
|-----|-------------|-------------------|
| AC-M1.1 | `backend/hindsight.go` file is deleted | `ls backend/hindsight.go` returns "No such file" |
| AC-M1.2 | `workers.go` file is deleted | `ls workers.go` returns "No such file" |
| AC-M1.3 | `model/bge-reranker-base-Q4_k_m.gguf` file is deleted | `ls model/bge-reranker-base-Q4_k_m.gguf` returns "No such file" |
| AC-M1.4 | `docs/hindsight.md` file is deleted | `ls docs/hindsight.md` returns "No such file" |
| AC-M1.5 | `session_cleaner.go` file exists with `sessionCleaner()` method on `*Server` | `ls session_cleaner.go` succeeds; grep confirms method signature |
| AC-M1.6 | `go build ./...` compiles with zero errors | `go build ./...` exit code 0 |
| AC-M1.7 | `go vet ./...` passes with zero warnings | `go vet ./...` exit code 0 |
| AC-M1.8 | `grep -r "IsSync" --include="*.go" ./` returns zero results | Zero matches in non-test, non-vendor Go files |
| AC-M1.9 | `grep -r "BackendHindsight" --include="*.go" ./` returns zero results | Zero matches |
| AC-M1.10 | `grep -r "hindsight\|Hindsight\|HINDSIGHT" --include="*.go" ./` returns zero results (in non-test files) | Zero matches in main + backend packages |
| AC-M1.11 | `grep -r "reranker\|Reranker\|RERANKER" --include="*.go" ./` returns zero results (in non-test files) | Zero matches in main + backend packages |
| AC-M1.12 | `grep "HindsightPort\|RerankerModel\|CloudReranker" config.go` returns zero results | Zero matches |
| AC-M1.13 | `services.go` `healthCache` is `[2]bool`, not `[3]bool` | grep confirms |
| AC-M1.14 | `allHealthy()` returns 2 values `(llama, cognee bool)` | grep confirms signature |
| AC-M1.15 | `allHealthy()` has ZERO references to `IsCloudReranker`, `BackendHindsight`, `LlamaRerankerPort`, `HindsightPort` | grep on services.go confirms |
| AC-M1.16 | `services.go` has no `startLlamaReranker`, `startHindsight` functions | grep confirms |
| AC-M1.17 | `services.go` `start()`, `stop()`, `monitor()` have no `case BackendHindsight:` | grep confirms |
| AC-M1.18 | `handlers.go` `toolsList()` unconditionally returns 5 tools: retain, recall, reflect, forget, retain_status | Code review + curl to `/mcp/tools` (if server compiles and runs) |
| AC-M1.19 | `handlers.go` has zero `IsSync()` calls | grep confirms |
| AC-M1.20 | `handlers.go` has zero `s.workers.` references | grep confirms |
| AC-M1.21 | `server.go` Cognee infrastructure (semaphore, jobTracker, ctx, dataDir, improveState) is constructed unconditionally — no `if !IsSync()` guard | Code review |
| AC-M1.22 | `server.go` has no `workers *workerSystem` field | grep confirms |
| AC-M1.23 | `server.go` `backend.BackendConfig` literal has no `HindsightPort`, `CircuitBreakerThreshold`, `CircuitBreakerCooldown` fields | grep confirms |
| AC-M1.24 | `server.go` `Start()` explicitly starts `go s.sessionCleaner()` | Code review |
| AC-M1.25 | `server.go` `Stop()` does not call `s.workers.stop()` | grep confirms |
| AC-M1.26 | `errors.go` has no `errBinaryNotFound` | grep confirms |
| AC-M1.27 | `pids.go` has no `hindsight` or `llama_reranker` in `savePids()` | grep confirms |
| AC-M1.28 | `main.go` Phase 1 comment mentions Cognee, not Hindsight | grep confirms |
| AC-M1.29 | `.env.example` has no HINDSIGHT_* entries, no CLOUD_RERANKER_* entries | grep confirms |
| AC-M1.30 | `types.go` has no `MemoryJob` or `MemoryResult` types | grep confirms |
| AC-M1.31 | `config.go` `Validate()` default error lists only `cognee-python`, `cognee-rust` | Code review |
| AC-M1.32 | `config.go` `LoadConfig()` Backend default is `"cognee-python"` | Code review |
| AC-M1.33 | Health endpoint (`GET /health`) returns `llama`, `cognee` fields (not `hindsight`, `reranker`) | curl /health JSON inspection |
| AC-M1.34 | Health endpoint has no `retain_workers`, `reflect_workers`, `retain_panics`, `reflect_panics` fields | curl /health JSON inspection |
| AC-M1.35 | `backend.Backend` interface has NO `IsSync()` method | grep on backend/backend.go |
| AC-M1.36 | `var _ backend.Backend = (*CogneeBackend)(nil)` compiles | `go build ./backend/...` passes |
| AC-M1.37 | All test files compile without errors | `go build ./...` includes test files? No — build doesn't compile tests. But `go vet ./...` does check test files. |
| AC-M1.38 | `CGO_ENABLED=0 go build ./...` succeeds | Pure Go — no CGO dependency introduced |
| AC-M1.39 | `config.go` has exactly 24 fewer fields (count pre vs post) | Diff count of struct fields |
| AC-M1.40 | Cognee retain goroutine path is the ONLY path in `memory_retain` — no if/else branching on backend type | Code review |

---

## 11. Edge Cases & Risk Mitigation

### 11.1 Compile-time Risks

| Risk | Likelihood | Mitigation |
|------|-----------|-----------|
| Test files reference deleted `HindsightPort` config field | High | Must update test configs (see §5.1). Tests only need to compile, not pass. |
| Test mock has `IsSync()` method | Medium | `auto_improve_test.go:159` — delete IsSync from mockBackend |
| `deep_test.go` checks health fields by name | Medium | Remove `"hindsight"` and `"reranker"` from required field list |
| Unused import after deleting Hindsight code | Low | Go compiler catches this. Fix imports. |

### 11.2 Runtime Risks

| Risk | Likelihood | Mitigation |
|------|-----------|-----------|
| `s.cogneeCancel` nil dereference in Stop() | Low | After unconditional construction, always non-nil. The `if s.cogneeCancel != nil` guard is kept. |
| SessionCleaner not started | Medium | Explicitly start `go s.sessionCleaner()` in Start(). AC-M1.24 verifies. |
| Health endpoint breaks clients expecting old field names | Medium | This is a breaking change. Document in M5. For M1, just ensure new shape is valid JSON. |
| `allHealthy()` returns wrong size array to singleflight | Low | Type assertion `val.([2]bool)` will panic if wrong. Compile-time check ensures consistency. |
| Metrics `queueGauge` set to 0 loses observability | Low | Documented as known limitation until M3. `queue_depth` in health response is 0. |

### 11.3 Data Risks

| Risk | Likelihood | Mitigation |
|------|-----------|-----------|
| Reranker model file deletion breaks nothing | None | Verify only used by Hindsight's llama reranker. Delete is safe. |
| Hindsight binary still on disk | None | Not deleted — only code references removed. Binary cleanup is manual. |

---

## 12. Lock Ordering (Post-M1)

No changes to lock ordering. M1 is purely deletion. Lock ordering pre-M1 and post-M1 is identical:

1. `s.mu` (Server state R/W lock) — outermost, acquired in Start/Stop
2. `s.sessionsMu` — protects sessions map
3. `svc.mu` — protects cmd pointers in services
4. `svc.healthMu` — protects healthCache

No ABBA risk because goroutines acquire at most one lock at a time.

---

## 13. Verification After M1 (Orchestrator Checklist)

After the Coder completes M1, the Orchestrator MUST verify:

1. `git diff --stat` shows the expected files deleted and modified
2. `go build ./...` exits 0
3. `go vet ./...` exits 0
4. `grep -r "IsSync" --include="*.go" ./` returns 0 results
5. `grep -r "hindsight\|Hindsight" --include="*.go" ./` returns 0 results in non-test files
6. `grep -r "reranker\|Reranker" --include="*.go" ./` returns 0 results in non-test files
7. `ls backend/hindsight.go workers.go model/bge-reranker-base-Q4_k_m.gguf docs/hindsight.md` all fail with "No such file"
8. `ls session_cleaner.go` succeeds
9. Health endpoint JSON shape matches §4.6

---

## 14. Handoff Notes for Coder

1. **Order of operations**: Delete files first (backend/hindsight.go, workers.go, model file, docs), then modify remaining files. This ensures you find all compilation errors early.
2. **allHealthy() singleflight**: The `val.([2]bool)` type assertion must match the `return [2]bool{l, c}, nil` inside the singleflight function. Mismatch causes runtime panic.
3. **Session cleaner extraction**: Copy the method body verbatim from workers.go. Only change the `s.workers` queue depth block to `// TODO(M3)` + `s.metrics.queueGauge.Set(0)`.
4. **Import cleanup**: After deleting Hindsight code, run `goimports` or manually remove unused imports. `context` may become unused in handlers.go if only used by deleted code — check carefully.
5. **Test compilation**: Run `go vet ./...` to catch test file compilation errors. Tests do NOT need to pass — only compile.
6. **Zero new features**: This module is pure deletion. No new functions, no new types, no SQLite, no queue. If you find yourself writing new logic, you're doing it wrong.
