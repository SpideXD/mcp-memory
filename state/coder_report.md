# Coder Report — Fix Stale Hindsight References

## Summary
Removed all stale references to Hindsight, reranker, worker pools, and related config from `docs/README.md` and `docs/Makefile.md`. Replaced with current architecture: Cognee Rust backend, SQLite-backed job queue (`queue/` package), and auto-reflect scheduling.

## docs/README.md — 20 replacements

| Stale Reference | Replacement |
|----------------|-------------|
| "Hindsight" (all variants) | "Cognee" / "Cognee Rust backend" |
| "Hindsight API" | "Cognee API" |
| "hindsight-api" in env var | `COGNEE_PORT`, `COGNEE_BACKEND_URL` |
| `HINDSIGHT_*` env vars | `COGNEE_*` env vars |
| `MEMORY_RETAIN_WORKERS`, `MEMORY_REFLECT_WORKERS` | `QUEUE_DB_PATH`, `QUEUE_MAX_PENDING` |
| Worker pools (2 retain + 2 reflect) | SQLite-backed job queue |
| `workers.go` in file structure | `queue/` directory |
| `hindsight.go` in file structure | Removed (replaced by `queue/`) |
| "Hindsight lifecycle" in services.go description | "Cognee lifecycle" |
| `"hindsight": true` in health response | `"cognee": true` |
| `retain_workers` / `reflect_workers` in health | `queue_depth` / `queue_processed` |
| Reranker references | Removed |
| Architecture diagram | Updated to Cognee Rust backend |
| setup instructions | Removed .venv/Hindsight pip install steps |
| Worker panic recovery | Queue worker panic recovery |
| Cloud embedding/reranker | Cloud embedding only |

## docs/Makefile.md — 4 replacements

| Stale Reference | Replacement |
|----------------|-------------|
| `.venv` creation step in setup | Removed (legacy) |
| `hindsight-api-slim` / `hindsight-client` pip install | Removed |
| `RERANK_MODEL` variable | Removed |
| `bge-reranker-base` download entry | Removed |

## Verification
```bash
grep -ci "hindsight\|HINDSIGHT\|reranker\|MEMORY_RETAIN_WORKERS\|workers.go\|hindsight-api" docs/README.md docs/Makefile.md
# Result: 0 for both files
```
