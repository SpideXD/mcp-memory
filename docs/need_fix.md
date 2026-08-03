# Required Fixes Before Production

This document turns the codebase review into an implementation checklist for the
MCP memory gateway.

The target workload is approximately 10–50 simultaneous agents. Each agent is
associated with a unique `user + profile` memory bank. Cognee/Rust remains an
external dependency; this project should not assume that Cognee can make LLM
extraction or embedding low-latency.

The goal is reliable admission, durable processing, tenant isolation, controlled
backpressure, and recoverability.

## P0 — Must fix before exposing the service

### 1. Make the documented Rust deployment reproducible ✅ DONE (M6)

- [x] Decide the supported default backend. The project documentation describes
  Rust, while [`config.go`](../config.go) defaults to `cognee-python`.
  **→ Python removed entirely. Rust is the sole backend. No silent fallback.**
- [x] Make `make setup && make run` select the intended backend explicitly.
  **→ `make setup` builds Cognee Rust via `build-cognee` target.**
- [x] Build the Rust HTTP binary with the required Cargo `bin` feature. The
  external server documentation requires `--features bin`.
  **→ `--features bin` added to Makefile `build-cognee`.**
- [x] Add a startup preflight that verifies the selected Cognee binary, llama
  binary/model, required directories, and required API key before spawning
  children.
  **→ `preflightCheck()` in config.go: 5 checks (binary/llama/model/dirs/API key) with actionable errors.**
- [x] Fail with an actionable error when the selected backend is unavailable;
  do not silently fall back to a different backend.
  **→ Single backend, no fallback possible. Preflight fails with actionable message.**
- [x] Keep the Python path documented separately if it remains supported.
  **→ Python removed entirely. All `cognee-python`/`BackendCogneePython`/`startCogneePython`/`cogneePythonEnv`/`resolveCogneePythonPath`/`COGNEE_PYTHON_PATH` references deleted.**

Commits: `3f5268b`, `bba6cd1`, `d592885`. QA PASS.

Relevant areas: [`Makefile`](../Makefile), [`config.go`](../config.go),
[`main.go`](../main.go), [`services.go`](../services.go),
[`logger/logger.go`](../logger/logger.go).

### 2. Make tenant identity authoritative

**⚠️ P0 — NOT YET STARTED**

The bank is currently supplied by the SSE URL. A bank name is a routing key, not
an authorization decision.

- [ ] Authenticate the calling agent before accepting a bank.
- [ ] Derive or validate the bank from authenticated claims representing the
  user and profile.
- [ ] Do not allow a client with a shared gateway token to select an arbitrary
  user/profile bank.
- [ ] Define a canonical bank format, including case sensitivity, Unicode
  normalization, maximum lengths, and separator escaping.
- [ ] Ensure every backend operation—remember, recall, improve, forget, and
  status—uses the authenticated bank.
- [ ] Keep the bank immutable for the lifetime of an SSE session.
- [ ] Add audit logging for rejected bank/identity mismatches without logging
  memory contents.

### 3. Close the direct Cognee and llama attack surface ✅ DONE (M6)

- [x] Bind MCP, Cognee, and llama.cpp to loopback by default.
  **→ MCP_HOST and LLAMA_HOST defaults changed from `0.0.0.0` to `127.0.0.1`.**
- [x] Require an explicit authentication configuration when MCP binds to a
  non-loopback address.
  **→ `config.Validate()` rejects non-loopback MCP_HOST without MCP_AUTH_TOKEN using `net.ParseIP().IsLoopback()`.**
- [ ] Prevent direct network access to Cognee and llama ports with binding or
  firewall rules.
  **→ Cognee/llama bind to 127.0.0.1 by default. Firewall rules are deployment-specific; documented in deployment guide.**
- [x] Never rely on Cognee's OSS HTTP server for tenant authorization; its
  single-user/no-auth model makes the gateway the security boundary.
  **→ Gateway is the sole security boundary. Cognee ports bound to loopback.**
- [ ] Rotate any API keys present in local or historical environment files.
  **→ Purged from git history via `git filter-repo` (commit `acd427c`). Current `.env` files are gitignored. Operator must rotate their own keys.**
- [x] Redact API keys and sensitive request bodies from logs and alerts.
  **→ `redactingHandler` in logger redacts `LLM_API_KEY` and `EMBEDDING_API_KEY` values to `***REDACTED***`. Message-level + WithAttrs/WithGroup preservation. 8 tests.**
- [x] Add a deployment check that rejects unsafe combinations such as public
  binding plus empty authentication.
  **→ `Validate()` loopback safety gate.**

Commits: `3f5268b`, `bba6cd1`, `d592885`. QA PASS (after fix round).

Relevant areas: [`.env.example`](../.env.example), [`handlers.go`](../handlers.go),
[`services.go`](../services.go), [`pids.go`](../pids.go),
[`config.go`](../config.go), [`logger/logger.go`](../logger/logger.go).

## P1 — Must fix for reliable 10–50 agent operation

### 4. Make startup, shutdown, and restart transactional

**🔄 PARTIALLY DONE** — shutdown race fixed (`main.go` + `lifecycle_test.go`), rest pending.

- [x] Do not call `os.Exit` while managed child processes are still running.
  **→ `shutdownDone` channel gates `main()` return. `Stop()` drains children before exit.**
- [ ] Serialize `Start()` and `Stop()` calls with an explicit lifecycle mutex or
  state transition mechanism.
