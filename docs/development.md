# Development Guide

## Project Structure

```
mcp/memory/
+-- main.go            Entry point, signal handling, .env loading
+-- server.go          Server struct, Start/Stop lifecycle, processQueueJob
+-- config.go          Config, LoadConfig, Validate, env helpers
+-- types.go           MCPSession, Backend, ServiceState
+-- handlers.go        HTTP handlers (SSE, MCP, health, debug/queue)
+-- auto_reflect.go    Per-bank auto-reflect trigger logic
+-- auto_improve.go    Per-bank graph optimization
+-- services.go        llama.cpp + Cognee lifecycle, health monitor, singleflight
+-- session_cleaner.go Idle session cleanup goroutine
+-- pids.go            Orphan process recovery
+-- mcp.go             MCP protocol helpers (SSE write)
+-- errors.go          Error constructors
+-- alerts.go          Alert client (webhook notifications)
+-- deep_test.go       Deep integration tests
+-- e2e_test.go        End-to-end tests
+-- Makefile           Build, test, setup, download targets
+-- scripts/           start.sh, stop.sh convenience scripts
+-- queue/             SQLite-backed job queue
|   +-- job.go         Job struct, Status enum, Validate
|   +-- store.go       SQLite store with WAL, recovery, TTL cleanup
|   +-- worker.go      Worker pool with semaphore, OnDead callback
+-- worker/            Generic worker pool (panic-safe, restartable)
|   +-- pool.go        Pool struct, Start/Stop, panic recovery
+-- logger/            Structured logging
+-- metrics/           Counters, timers, gauges
+-- logs/              Runtime logs (gitignored)
+-- data/              Runtime data (queue.db, etc.)
+-- .env.example       Config template
+-- .gitignore         Git ignore rules
+-- docs/              Documentation
```

## Adding a New Feature

1. **New config option:** Add to `config.go` struct + `LoadConfig()`. Add validation in `Validate()`. Document in `.env.example`.
2. **New HTTP endpoint:** Add handler in `handlers.go`, register in `main.go` mux.
3. **New queue behavior:** Add to `queue/store.go` (data layer) or `queue/worker.go` (processing). Wire via `server.go` `Start()`.
4. **New auto-trigger:** Add to `auto_reflect.go` or `auto_improve.go` with per-bank state tracking.
5. Always add corresponding tests.

## Quick Reference

```bash
make setup           # Download llama-server + models
make run             # Start server (auto-starts llama.cpp + Cognee + MCP)
make build           # Build binary to bin/mcp-memory
make test            # Run all tests with race detector (-race -timeout 240s)
make vet             # Run go vet static analysis
make stop            # Graceful shutdown
make clean           # Remove build artifacts and bin/llama/
make download-llama  # Download platform-specific llama-server binary
make download-models # Download GGUF model files from Hugging Face
```

## Testing

```bash
# All tests (race detector + 240s timeout) — primary
make test
# Expands to: go test -race -count=1 -timeout 240s ./...

# Specific package
go test ./queue/...
go test ./worker/...

# E2E (requires running server)
go test -v -run "TestStress" -count=1

# Race detector (single test)
go test -race -run "TestConcurrent" -count=1

# Single test
go test -run "TestSingleAgent" -v

# Static analysis
make vet
```

## Conventions

- **Error handling:** Bubble up with `fmt.Errorf("context: %w", err)`. No panic except init.
- **Logging:** Use `s.log.Info/Warn/Error` with structured key-value attrs. Never `fmt.Println`.
- **Metrics:** `metrics.NewCounter/Timer` in handler/worker code. Auto-registered globally.
- **Concurrency:** `sync.RWMutex` for shared maps. Channels for worker dispatch.
- **Configuration:** All via env vars. Never hardcode operational values.
- **Queue:** Use `queue.NewStore()` + `queue.NewWorker()` for job processing. SQLite-backed.
- **Content validation:** Validate content size before queuing to workers.
- **Context cancellation:** Pass context through to backend API calls for clean shutdown.

## Debugging

```bash
# Live metrics
curl http://localhost:8899/health | jq '.metrics'

# Queue state
curl http://localhost:8899/debug/queue

# Structured logs
tail -f logs/memory.log | jq '.'

# Orphaned processes
cat logs/.mcp-pids.json

# Port conflicts
lsof -ti :8080 :8899
```

## Key Implementation Details

### Queue State Machine
```
pending -> running -> completed (success)
                   -> failed (retryable) -> pending (if retries left)
                                        -> dead (exhausted)
```

### Singleflight Health Checks
```
allHealthy() called by N concurrent goroutines
  -> Check 10s cache first (fast path)
  -> Cache expired: singleflight.Do("health", ...)
     -> Only 1 goroutine performs HTTP checks
     -> Others wait for result
  -> Update cache
```

### Worker Context Cancellation
```
worker.Start():
  for {
    select {
    case <-ctx.Done():
      return  // clean exit on shutdown
    default:
      job = store.NextPending()
      sem <- struct{}{} // acquire
      processJob(ctx, job)
      <-sem // release
    }
  }
```

### Cloud Embedding Detection
```
Validate():
  if c.IsCloudEmbedding() {
    // Requires 3 env vars: CLOUD_EMBEDDING_API_KEY, CLOUD_EMBEDDING_URL, CLOUD_EMBEDDING_MODEL
  }
```
