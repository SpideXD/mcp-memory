# MCP Memory Server

> Originally extracted from `cagents/mcp/memory`.
> This is now the canonical source for the MCP Memory Server.

Standalone Go MCP server that proxies memory operations to a Cognee Rust backend. Supports N concurrent agents with bank-level isolation.

## Quick Start

```bash
cp .env.example .env    # Edit with your OpenRouter key
make setup              # Downloads llama-server + models
make run                # Starts server (auto-starts llama.cpp + MCP as child processes)
```

**Prerequisites:** Go 1.26+, OpenRouter API key. `make setup` handles llama-server and model downloads.

To stop: `make stop` or `./scripts/stop.sh`

## Architecture

```
pi.go agent -> SSE -> mcp-memory -> HTTP -> Cognee Rust backend

Embedding: llama.cpp (qwen3-embedding-0.6b, q4_0 cache) or cloud endpoint
LLM:       DeepSeek V4 Flash via OpenRouter
```

Two independent services managed as child processes with health watchdog and auto-restart. Cloud embedding endpoints (HTTP/HTTPS URLs) skip local process management, using the `CLOUD_EMBEDDING_*` env vars (see Configuration below).

Async retain/reflect operations are dispatched through a SQLite-backed job queue (`queue/` package). Auto-reflect is scheduled automatically after N retains or after a configurable timeout.

## Endpoints

| Endpoint | Method | Purpose |
|----------|--------|---------|
| `/mcp/sse?bank=profile:user_id` | GET | Connect agent, bank in URL |
| `/mcp/message` | POST | MCP JSON-RPC (tools/list, tools/call) |
| `/health` | GET | Service health + metrics |
| `/start` | POST | Start services |
| `/stop` | POST | Graceful shutdown |

## Concurrency Model

- **Bank from URL:** `/mcp/sse?bank=outreach:spidex_owner` -- bank is immutable after session creation. Format: `profile:user_id` (e.g., `outreach:spidex_owner`, `email:client_42`). Max 128 chars, allowed chars: `a-zA-Z0-9:_-`.
- **Per-session state:** No global `currentUser`. Each SSE connection has its own bank.
- **Fast path:** `memory_recall` calls Cognee directly (concurrent reads, ~300ms)
- **Slow path:** `memory_retain/reflect` queued through SQLite-backed job queue (`queue/` package), processed by async workers
- **Session limit TOCTOU fix:** Atomic check+insert under `sessionsMu.Lock()` prevents race conditions.
- **Worker context cancellation:** Workers respect context cancellation for clean shutdown.

## Bug Fixes (14 total)

| Priority | Fix | Description |
|----------|-----|-------------|
| P0 | Circuit breaker | Cognee API failures trip breaker (threshold=5, cooldown=30s) to fail fast |
| P0 | Exponential backoff | `delay * 2^attempt`, capped at `MCP_RETRY_MAX_DELAY` (30s default) |
| P0 | Singleflight health | Concurrent health checks deduplicated via `singleflight.Group` |
| P1 | Configurable timeouts | All Cognee API calls have configurable timeouts via env vars |
| P1 | Session limit TOCTOU | Atomic check+insert under mutex prevents session limit race |
| P1 | Worker context cancellation | Queue workers respect context.Done() for clean shutdown |
| P1 | SSE drop tracking | `memory.sse_drops` counter tracks dropped SSE writes |
| P2 | Content size validation | `MAX_CONTENT_BYTES` limits input content size (1MB default) |
| P2 | Health cache TTL | Health status cached for 10s, refreshed via singleflight on expiry |
| P2 | Cloud embedding support | HTTP/HTTPS model paths skip local process management |
| P2 | Service process exit detection | Monitor detects `cmd.ProcessState.Exited()` independently of HTTP health |
| P2 | Max restarts per hour | 5 restarts per service per hour, then stop trying |
| P2 | Recovery detection | Service back online after crash -> logged + alert |
| P2 | Alert client | Webhook notifications for crashes, recoveries, startup |

## Configuration

All via environment variables. See `.env.example` for full reference.

