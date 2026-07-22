# Makefile Reference

The `Makefile` at the project root provides all build, test, setup, and download targets.

## Variables

| Variable | Default | Description |
|----------|---------|-------------|
| `LLAMA_VERSION` | `b10085` | llama.cpp release tag to download |
| `LLAMA_DIR` | `bin/llama` | Download destination for llama-server |
| `MODEL_DIR` | `model` | Download destination for GGUF model files |
| `EMBED_MODEL` | `model/qwen3-embedding-0.6b-Q8_0.gguf` | Embedding model file path |
| `RERANK_MODEL` | `model/bge-reranker-base-Q4_k_m.gguf` | Reranker model file path |

Override at invocation: `make LLAMA_VERSION=b5432 download-llama`.

## Targets

### `setup`
Full project setup. Runs in order:
1. Creates `.venv` virtual environment (if not present)
2. Installs `hindsight-api-slim==0.8.2` and `hindsight-client==0.8.2`
3. Runs `download-llama` to get platform-specific llama-server binary
4. Runs `download-models` to get GGUF model files

```bash
make setup
```

### `run`
Starts the MCP memory server via `go run .`. Prints a hint if `bin/llama/llama-server` is not found (suggesting `make setup`).

```bash
make run
```

### `build`
Builds the Go binary to `bin/mcp-memory`.

```bash
make build
```

### `test`
Runs all Go tests with the race detector and a 240-second timeout:

```bash
make test
# Expands to: go test -race -count=1 -timeout 240s ./...
```

### `vet`
Runs Go static analysis:

```bash
make vet
# Expands to: go vet ./...
```

### `stop`
Gracefully stops all services:

```bash
make stop
# Calls: ./scripts/stop.sh
```

### `clean`
Removes all generated artifacts:
- `.venv/` — Python virtual environment
- `bin/mcp-memory` — Go binary
- `mcp-memory` — Go binary (root)
- `bin/llama/` — Downloaded llama-server

```bash
make clean
```

### `download-llama`
Downloads the platform-specific `llama-server` binary from the llama.cpp GitHub releases.

**Platform detection:**
| `uname -s` | OS name |
|------------|---------|
| Darwin | `macos` |
| Linux | `ubuntu` |

| `uname -m` | Architecture |
|------------|-------------|
| arm64 / aarch64 | `arm64` |
| x86_64 | `x64` |

Download URL pattern: `https://github.com/ggml-org/llama.cpp/releases/download/{LLAMA_VERSION}/llama-{LLAMA_VERSION}-bin-{osname}-{arch}.tar.gz`

Extracts to `bin/llama/`. Skips if `bin/llama/llama-server` already exists and is executable.

```bash
make download-llama
# Or override version:
make download-llama LLAMA_VERSION=b5432
```

### `download-models`
Downloads GGUF model files from Hugging Face to `model/`:

| Model | Size | Source |
|-------|------|--------|
| `qwen3-embedding-0.6b-Q8_0.gguf` | ~610MB | `Qwen/Qwen3-Embedding-0.6B-GGUF` |
| `bge-reranker-base-Q4_k_m.gguf` | ~209MB | `sinjab/bge-reranker-base-Q4_K_M-GGUF` |

Skips files that already exist.

```bash
make download-models
```

## Typical Workflow

```bash
# First time setup
make setup

# Daily development
make run          # Start server
make test         # Run tests after changes
make vet          # Static analysis

# Cleanup
make stop         # Graceful shutdown
make clean        # Remove all generated artifacts
```
