# M6 Coder Report: Python Removal + Rust Default + Loopback + Preflight

## Summary

All 7 AC sections implemented. `go build ./...` passes, `go vet ./...` clean, full test suite green.

## Changes Made

### A. Python Removal

| File | Change |
|------|--------|
| `types.go` | Removed `BackendCogneePython` constant. Updated Backend docstring to reflect single backend. |
| `config.go` | Removed `CogneePythonPath` field from Config struct and LoadConfig(). Changed Backend default from `"cognee-python"` to `"cognee-rust"`. Deleted Python validation case in Validate(). Updated unknown backend error to list only `"cognee-rust"`. Updated comment on Backend field. |
| `services.go` | Deleted `cogneePythonEnv()`, `startCogneePython()`, `resolveCogneePythonPath()` functions. Removed Python case blocks in `start()` and `monitor()`. Collapsed `BackendCogneePython, BackendCogneeRust` to `BackendCogneeRust` in `stop()`. Removed unused `runtime` import. |
| `main.go` | Deleted `"cognee-python"` case in `backendEnvFile()`. Added `preflightCheck()` call before `srv.Start()`. |
| `backend/backend.go` | Removed `"cognee-python"` from BackendConfig comment, Name() docstring, and New() switch. |
| `m3_tester_pass2_test.go` | Replaced all 4 `BackendCogneePython` references with `BackendCogneeRust`. |
| `units_test.go` | Deleted `TestUnit_CogneePythonEnvUsesJSONInstructorMode` and `TestUnit_CogneeEnvVariantsDiffer` (Python-specific tests). |
| `lifecycle_test.go` | Added `COGNEE_LLM_API_KEY=test-key-for-preflight` to test env vars (2 locations) so preflightCheck passes in integration tests. |

### B. Rust Default + Makefile

| File | Change |
|------|--------|
| `Makefile` | Added `--features bin` to `cargo build --release -p cognee-http-server` in `build-cognee` target. |

### C. Loopback Defaults

| File | Change |
|------|--------|
| `config.go` | Changed `MCP_HOST` default from `"0.0.0.0"` to `"127.0.0.1"`. Changed `LLAMA_HOST` default from `"0.0.0.0"` to `"127.0.0.1"`. |
| `.env.example` | Changed `MCP_HOST` and `LLAMA_HOST` to `127.0.0.1`. |
| `docs/deployment.md` | Updated MCP_HOST default to `127.0.0.1`. Added LLAMA_HOST row with `127.0.0.1` default. Removed `COGNEE_PYTHON_PATH` row. |
| `docs/README.md` | Updated MCP_HOST reference to `127.0.0.1`. |

### D. Startup Preflight

Added `preflightCheck(cfg Config) error` in `config.go`. Checks in order:
1. Cognee Rust binary exists and is executable (default `bin/cognee-http-server`)
2. llama-server binary exists (config path, then system PATH)
3. Embedding model file exists and is non-empty (skipped for cloud URLs)
4. Required data directories exist or are creatable (`cognee-data/`, `logs/`)
5. API key env var is set and non-empty (`COGNEE_LLM_API_KEY` or `OPENROUTER_API_KEY`)

Each check produces an actionable error message referencing the command to run or env var to set. Called in `main.go` before `srv.Start()` — exits with code 1 on failure.

### E. Deployment Safety Gate

Added `isLoopback(host string) bool` helper in `config.go` using `net.ParseIP()` + `IsLoopback()` (handles IPv4 `127.0.0.1`, IPv6 `::1`, and hostname `localhost` via `net.LookupIP`).

Added loopback check in `Validate()`: rejects non-loopback `MCP_HOST` when `MCP_AUTH_TOKEN` is empty. Error: `"MCP_HOST=<host> requires MCP_AUTH_TOKEN to be set for non-loopback binding"`.

### F. API Key Redaction

Added `redact(msg string) string` and `redactingHandler` in `logger/logger.go`. Uses regex to match `LLM_API_KEY=...` and `EMBEDDING_API_KEY=...` (both quoted and unquoted formats). Replaces value with `***REDACTED***` while preserving key name. Wired into `create()` via handler wrapper so all loggers automatically redact.

### G. Test Compatibility

- `go test -race -count=1 -timeout 240s ./...` — all packages green
- `go build ./...` passes
- `go vet ./...` clean

## Design Decisions

1. **Validate() CogneeBinary is optional**: Changed from required to optional (check only if set). The preflightCheck() catches missing binaries at runtime. This allows tests to create Configs without providing a real binary path, which is better separation of concerns: Validate() checks config consistency, preflightCheck() checks runtime prerequisites.

2. **Lifecycle test API key**: Added `COGNEE_LLM_API_KEY=test-key-for-preflight` to test env vars so preflightCheck passes in integration tests that spawn the binary as a subprocess.

3. **Regex for redaction**: Used `regexp.MustCompile` for the redaction pattern, compiled once at package level for performance.

## Files Modified (14 total)

1. `types.go`
2. `config.go`
3. `services.go`
4. `main.go`
5. `Makefile`
6. `.env.example`
7. `logger/logger.go`
8. `docs/deployment.md`
9. `docs/README.md`
10. `backend/backend.go`
11. `m3_tester_pass2_test.go`
12. `units_test.go`
13. `lifecycle_test.go`

## AC Verification

| AC | Status | Verification |
|----|--------|--------------|
| A1-A12 | Done | grep confirms no BackendCogneePython/CogneePythonPath references in .go files |
| B1-B3 | Done | Makefile has `--features bin`, no Python refs |
| C1-C6 | Done | Defaults changed, docs updated, .env.example updated |
| D1-D8 | Done | preflightCheck() implemented with all 5 checks, called before srv.Start() |
| E1-E4 | Done | isLoopback() uses net.ParseIP/IsLoopback, handles localhost via LookupIP |
| F1-F3 | Done | redactingHandler wraps slog, regex handles KEY=VALUE and KEY="VALUE" |
| G1-G3 | Done | Full test suite green |