- [ ] Roll back llama if Cognee startup or health readiness fails.
- [ ] Roll back children, monitor goroutines, queue stores, and workers if any
  later startup phase fails.
- [ ] Give every background goroutine a context owned by the server lifecycle.
- [ ] Cancel TTL cleanup, session cleanup, auto-reflect cleanup, monitors, and
  other workers during shutdown.
- [ ] Recreate lifecycle contexts and one-shot channels on a legitimate restart,
  or explicitly make the server process non-restartable and remove the promise
  of reusable `/start` and `/stop`.
- [ ] Add a bounded graceful-drain policy. Shutdown must not wait indefinitely
  for a 15-minute Cognee retain.

Relevant areas: [`server.go`](../server.go), [`main.go`](../main.go),
[`services.go`](../services.go), [`queue/worker.go`](../queue/worker.go),
[`queue/store.go`](../queue/store.go).

### 5. Remove SSE/session races

- [ ] Protect `MCPSession.LastActive` with a mutex or atomic timestamp.
- [ ] Define one owner for closing `SSEChannel`.
- [ ] Make the closed check and send operation safe as one operation; checking an
  atomic flag before sending is not sufficient.
- [ ] Ensure session cleanup, HTTP disconnects, server shutdown, and response
  writers cannot close/send concurrently.
- [ ] Never silently lose an MCP response because the SSE buffer is full. Use a
  bounded backpressure policy, explicit disconnect, or durable response state.
- [ ] Make panic recovery safe even when the session is already closed.

Relevant areas: [`types.go`](../types.go), [`mcp.go`](../mcp.go),
[`handlers.go`](../handlers.go), [`session_cleaner.go`](../session_cleaner.go).

### 6. Serialize service restart attempts

- [ ] Add a per-service restart mutex or in-flight state.
- [ ] Prevent a new health tick from starting another restart while the previous
  restart is backing off or waiting for readiness.
- [ ] Synchronize reads and writes of `exec.Cmd` and process state.
- [ ] Ensure a failed restart cannot overwrite a healthy process pointer.
- [ ] Keep the restart budget and circuit state observable.

Relevant area: [`services.go`](../services.go).

### 7. Make backend writes idempotent

The gateway retries POST requests and the durable queue provides at-least-once
delivery. A connection failure after Cognee has applied a request can therefore
duplicate a retain or repeat another side effect.

- [ ] Create a stable idempotency key per logical retain/reflect/forget request.
- [ ] Persist the key with the queue job.
- [ ] Pass the key to Cognee if supported, or maintain a gateway-side deduplication
  record.
- [ ] Decide which operations are safe to retry and which require confirmation.
- [ ] Add retry jitter to prevent many agents retrying together.

Relevant areas: [`backend/doRequest.go`](../backend/doRequest.go),
[`backend/cognee.go`](../backend/cognee.go), [`queue/job.go`](../queue/job.go).

### 8. Define queue fairness and ordering

- [ ] Add per-bank queue limits so one user/profile cannot consume the entire
  global queue.
- [ ] Add fair scheduling or weighted round-robin across banks.
- [ ] Decide whether operations for the same bank must be serialized.
- [ ] Put forget behind the same ordering mechanism as retain when ordering is
  required.
- [ ] Make auto-reflect and auto-improve obey the same global capacity budget.
- [ ] Return clear backpressure responses, including queue-full state and a
  suggested retry interval.

### 9. Make worker cancellation and job state correct

- [ ] If a job is claimed and cancellation occurs before processing, transition
  it out of `running` or make it recoverable with a lease.
- [ ] Add a running-job lease/heartbeat so abandoned jobs can be reclaimed
  without waiting for a full process restart.
- [ ] Enforce legal queue state transitions in the store.
- [ ] Pass the refreshed dead-letter job to `OnDead`, including its final error
  and retry count.
- [ ] Run the normal retry/dead-letter path for process panics, not only mark the
  job failed.

Relevant areas: [`queue/store.go`](../queue/store.go),
[`queue/worker.go`](../queue/worker.go), [`server.go`](../server.go).

## P2 — Correctness and operational cleanup

- [ ] Validate all values that can panic or disable background behavior:
  ticker intervals, SSE buffer size, body limits, session timeouts, retry
  delays, ports, and queue settings.
- [ ] Wire cloud embedding variables correctly or remove the incomplete cloud
  mode from the documentation.
- [ ] Make documented circuit-breaker settings configurable or remove them from
  the docs.
- [ ] Clarify that timeout-based auto-reflect is passive unless a timer exists;
  currently it is evaluated when another retain completes.
- [ ] Bound and clean up long-lived per-bank auto-improve state.
- [ ] Remove stale Hindsight references and hardcoded ports from scripts.
- [ ] Update health examples and metrics names to match the implementation.
- [ ] Remove stale tester-pass comments that report already-fixed bugs.
- [ ] Add a provider contract document for the exact Cognee HTTP API used by the
  gateway.

## Definition of done

Before calling the system production-ready, verify that:

- [ ] 50 authenticated agents can connect concurrently.
- [ ] A burst of 50 retains is durably accepted without loss.
- [ ] One noisy bank cannot starve other banks.
- [ ] A Cognee crash recovers without duplicate uncontrolled children.
- [ ] A gateway crash recovers running jobs safely.
- [ ] Shutdown completes within the documented drain budget.
- [ ] A client cannot read or write another bank.
- [ ] Retries do not duplicate logical memory writes.
- [ ] Queue age, failures, dead letters, and service health are observable.

