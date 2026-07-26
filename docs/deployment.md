# Deployment

## Prerequisites

- **Go 1.26+** (build)
- **OpenRouter API key** (https://openrouter.ai/keys)
- **llama-server** — downloaded automatically by `make setup` to `bin/llama/` (brew is a fallback). Skipped if using cloud endpoints.
- **Model file** — downloaded automatically by `make setup` to `./model/`:
  - `qwen3-embedding-0.6b-Q8_0.gguf` (~610MB) — or use cloud endpoint

## Setup

```bash
cd mcp/memory
cp .env.example .env
# Edit .env: set OPENROUTER_API_KEY
make setup              # Downloads llama-server + models
```

## Run

```bash
# Primary: one-command development run
make run

# Production build
make build
./bin/mcp-memory

# Convenience script (secondary)
./scripts/start.sh
```

## Stop

```bash
# Graceful (preferred)
make stop

# Or via script
./scripts/stop.sh

# Or via API
curl -X POST http://localhost:8899/stop
```

## Verify

```bash
# Health
curl http://localhost:8899/health

# Should show: {"status":"running","llama":true,"cognee":true}

# Queue debug
curl http://localhost:8899/debug/queue
```

## Environment

All configuration via `.env` or environment variables. See `.env.example` for all options.

### Server

| Variable | Default | Description |
|----------|---------|-------------|
| `MCP_PORT` | `8899` | Server port |
| `MCP_HOST` | `0.0.0.0` | Bind address |
| `MCP_AUTH_TOKEN` | (empty) | Auth token for endpoints |

### llama.cpp

| Variable | Default | Description |
|----------|---------|-------------|
| `LLAMA_PORT` | `8080` | Embedding server port |
| `LLAMA_PATH` | `./bin/llama/llama-server` | llama-server binary path (brew on PATH is fallback) |
| `LLAMA_MODEL_PATH` | `./model/qwen3-embedding-0.6b-Q8_0.gguf` | Embedding model (or HTTP URL for cloud) |
| `LLAMA_GPU_LAYERS` | `999` | GPU layers (0=CPU only) |
| `LLAMA_CTX_SIZE` | `8192` | Context size |

### Cognee

| Variable | Default | Description |
|----------|---------|-------------|
| `COGNEE_PORT` | `8000` | Cognee API port |
| `COGNEE_DATA_DIR` | `./cognee-data` | Cognee data directory |
| `COGNEE_BINARY` | (empty) | Cognee Rust binary path (cognee-rust backend) |
| `COGNEE_PYTHON_PATH` | (empty) | Cognee Python venv path (cognee-python backend) |
| `COGNEE_LLM_API_KEY` | (OPENROUTER_API_KEY) | LLM API key for Cognee |
| `COGNEE_LLM_MODEL` | `deepseek/deepseek-v4-flash` | LLM model for Cognee |
| `COGNEE_LLM_ENDPOINT` | `https://openrouter.ai/api/v1` | LLM endpoint |
| `COGNEE_EMBEDDING_ENDPOINT` | `http://localhost:8080/v1` | Embedding endpoint |
| `COGNEE_EMBEDDING_PROVIDER` | `openai` | Embedding provider |
| `COGNEE_MAX_CONCURRENT_RETAINS` | `10` | Max concurrent retain operations |
| `COGNEE_RETAIN_TIMEOUT` | `900s` | Timeout for retain operations |
| `COGNEE_TEMPORAL_COGNIFY` | `true` | Enable temporal cognify |
| `COGNEE_MEMORY_ONLY` | `true` | Memory-only mode |

### Queue

| Variable | Default | Description |
|----------|---------|-------------|
| `QUEUE_DB_PATH` | `./data/queue.db` | SQLite database path |
| `QUEUE_MAX_PENDING` | `1000` | Max pending jobs before rejection |
| `QUEUE_WORKERS` | `4` | Worker goroutine count |
| `QUEUE_MAX_CONCURRENT` | `3` | Max concurrent in-flight calls |
| `QUEUE_JOB_TTL` | `24h` | Retention for completed/failed/dead jobs |
| `QUEUE_TTL_INTERVAL` | `5m` | TTL cleanup frequency |

### Auto-Improve

| Variable | Default | Description |
|----------|---------|-------------|
| `AUTO_IMPROVE_AFTER_N` | `0` | Retains before triggering (0=disabled) |
| `AUTO_IMPROVE_COOLDOWN` | `120s` | Min time between triggers |

### Auto-Reflect

| Variable | Default | Description |
|----------|---------|-------------|
| `AUTO_REFLECT_AFTER_N` | `10` | Retains before triggering (0=disabled) |
| `AUTO_REFLECT_TIMEOUT` | `6h` | Max time since last reflect (0=disabled) |

### Error Webhook

| Variable | Default | Description |
|----------|---------|-------------|
| `ERROR_WEBHOOK_URL` | (empty) | POSTed on dead-letter events |

### Sessions

| Variable | Default | Description |
|----------|---------|-------------|
| `MCP_MAX_SESSIONS` | `100` | Max concurrent SSE sessions |
| `MCP_SSE_BUFFER` | `100` | SSE channel buffer per session |
| `MCP_SESSION_IDLE` | `30m` | Idle session cleanup timeout |
| `MCP_SESSION_CLEAN_INTERVAL` | `30s` | Session cleaner frequency |

### Health Monitor

| Variable | Default | Description |
|----------|---------|-------------|
| `HEALTH_CHECK_INTERVAL` | `5s` | Health poll frequency |
| `HEALTH_CHECK_TIMEOUT` | `60s` | Health check timeout |
| `HEALTH_CONSECUTIVE_FAILURES` | `2` | Failures before restart |

### Retry & Backoff

| Variable | Default | Description |
|----------|---------|-------------|
| `MCP_RETRY_ATTEMPTS` | `3` | Max retry attempts per request |
| `MCP_RETRY_DELAY` | `1s` | Base retry delay |
| `MCP_RETRY_MAX_DELAY` | `30s` | Max retry delay (exponential backoff cap) |

### Content & HTTP

| Variable | Default | Description |
|----------|---------|-------------|
| `MAX_CONTENT_BYTES` | `1048576` | Max content size (1MB) |
| `HTTP_MAX_BODY_BYTES` | `1048576` | Max HTTP body size |
| `HTTP_READ_TIMEOUT` | `10s` | HTTP read timeout |
| `HTTP_IDLE_TIMEOUT` | `120s` | HTTP idle timeout |

### Service Timeouts

| Variable | Default | Description |
|----------|---------|-------------|
| `SERVICE_START_TIMEOUT` | `120s` | Max time for services to become healthy |
| `SERVICE_STOP_TIMEOUT` | `5s` | Max time for graceful service stop |
| `SHUTDOWN_TIMEOUT` | `10s` | Max time for HTTP server shutdown |
| `BACKEND_RETAIN_TIMEOUT` | `60s` | Timeout for retain API calls |
| `BACKEND_RECALL_TIMEOUT` | `10s` | Timeout for recall API calls |
| `BACKEND_REFLECT_TIMEOUT` | `60s` | Timeout for reflect API calls |

### Alerts

| Variable | Default | Description |
|----------|---------|-------------|
| `ALERT_URL` | (empty) | Webhook URL for alerts |
| `ALERT_MODE` | `optional` | `optional` or `required` |

## Cloud Embedding

To use a cloud embedding service instead of local llama.cpp:

```bash
LLAMA_MODEL_PATH=https://api.openai.com/v1
CLOUD_EMBEDDING_API_KEY=sk-...
CLOUD_EMBEDDING_URL=https://api.openai.com/v1
CLOUD_EMBEDDING_MODEL=text-embedding-3-small
```

When `LLAMA_MODEL_PATH` is an HTTP/HTTPS URL, the server skips local llama.cpp process management.

## Production Notes

- **Ports:** 8080, 8899 must be free (unless using cloud endpoints). Server checks on startup.
- **RAM:** ~650MB minimum with local models. Cloud endpoints reduce to ~50MB.
- **Logs:** `logs/memory.log` -- JSON structured, 10MB rotation, 3 backups.
- **Crash recovery:** `logs/.mcp-pids.json` -- cleans orphaned processes on restart.
- **Queue DB:** `data/queue.db` -- SQLite WAL mode, auto-created on first run.
- **Security:** Bind to `127.0.0.1` for local-only access. No authentication (local use only).

## Troubleshooting

| Problem | Fix |
|---------|-----|
| "model not found" | Check `LLAMA_MODEL_PATH` points to a valid `.gguf` file or HTTP URL |
| Port already in use | `./scripts/stop.sh` to kill all services, then retry |
| High latency | Check OpenRouter status, verify Cognee health |
| Content too large | Increase `MAX_CONTENT_BYTES` or reduce input size |
| Session limit reached | Increase `MCP_MAX_SESSIONS` or check for session leaks |
| Jobs stuck pending | Check worker logs, verify Cognee is healthy, check `QUEUE_WORKERS` count |
| Queue full | Increase `QUEUE_MAX_PENDING` or drain stuck jobs |
| Dead letter events | Check `ERROR_WEBHOOK_URL` is set, check webhook endpoint health |
