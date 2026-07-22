# Hindsight Reference

Pinned versions and configuration that this MCP memory server is built against.

## Version

```
hindsight-api-slim: 0.8.2
hindsight-client:   0.8.2
```

## Install

```bash
make setup
```

This creates a `.venv/` virtual environment, installs the pinned versions, and downloads llama-server + GGUF model files:

```
hindsight-api-slim==0.8.2
hindsight-client==0.8.2
```

llama-server is downloaded to `bin/llama/`. Models are downloaded to `model/`.

## MCP Memory Configuration

Our server passes these environment variables to Hindsight:

```bash
HINDSIGHT_API_PORT=8888
HINDSIGHT_API_LLM_PROVIDER=openrouter
HINDSIGHT_API_LLM_API_KEY=$OPENROUTER_API_KEY
HINDSIGHT_API_LLM_MODEL=deepseek/deepseek-v4-flash
HINDSIGHT_API_LLM_BASE_URL=https://openrouter.ai/api/v1
HINDSIGHT_API_EMBEDDINGS_PROVIDER=openai
HINDSIGHT_API_EMBEDDINGS_OPENAI_API_KEY=not-needed
HINDSIGHT_API_EMBEDDINGS_OPENAI_BASE_URL=http://localhost:8080/v1
HINDSIGHT_API_EMBEDDINGS_OPENAI_MODEL=qwen3-embedding-0.6b-Q8_0.gguf
HINDSIGHT_API_RERANKER_PROVIDER=cohere
HINDSIGHT_API_RERANKER_COHERE_API_KEY=not-needed
HINDSIGHT_API_RERANKER_COHERE_BASE_URL=http://localhost:8081/v1/rerank
HINDSIGHT_API_RERANKER_COHERE_MODEL=bge-reranker-base-Q4_k_m.gguf
```

### Cloud Mode Configuration

When using cloud endpoints (`LLAMA_MODEL_PATH` or `HINDSIGHT_RERANKER_MODEL` set to HTTP/HTTPS URLs), the following environment variables change:

```bash
# Embedding: cloud API key and URL instead of local llama.cpp
HINDSIGHT_API_EMBEDDINGS_OPENAI_API_KEY=$CLOUD_EMBEDDING_API_KEY
HINDSIGHT_API_EMBEDDINGS_OPENAI_BASE_URL=$CLOUD_EMBEDDING_URL
HINDSIGHT_API_EMBEDDINGS_OPENAI_MODEL=$CLOUD_EMBEDDING_MODEL

# Reranker: cloud API key and URL instead of local llama.cpp
HINDSIGHT_API_RERANKER_COHERE_API_KEY=$CLOUD_RERANKER_API_KEY
HINDSIGHT_API_RERANKER_COHERE_BASE_URL=$CLOUD_RERANKER_URL
HINDSIGHT_API_RERANKER_COHERE_MODEL=$CLOUD_RERANKER_MODEL
```

## Key Design Decisions

- **`slim` variant:** Uses embedded pg0 (PostgreSQL). No external database required.
- **`openai` provider for embeddings:** llama.cpp serves an OpenAI-compatible API. The key is `not-needed` because it's local.
- **`cohere` provider for reranker:** llama.cpp serves a Cohere-compatible API. Same `not-needed` key.
- **Bank isolation:** Each user gets their own Hindsight bank (`outreach:spidex_owner`). Cross-user reflection uses `outreach:shared`.
- **Cloud endpoints:** When `LLAMA_MODEL_PATH` or `HINDSIGHT_RERANKER_MODEL` is an HTTP/HTTPS URL, the base URL is passed directly to Hindsight (skipping local llama.cpp).

## API Endpoints Used

| Endpoint | Method | Purpose | Timeout Config |
|----------|--------|---------|----------------|
| `/v1/default/banks/{bank}/memories` | POST | Retain (store) memories | `HINDSIGHT_RETAIN_TIMEOUT` |
| `/v1/default/banks/{bank}/memories/recall` | POST | Recall (search) memories | `HINDSIGHT_RECALL_TIMEOUT` |
| `/v1/default/banks/{bank}/reflect` | POST | Reflect on memories | `HINDSIGHT_REFLECT_TIMEOUT` |

## Circuit Breaker Integration

All Hindsight API calls go through the circuit breaker:

```go
// Before API call:
if s.hindsightBreaker.IsTripped() {
    return "", fmt.Errorf("Hindsight circuit breaker open -- service unavailable")
}

// After API call:
if err != nil {
    s.hindsightBreaker.RecordFailure()  // increments count, trips if >= threshold
} else {
    s.hindsightBreaker.RecordSuccess()  // resets failure count
}
```

**Configuration:**
- `HINDSIGHT_CIRCUIT_BREAKER_THRESHOLD=5` -- failures before tripping
- `HINDSIGHT_CIRCUIT_BREAKER_COOLDOWN=30s` -- how long the breaker stays open

## Retry Behavior

Failed requests retry with exponential backoff:

```
Attempt 0: wait 1s (MCP_RETRY_DELAY)
Attempt 1: wait 2s (delay * 2^1)
Attempt 2: wait 4s (delay * 2^2)
...
Max delay: 30s (MCP_RETRY_MAX_DELAY)
```

**Configuration:**
- `MCP_RETRY_ATTEMPTS=3` -- max attempts per request
- `MCP_RETRY_DELAY=1s` -- base delay
- `MCP_RETRY_MAX_DELAY=30s` -- exponential backoff cap

## Performance Optimization

### Health Check Caching
- Health status cached for 10s (avoids 3 HTTP requests per tool call)
- Concurrent refreshes deduplicated via `singleflight.Group`
- Cache stores `[3]bool` for llama, reranker, hindsight

### Torch/Sklearn Blocking
Hindsight's `query_analyzer` and `local-ml` backends try to import torch/sklearn (400MB+). We block these at startup:

```go
env = append(env,
    "TORCH_UNAVAILABLE=1",
    "PYTHON_DISABLE_TORCH=1",
)
```

Hindsight gracefully falls back via `ImportError` when these packages aren't available.

## Why Pinned

Hindsight is under active development. Configuration keys, API paths, and behavior may change between versions. This document serves as the "known good" configuration for mcp-memory v2.

If upgrading Hindsight:
1. Test with `hindsight-api-slim==<new_version>`
2. Verify all e2e tests pass
3. Update this document
4. Commit the version bump