| Key groups | Examples |
|-----------|---------|
| Server | `MCP_PORT=8899`, `MCP_HOST=0.0.0.0` |
| llama.cpp | `LLAMA_PATH=./bin/llama/llama-server`, `LLAMA_PORT=8080`, `LLAMA_MODEL_PATH=...`, `LLAMA_GPU_LAYERS=999` |
| Cognee | `COGNEE_PORT=8888`, `COGNEE_BACKEND_URL=...` |
| Queue | `QUEUE_DB_PATH=./data/queue.db`, `QUEUE_MAX_PENDING=1000` |
| Sessions | `MCP_MAX_SESSIONS=100`, `MCP_SESSION_IDLE=30m` |
| Health | `HEALTH_CHECK_INTERVAL=5s`, `HEALTH_CONSECUTIVE_FAILURES=2` |
| Cognee Timeouts | `COGNEE_RETAIN_TIMEOUT=900s`, `COGNEE_RECALL_TIMEOUT=10s`, `COGNEE_REFLECT_TIMEOUT=60s` |
| Circuit Breaker | `COGNEE_CIRCUIT_BREAKER_THRESHOLD=5`, `COGNEE_CIRCUIT_BREAKER_COOLDOWN=30s` |
| Content | `MAX_CONTENT_BYTES=1048576` |
| Retry | `MCP_RETRY_MAX_DELAY=30s` |
| Cloud Embedding | `CLOUD_EMBEDDING_API_KEY`, `CLOUD_EMBEDDING_URL`, `CLOUD_EMBEDDING_MODEL` |

## Deployment

```bash
# One-command setup & run (primary)
make setup && make run

# Or step by step:
make setup              # Downloads llama-server + models
make run                # Starts server (auto-starts llama.cpp + MCP)

# Stop (graceful)
make stop

# Build binary
make build
./bin/mcp-memory

# Convenience scripts (secondary)
./scripts/start.sh
```

## File Structure

```
mcp/memory/
+-- main.go            Entry point, signal handling
+-- server.go          Server lifecycle, Start/Stop
+-- config.go          Configuration, LoadConfig, Validate
+-- types.go           MCPSession, MemoryJob, ServiceState
+-- handlers.go        HTTP + MCP handlers (SSE, health)
+-- queue/             SQLite-backed job queue (store, worker)
+-- services.go        llama.cpp + Cognee lifecycle, health monitor, singleflight
+-- pids.go            Orphan process recovery
+-- mcp.go             MCP protocol helpers (SSE write)
+-- errors.go          Error types
+-- alerts.go          Alert client (webhook notifications)
+-- Makefile           Build, test, setup, download targets
+-- scripts/           Start and stop convenience scripts
+-- worker/            Tested queue worker package
+-- logger/            Structured logging
+-- metrics/           Counters, timers, gauges
+-- logs/              Runtime logs (gitignored)
+-- bin/               Downloaded llama-server binary
+-- model/             Downloaded GGUF model files
+-- .env               Secrets (gitignored)
+-- .env.example       Config template
+-- .gitignore         Git ignore rules
+-- docs/              This folder
```

## Testing

```bash
# All tests (race detector, 240s timeout) -- primary
make test

# Equivalent to:
go test -race -count=1 -timeout 240s ./...

# E2E tests (requires running server)
go test -v -run "TestConcurrent|TestStress|TestRace"

# Static analysis
make vet
```

## Health & Observability

```bash
curl http://localhost:8899/health

# Returns:
{
  "status": "running",
  "version": "dev",
  "built": "unknown",
  "cognee": true, "llama": true,
  "down": [],
  "queue_depth": 0, "sessions": 2,
  "panics_total": 0,
  "uptime": "5m30s",
  "metrics": {
    "memory.recall_count": 142, "memory.retain_count": 23,
    "memory.retain_duration_p99": "30s",
    "memory.sse_drops": 0
  },
  "queue_depth": 0, "queue_processed": 142
}
```

Structured JSON logs at `logs/memory.log` with 10MB rotation (3 backups, 7-day retention).

## Reliability

- **Circuit breaker:** Cognee API failures trip breaker after 5 consecutive failures. Cooldown: 30s. Fails fast to prevent cascading timeouts.
- **Exponential backoff:** Retry with `delay * 2^attempt`, capped at 30s. Retries configurable via `MCP_RETRY_ATTEMPTS` (default: 3).
- **Singleflight health:** Concurrent health check requests deduplicated -- only 1 HTTP request per 10s cache window.
- **Orphan recovery:** PID file survives crashes. Next startup kills orphans.
- **Health watchdog:** Auto-restarts llama/Cognee after 2 consecutive failures. Max 5 restarts per hour per service.
- **Graceful shutdown:** HTTP -> workers -> sessions -> services (SIGTERM, 5s timeout, SIGKILL).
- **Queue worker panic recovery:** Panic returns error to caller, worker restarted with 100ms backoff.
- **At-least-once delivery:** Errors returned to agent. Agent retries on timeout.
- **Cloud embedding support:** HTTP/HTTPS model paths skip local process management (use remote embedding services).
- **Content size validation:** `MAX_CONTENT_BYTES` prevents oversized content from exhausting memory.
