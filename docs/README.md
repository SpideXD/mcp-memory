# MCP Memory Server

> Originally extracted from `cagents/mcp/memory`.
> This is now the canonical source for the MCP Memory Server.

Standalone Go MCP server that proxies memory operations to Hindsight. Supports N concurrent agents with bank-level isolation.

## Quick Start

```bash
cp .env.example .env    # Edit with your OpenRouter key
go run .                # Starts llama.cpp + Hindsight + MCP server
```

**Prerequisites:** llama-server (brew), hindsight-api (pip), OpenRouter API key.

## Architecture

```
pi.go agent -> SSE -> mcp-memory -> HTTP -> Hindsight API -> pg0

Embedding: llama.cpp (qwen3-embedding-0.6b, q4_0 cache) or cloud endpoint
Reranking: llama.cpp (bge-reranker-base, q8_0 cache) or cloud endpoint
LLM:       DeepSeek V4 Flash via OpenRouter
```

Three independent services managed as child processes with health watchdog and auto-restart. Cloud embedding/reranker endpoints (HTTP/HTTPS URLs) skip local process management.

## Endpoints

| Endpoint | Method | Purpose |
|----------|--------|---------|
| `/mcp/sse?bank=X` | GET | Connect agent, bank in URL |
| `/mcp/message` | POST | MCP JSON-RPC (tools/list, tools/call) |
| `/health` | GET | Service health + metrics |
| `/start` | POST | Start services |
| `/stop` | POST | Graceful shutdown |

## Concurrency Model

- **Bank from URL:** `/mcp/sse?bank=outreach:spidex_owner` -- bank is immutable after session creation. Format: `profile:user_id` (e.g., `outreach:spidex_owner`, `email:client_42`). Max 128 chars, allowed chars: `a-zA-Z0-9:_-`.
- **Per-session state:** No global `currentUser`. Each SSE connection has its own bank.
- **Fast path:** `memory_recall` calls Hindsight directly (concurrent reads, ~300ms)
- **Slow path:** `memory_retain/reflect` queued through dedicated worker pools (avoids concurrent LLM calls)
- **Worker pools:** 2 retain workers + 2 reflect workers (configurable). Uses `worker.Pool` with panic recovery.
- **Session limit TOCTOU fix:** Atomic check+insert under `sessionsMu.Lock()` prevents race conditions.
- **Worker context cancellation:** Workers respect context cancellation for clean shutdown.

## Bug Fixes (14 total)

| Priority | Fix | Description |
|----------|-----|-------------|
| P0 | Circuit breaker | Hindsight API failures trip breaker (threshold=5, cooldown=30s) to fail fast |
| P0 | Exponential backoff | `delay * 2^attempt`, capped at `MCP_RETRY_MAX_DELAY` (30s default) |
| P0 | Singleflight health | Concurrent health checks deduplicated via `singleflight.Group` |
| P1 | Configurable timeouts | All Hindsight API calls have configurable timeouts via env vars |
| P1 | Session limit TOCTOU | Atomic check+insert under mutex prevents session limit race |
| P1 | Worker context cancellation | Workers respect context.Done() for clean shutdown |
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
| llama.cpp | `LLAMA_PORT=8080`, `LLAMA_MODEL_PATH=...`, `LLAMA_GPU_LAYERS=999` |
| Hindsight | `HINDSIGHT_PORT=8888`, `HINDSIGHT_LLM_PROVIDER=openrouter` |
| Workers | `MEMORY_RETAIN_WORKERS=2`, `MEMORY_REFLECT_WORKERS=2` |
| Sessions | `MCP_MAX_SESSIONS=100`, `MCP_SESSION_IDLE=30m` |
| Health | `HEALTH_CHECK_INTERVAL=5s`, `HEALTH_CONSECUTIVE_FAILURES=2` |
| Hindsight Timeouts | `HINDSIGHT_RETAIN_TIMEOUT=60s`, `HINDSIGHT_RECALL_TIMEOUT=10s`, `HINDSIGHT_REFLECT_TIMEOUT=60s` |
| Circuit Breaker | `HINDSIGHT_CIRCUIT_BREAKER_THRESHOLD=5`, `HINDSIGHT_CIRCUIT_BREAKER_COOLDOWN=30s` |
| Content | `MAX_CONTENT_BYTES=1048576` |
| Retry | `MCP_RETRY_MAX_DELAY=30s` |

## Deployment

```bash
# Start
cd mcp/memory && go run .

# Stop (graceful)
./stop.sh

# Build binary
go build -o mcp-memory .
./mcp-memory
```

## File Structure

```
mcp/memory/
+-- main.go            Entry point, signal handling
+-- server.go          Server lifecycle, Start/Stop
+-- config.go          Configuration, LoadConfig, Validate
+-- types.go           MCPSession, MemoryJob, ServiceState
+-- handlers.go        HTTP + MCP handlers (SSE, health)
+-- workers.go         Worker pools (retain, reflect)
+-- hindsight.go       Hindsight API client (retain, recall, reflect) + circuit breaker
+-- services.go        llama.cpp + Hindsight lifecycle, health monitor, singleflight
+-- pids.go            Orphan process recovery
+-- mcp.go             MCP protocol helpers (SSE write)
+-- errors.go          Error types
+-- alerts.go          Alert client (webhook notifications)
+-- stop.sh            Graceful shutdown script
+-- worker/            Tested worker pool package
+-- logger/            Structured logging
+-- metrics/           Counters, timers, gauges
+-- logs/              Runtime logs (gitignored)
+-- .env               Secrets (gitignored)
+-- .env.example       Config template
+-- docs/              This folder
```

## Testing

```bash
# Unit tests
go test ./...

# E2E tests (requires running server)
go test -v -run "TestConcurrent|TestStress|TestRace"

# With race detector
go test -race -run "TestStress" -count=1
```

## Health & Observability

```bash
curl http://localhost:8899/health

# Returns:
{
  "status": "running",
  "hindsight": true, "llama": true, "reranker": true,
  "queue_depth": 0, "sessions": 2,
  "metrics": {
    "memory.recall_count": 142, "memory.retain_count": 23,
    "memory.retain_duration_p99": "30s",
    "memory.sse_drops": 0
  },
  "retain_workers": 2, "reflect_workers": 2,
  "retain_panics": 0, "reflect_panics": 0
}
```

Structured JSON logs at `logs/memory.log` with 10MB rotation (3 backups, 7-day retention).

## Reliability

- **Circuit breaker:** Hindsight API failures trip breaker after 5 consecutive failures. Cooldown: 30s. Fails fast to prevent cascading timeouts.
- **Exponential backoff:** Retry with `delay * 2^attempt`, capped at 30s. Retries configurable via `MCP_RETRY_ATTEMPTS` (default: 3).
- **Singleflight health:** Concurrent health check requests deduplicated -- only 1 HTTP request per 10s cache window.
- **Orphan recovery:** PID file survives crashes. Next startup kills orphans.
- **Health watchdog:** Auto-restarts llama/Hindsight after 2 consecutive failures. Max 5 restarts per hour per service.
- **Graceful shutdown:** HTTP -> workers -> sessions -> services (SIGTERM, 5s timeout, SIGKILL).
- **Worker panic recovery:** Panic returns error to caller, worker restarted with 100ms backoff.
- **At-least-once delivery:** Errors returned to agent. Agent retries on timeout.
- **Cloud embedding support:** HTTP/HTTPS model paths skip local process management (use remote embedding/reranker services).
- **Content size validation:** `MAX_CONTENT_BYTES` prevents oversized content from exhausting memory.
