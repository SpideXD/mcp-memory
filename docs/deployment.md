# Deployment

## Prerequisites

- **Go 1.26+** (build)
- **llama-server** (`brew install llama.cpp`) -- skipped if using cloud endpoints
- **Python 3.12+** with `hindsight-api` (`pip install hindsight-api-slim`)
- **OpenRouter API key** (https://openrouter.ai/keys)
- **Model files** in `../../model/`:
  - `qwen3-embedding-0.6b-Q8_0.gguf` (~610MB) -- or use cloud endpoint
  - `bge-reranker-base-Q4_k_m.gguf` (~209MB) -- or use cloud endpoint

## Setup

```bash
cd mcp/memory
cp .env.example .env
# Edit .env: set OPENROUTER_API_KEY
```

## Run

```bash
# Development
go run .

# Production
go build -o mcp-memory .
./mcp-memory
```

## Stop

```bash
# Graceful (preferred)
./stop.sh

# Or
curl -X POST http://localhost:8899/stop
```

## Verify

```bash
# Health
curl http://localhost:8899/health

# Should show: {"status":"running","hindsight":true,"llama":true}
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
| `LLAMA_RERANKER_PORT` | `8081` | Reranker server port |
| `LLAMA_MODEL_PATH` | `../../model/qwen3-embedding-0.6b-Q8_0.gguf` | Embedding model (or HTTP URL for cloud) |
| `LLAMA_GPU_LAYERS` | `999` | GPU layers (0=CPU only) |
| `LLAMA_CTX_SIZE` | `8192` | Context size |

### Hindsight

| Variable | Default | Description |
|----------|---------|-------------|
| `HINDSIGHT_PORT` | `8888` | Memory API port |
| `OPENROUTER_API_KEY` | (required) | LLM API key |
| `HINDSIGHT_LLM_MODEL` | `deepseek/deepseek-v4-flash` | LLM model |
| `HINDSIGHT_EMBEDDINGS_MODEL` | `qwen3-embedding-0.6b-Q8_0.gguf` | Embedding model name |
| `HINDSIGHT_RERANKER_MODEL` | `../../model/bge-reranker-base-Q4_k_m.gguf` | Reranker model (or HTTP URL for cloud) |

### Hindsight API Timeouts

| Variable | Default | Description |
|----------|---------|-------------|
| `HINDSIGHT_RETAIN_TIMEOUT` | `60s` | Timeout for retain API calls |
| `HINDSIGHT_RECALL_TIMEOUT` | `10s` | Timeout for recall API calls |
| `HINDSIGHT_REFLECT_TIMEOUT` | `60s` | Timeout for reflect API calls |

### Circuit Breaker

| Variable | Default | Description |
|----------|---------|-------------|
| `HINDSIGHT_CIRCUIT_BREAKER_THRESHOLD` | `5` | Failures before tripping |
| `HINDSIGHT_CIRCUIT_BREAKER_COOLDOWN` | `30s` | Cooldown period after trip |

### Workers & Queue

| Variable | Default | Description |
|----------|---------|-------------|
| `MEMORY_RETAIN_WORKERS` | `2` | Retain worker pool size |
| `MEMORY_REFLECT_WORKERS` | `2` | Reflect worker pool size |
| `MEMORY_JOB_BUFFER` | `100` | Job channel buffer size |
| `MEMORY_QUEUE_PUSH_TIMEOUT` | `5s` | Max wait to push job to queue |
| `MEMORY_QUEUE_RESPONSE_TIMEOUT` | `60s` | Max wait for job result |

### Sessions

| Variable | Default | Description |
|----------|---------|-------------|
| `MCP_MAX_SESSIONS` | `100` | Max concurrent SSE sessions |
| `MCP_SESSION_IDLE` | `30m` | Idle session cleanup timeout |

### Health Monitor

| Variable | Default | Description |
|----------|---------|-------------|
| `HEALTH_CHECK_INTERVAL` | `5s` | Health poll frequency |
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

### Alerts

| Variable | Default | Description |
|----------|---------|-------------|
| `ALERT_URL` | (empty) | Webhook URL for alerts |
| `ALERT_MODE` | `optional` | `optional` or `required` |

## Cloud Embedding/Reranker

To use cloud embedding or reranker services instead of local llama.cpp:

```bash
# Use OpenAI-compatible embedding endpoint
LLAMA_MODEL_PATH=https://api.openai.com/v1

# Use Cohere-compatible reranker endpoint
HINDSIGHT_RERANKER_MODEL=https://api.cohere.com/v1/rerank
```

When `LLAMA_MODEL_PATH` or `HINDSIGHT_RERANKER_MODEL` is an HTTP/HTTPS URL, the server skips local process management and configures Hindsight to use the remote endpoint directly.

## Production Notes

- **Ports:** 8080, 8081, 8888 must be free (unless using cloud endpoints). Server checks on startup.
- **RAM:** ~1GB minimum with local models. Cloud endpoints reduce to ~250MB.
- **Logs:** `logs/memory.log` -- JSON structured, 10MB rotation, 3 backups.
- **Crash recovery:** `logs/.mcp-pids.json` -- cleans orphaned processes on restart.
- **Security:** Bind to `127.0.0.1` for local-only access. No authentication (local use only).

## Troubleshooting

| Problem | Fix |
|---------|-----|
| "model not found" | Check `LLAMA_MODEL_PATH` points to a valid `.gguf` file or HTTP URL |
| "hindsight-api not found" | `pip install hindsight-api-slim` or set `HINDSIGHT_PATH` |
| Port already in use | `./stop.sh` to kill all services, then retry |
| Hindsight fails to start | Check `logs/hindsight-crash.log` for errors |
| High latency | Reduce `MEMORY_RETAIN_WORKERS` to 1, check OpenRouter status |
| Circuit breaker open | Check Hindsight health, wait for cooldown (30s), or increase threshold |
| Content too large | Increase `MAX_CONTENT_BYTES` or reduce input size |
| Session limit reached | Increase `MCP_MAX_SESSIONS` or check for session leaks |
