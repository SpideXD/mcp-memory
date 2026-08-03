# M6 Spec: Python Removal + Rust Default + Loopback + Preflight

## Scope

Remove all Cognee Python support, make Cognee Rust the sole backend,
enforce loopback binding by default, add startup preflight, and add
deployment safety checks.

## ACs

### A. Python Removal

- **AC-A1**: `BackendCogneePython` constant deleted from `types.go`. `Backend` type docstring updated to reflect single backend.
- **AC-A2**: `BackendCogneeRust` constant remains, `BackendCogneePython` removed. All `case BackendCogneePython:` blocks in `services.go` deleted. `case BackendCogneePython, BackendCogneeRust:` collapses to `case BackendCogneeRust:`.
- **AC-A3**: `startCogneePython()` function (lines 468-490) deleted entirely.
- **AC-A4**: `resolveCogneePythonPath()` function (lines 519-540) deleted entirely.
- **AC-A5**: `cogneePythonEnv()` function (lines 447-454) deleted entirely.
- **AC-A6**: Python-specific functions (`startCogneePython`, `resolveCogneePythonPath`, `cogneePythonEnv`) removed from `services.go`. `cogneeBaseEnv()` and `cogneeRustEnv()` retained unchanged.
- **AC-A7**: `CogneePythonPath` field removed from `Config` struct in `config.go`.
- **AC-A8**: `Backend` default changed from `"cognee-python"` to `"cognee-rust"` in `config.go` line 158.
- **AC-A9**: `config.Validate()` Python case (lines 271-296) deleted. Unknown backend error (line 326) updated to list only `"cognee-rust"`.
- **AC-A10**: `main.go` `backendEnvFile()` Python case (line 136) deleted.
- **AC-A11**: All `BackendCogneePython` references removed from test files. Test Config literals that use `BackendCogneePython` changed to `BackendCogneeRust`.
- **AC-A12**: `go build ./...` passes. `go vet ./...` clean.

### B. Rust Default + Makefile

- **AC-B1**: `make setup` builds the Rust binary via `$(MAKE) build-cognee` (already wired).
- **AC-B2**: `make build-cognee` builds with `--features bin`: change `cargo build --release -p cognee-http-server` to `cargo build --release -p cognee-http-server --features bin`.
- **AC-B3**: No Python references remain in Makefile.

### C. Loopback Defaults

- **AC-C1**: `MCP_HOST` default in `config.go` changed from `"0.0.0.0"` to `"127.0.0.1"`.
- **AC-C2**: `LLAMA_HOST` default in `config.go` changed from `"0.0.0.0"` to `"127.0.0.1"`.
- **AC-C3**: `services.go` `startCogneeRust` `--host` flag: if present and hardcoded to `0.0.0.0`, change to `127.0.0.1`.
- **AC-C4**: `services.go` `startLlama` `--host` flag: if present and hardcoded to `0.0.0.0`, change to `127.0.0.1`.
- **AC-C5**: `.env.example`: `MCP_HOST` and `LLAMA_HOST` defaults changed to `127.0.0.1`. Remove stale Hindsight/`COGNEE_PYTHON_PATH` entries.
- **AC-C6**: Docs (`docs/deployment.md`, `docs/README.md`) updated: `MCP_HOST` and `LLAMA_HOST` defaults shown as `127.0.0.1`.

### D. Startup Preflight

- **AC-D1**: New function `preflightCheck(cfg Config) error` in `config.go` called by `main.go` before `srv.Start()`.
- **AC-D2**: Checks that Cognee Rust binary exists and is executable: `os.Stat()` + mode check `&0111 != 0` on `cfg.CogneeBinary` (default `bin/cognee-http-server`).
- **AC-D3**: Checks that llama-server binary exists and is executable: `os.Stat()` + `&0111 != 0` on resolved path (env var `LLAMA_PATH`, `bin/llama/llama-server`, or `which llama-server`).
- **AC-D4**: Checks that embedding model file exists and is non-empty: `cfg.EmbeddingModelPath` or default `model/qwen3-embedding-0.6b-Q8_0.gguf`.
- **AC-D5**: Checks that required data directories exist (or are creatable): `cfg.CogneeDataDir` (default `./cognee-data`), `logs/`.
- **AC-D6**: Checks that API key env var is set and non-empty: `LLM_API_KEY` (the env var Cognee Rust reads).
- **AC-D7**: Each check produces an actionable error message on failure: "Cognee Rust binary not found at /path — run 'make build-cognee'".
- **AC-D8**: Preflight is called BEFORE `srv.Start()` in `main.go`. If preflight fails, process exits with code 1 and a descriptive message printed to stderr.

### E. Deployment Safety Gate

- **AC-E1**: `config.Validate()` rejects `MCP_HOST` set to a non-loopback address (`0.0.0.0` or any external IP) when `MCP_AUTH_TOKEN` is empty. Error: "MCP_HOST=<host> requires MCP_AUTH_TOKEN to be set for non-loopback binding".
- **AC-E2**: Loopback detection uses `net.ParseIP()` and `.IsLoopback()` — handles both IPv4 `127.0.0.1` and IPv6 `::1`.
- **AC-E3**: `localhost` hostname is treated as loopback-equivalent (net.LookupIP).
- **AC-E4**: The check is in `Validate()`, not main, so tests that create Configs with non-loopback + no auth still pass — only the real startup hits it.

### F. API Key Redaction

- **AC-F1**: Logger strips `LLM_API_KEY` and `EMBEDDING_API_KEY` values from any log output. Implementation: a `redact(msg string) string` helper applied in `logger/logger.go` `log()` method.
- **AC-F2**: Redaction replaces the value with `***REDACTED***` while preserving the key name. Pattern: `LLM_API_KEY=sk-abc123` → `LLM_API_KEY=***REDACTED***`.
- **AC-F3**: Redaction handles both `KEY=VALUE` and `KEY="VALUE"` formats.

### G. Test Compatibility

- **AC-G1**: All existing tests continue to pass: `go test -race -count=1 -timeout 240s ./...` — all packages green.
- **AC-G2**: Tests updated for removed types: `BackendCogneePython` → `BackendCogneeRust` in test Config literals, Python-specific test cases deleted.
- **AC-G3**: `CogneePythonPath` references in tests removed.

## Files Modified

| File | Change |
|------|--------|
| `types.go` | Delete `BackendCogneePython` const, update docstring |
| `config.go` | Remove `CogneePythonPath` field, change defaults (BACKEND, MCP_HOST, LLAMA_HOST), delete Python Validate case, add safety gate, add `preflightCheck()` |
| `services.go` | Delete `startCogneePython()`, `resolveCogneePythonPath()`, `cogneePythonEnv()`. Collapse Python cases. Change `--host 0.0.0.0` → `127.0.0.1` |
| `main.go` | Delete Python case in `backendEnvFile()`, add preflight call |
| `Makefile` | Add `--features bin` to `build-cognee` |
| `.env.example` | Change MCP_HOST/LLAMA_HOST to 127.0.0.1, remove Python entries |
| `logger/logger.go` | Add key redaction |
| `docs/deployment.md` | Update defaults |
| `docs/README.md` | Remove Python references |
| `*.go` test files | Replace `BackendCogneePython` with `BackendCogneeRust`, delete Python-specific tests |

## Goroutine Inventory

No new goroutines. Preflight is synchronous. Log redaction is a string filter.
