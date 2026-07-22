# Development Guide

## Project Structure

```
mcp/memory/
+-- main.go            Entry point, signal handling, .env loading
+-- server.go          Server struct, Start/Stop lifecycle
+-- config.go          Config, LoadConfig, Validate, env helpers
+-- types.go           MCPSession, MemoryJob, ServiceState
+-- handlers.go        HTTP handlers (SSE, MCP, health)
+-- workers.go         Worker pools (retain, reflect)
+-- hindsight.go       Hindsight API (retain, recall, reflect) + circuit breaker
+-- services.go        llama.cpp + Hindsight lifecycle, health monitor, singleflight
+-- pids.go            Orphan process recovery
+-- mcp.go             MCP protocol helpers (SSE write)
+-- errors.go          Error constructors
+-- alerts.go          Alert client (webhook notifications)
+-- deep_test.go       Deep integration tests
+-- e2e_test.go        End-to-end tests
+-- tester_pass1_venv_test.go        Vendor .venv discovery tests
+-- tester_pass1_download_test.go    llama-server download tests
+-- tester_pass2_boundary_test.go    Boundary tests
+-- tester_pass2_venv_boundary_test.go       Venv boundary tests
+-- tester_pass2_download_boundary_test.go   Download boundary tests
+-- tester_cloud_adversarial_test.go  Cloud adversarial tests
+-- tester_adversarial_test.go       Adversarial tests
+-- Makefile           Build, test, setup, download targets
+-- scripts/           start.sh, stop.sh convenience scripts
+-- worker/            Worker pool package
+-- logger/            Structured logging
+-- metrics/           Counters, timers, gauges
+-- logs/              Runtime logs (gitignored)
+-- .env.example       Config template
+-- .gitignore         Git ignore rules
+-- docs/              Documentation
```

## Adding a New Feature

1. **New config option:** Add to `config.go` struct + `LoadConfig()`. Add validation in `Validate()`. Document in `.env.example`.
2. **New HTTP endpoint:** Add handler in `handlers.go`, register in `main.go` mux.
3. **New Hindsight operation:** Add method to `hindsight.go`. Include circuit breaker check and configurable timeout.
4. **New worker:** Add channel + pool in `workers.go` `start()`. Add context cancellation support.
5. Always add corresponding tests.

## Quick Reference

```bash
make setup           # Create .venv, install Hindsight, download llama-server + models
make run             # Start server (auto-starts llama.cpp + Hindsight + MCP)
make build           # Build binary to bin/mcp-memory
make test            # Run all tests with race detector (-race -timeout 240s)
make vet             # Run go vet static analysis
make stop            # Graceful shutdown
make clean           # Remove .venv, build artifacts, and bin/llama/
make download-llama  # Download platform-specific llama-server binary
make download-models # Download GGUF model files from Hugging Face
```

## Testing

```bash
# All tests (race detector + 240s timeout) — primary
make test
# Expands to: go test -race -count=1 -timeout 240s ./...

# Specific package
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
- **Logging:** Use `s.log.Info/Warn/Error` with structured attrs. Never `fmt.Println`.
- **Metrics:** `metrics.NewCounter/Timer` in handler/worker code. Auto-registered globally.
- **Concurrency:** `sync.RWMutex` for shared maps. Channels for worker dispatch.
- **Configuration:** All via env vars. Never hardcode operational values.
- **Worker pools:** Use `worker.NewPool()` for goroutine management. Panic-safe.
- **Circuit breaker:** Check `s.hindsightBreaker.IsTripped()` before Hindsight API calls.
- **Content validation:** Validate content size before queuing to workers.
- **Context cancellation:** Pass context through to Hindsight API calls for clean shutdown.

## Debugging

```bash
# Live metrics
curl http://localhost:8899/health | jq '.metrics'

# Structured logs
tail -f logs/memory.log | jq '.'

# Hindsight errors
tail -f logs/hindsight-crash.log

# Orphaned processes
cat logs/.mcp-pids.json

# Port conflicts
lsof -ti :8080 :8081 :8888 :8899

# Circuit breaker state
curl http://localhost:8899/health | jq '.hindsight'
# false = circuit breaker may be open, check logs for "circuit breaker open"
```

## Key Implementation Details

### Circuit Breaker Flow
```
Hindsight API call
  -> IsTripped()? 
     -> Yes: return "circuit breaker open" immediately
     -> No: proceed with request
        -> Success: RecordSuccess() (resets failure count)
        -> Failure: RecordFailure() (increments count, trips if >= threshold)
```

### Singleflight Health Checks
```
allHealthy() called by N concurrent goroutines
  -> Check 10s cache first (fast path)
  -> Cache expired: singleflight.Do("health", ...)
     -> Only 1 goroutine performs 3 HTTP checks
     -> Others wait for result
  -> Update cache
```

### Worker Context Cancellation
```
worker.Start():
  for {
    select {
    case job := <-jobs:
      process(job)
    case <-ctx.Done():
      return  // clean exit on shutdown
    }
  }
```

### Cloud Embedding Detection
```
Validate():
  if c.IsCloudEmbedding() {
    // Requires 3 env vars: CLOUD_EMBEDDING_API_KEY, CLOUD_EMBEDDING_URL, CLOUD_EMBEDDING_MODEL
  }
  if c.IsCloudReranker() {
    // Requires 3 env vars: CLOUD_RERANKER_API_KEY, CLOUD_RERANKER_URL, CLOUD_RERANKER_MODEL
  }
  for _, path := range []string{c.ModelPath, c.RerankerModel} {
    if len(path) > 7 && (path[:7] == "http://" || path[:8] == "https://") {
      // Cloud mode: skip local file existence check, validate cloud vars instead
      continue
    }
    // check file exists
  }
```
