# M1 Tester Pass 1 Report — Regression & Residual Detection

**Tester**: SDET (Pass 1)
**Date**: 2026-07-22
**Scope**: Verify M1 — Hindsight removal, Cognee-only simplification

---

## Summary

**BUG FOUND: 1** (SEVERE — runtime test failure due to deleted file reference)

---

## Verification Results

### 1. Zero Residual Hindsight Code (non-test files)

| Check | Result |
|-------|--------|
| `grep -rn "IsSync" --include="*.go" . \| grep -v "_test.go"` | PASS — zero results |
| `grep -rn "hindsight\|Hindsight\|HINDSIGHT" --include="*.go" . \| grep -v "_test.go"` | PASS — only traceability comments (job_tracker.go:38, session_cleaner.go:9, backend/doRequest.go:13) |
| `grep -rn "startLlamaReranker\|startHindsight" --include="*.go" . \| grep -v "_test.go"` | PASS — zero results |
| `grep -rn "reranker\|Reranker\|RERANKER" --include="*.go" . \| grep -v "_test.go"` | PASS — zero results |
| `grep -rn "BackendHindsight" --include="*.go" . \| grep -v "_test.go"` | PASS — zero results |

### 2. Deleted Files Actually Gone

| File | Result |
|------|--------|
| `backend/hindsight.go` | PASS — No such file |
| `workers.go` | PASS — No such file |
| `model/bge-reranker-base-Q4_k_m.gguf` | PASS — No such file |
| `docs/hindsight.md` | PASS — No such file |

### 3. healthCache [2]bool Verified

| Check | Result |
|-------|--------|
| `grep -rn "\[3\]bool" --include="*.go" . \| grep -v "_test.go"` | PASS — zero results |
| `allHealthy()` signature | PASS — `func (svc *services) allHealthy() (llama, cognee bool)` at services.go:247 |

### 4. Health Endpoint JSON Shape

| Check | Result |
|-------|--------|
| No "hindsight", "reranker", "retain_workers", "retain_panics" fields | PASS |
| Has "status", "llama", "cognee", "queue_depth" | PASS |
| fields: status, version, built, llama, cognee, down, queue_depth, sessions, sse_drops, uptime, panics_total, metrics | PASS |

### 5. Session Cleaner Extracted

| Check | Result |
|-------|--------|
| `session_cleaner.go` exists | PASS |
| `sessionCleaner()` is method on `*Server` | PASS (confirmed by code review) |
| `server.go` `Start()` calls `go s.sessionCleaner()` | PASS (server.go:164) |
| `workers.go` deleted | PASS |

### 6. Unconditional Cognee Infrastructure

| Check | Result |
|-------|--------|
| No `if !s.backend.IsSync()` guard in NewServer() | PASS — semaphore, jobTracker, cogneeCtx constructed unconditionally (server.go:130-138) |
| No `s.backend.IsSync()` calls anywhere | PASS — zero results |

### 7. Goroutine Leak Check

**Goroutines present** (from `grep -rn "go func\|go s\." --include="*.go" . | grep -v "_test.go"`):
- metrics/reporter.go:14, 39 — metrics reporter goroutines
- server.go:138 — `go s.jobTrackerCleanup()`
- server.go:164 — `go s.sessionCleaner()`
- server.go:167 — `go s.svc.monitor(ctx, &s.panics)`
- services.go:272, 278 — allHealthy health check goroutines (llama, cognee)
- services.go:554 — stopProcess cmd.Wait goroutine
- handlers.go:94 — Stop on panic
- handlers.go:169 — safeRouteMCP per-message
- handlers.go:282, 349 — retain, reflect goroutines
- handlers.go:472 — fireErrorWebhook
- pids.go:78 — stopProcess cmd.Wait
- auto_improve.go:175 — auto-improve goroutine
- main.go:90 — shutdown signal handler

**No orphaned worker pool goroutines** — PASS. The removed goroutines from pre-M1 (reranker checkAndRestart, hindsight checkAndRestart, allHealthy reranker check, pool workers) are all absent.

### 8. Compile Check

| Command | Result |
|---------|--------|
| `go build ./...` | PASS (exit 0) |
| `go vet ./...` | PASS with 3 pre-existing test-file warnings (atomic.Int64 copies — same as coder reported) |

### 9. Additional Verification

| AC# | Check | Result |
|-----|-------|--------|
| AC-M1.6 | `go build ./...` | PASS |
| AC-M1.7 | `go vet ./...` | PASS (pre-existing warnings only) |
| AC-M1.11 | No `reranker` in non-test files | PASS |
| AC-M1.14 | allHealthy returns 2 values | PASS |
| AC-M1.18 | toolsList unconditionally returns 5 tools (retain, recall, reflect, forget, retain_status) | PASS |
| AC-M1.19 | handlers.go has zero IsSync() calls | PASS |
| AC-M1.20 | handlers.go has zero `s.workers.` references | PASS |
| AC-M1.22 | server.go has no `workers *workerSystem` field | PASS |
| AC-M1.25 | Stop() does not call `s.workers.stop()` | PASS |
| AC-M1.26 | errors.go has no `errBinaryNotFound` | PASS |
| AC-M1.27 | pids.go has no hindsight or llama_reranker | PASS |
| AC-M1.28 | main.go Phase 1 comment mentions Cognee, not Hindsight | PASS |
| AC-M1.30 | types.go has no MemoryJob/MemoryResult | PASS |
| AC-M1.31 | Validate() default error lists only cognee-python, cognee-rust | PASS |
| AC-M1.32 | LoadConfig() Backend default is "cognee-python" | PASS |
| AC-M1.35 | Backend interface has no IsSync() | PASS |
| AC-M1.36 | `var _ backend.Backend = (*CogneeBackend)(nil)` compiles | PASS |

### 10. Test Results (batched)

| Batch | Pattern | Result |
|-------|---------|--------|
| Config & Validate | `TestConfig\|TestValidate` | PASS |
| Health | `TestHTTP_Health\|TestHealth` | PASS |
| Auto-improve | `TestMaybeAutoImprove\|TestAutoImprove` | PASS |
| Concurrent | `TestConcurrent` | PASS |
| Cognee backend | `TestCogneeBackend\|TestCogneeMock` | PASS |
| Session handling | `TestSession\|TestStartStop` | PASS |
| Chaos (most) | `TestChaos_*` (excl. LockOrdering) | PASS |
| Stress | `TestStress\|TestScale` | PASS |
| Goroutine lifecycle | `TestGoroutine\|TestPanics` | PASS |
| Tools & Protocol | `TestProtocol\|TestTool_\|TestJSONRPC` | PASS |
| Memory ops | `TestMemory\|TestRecall\|TestRetain\|TestForget\|TestReflect` | PASS |
| Services | `TestServices\|TestAllHealthy\|TestWaitHealthy` | PASS |
| Fix verification | `TestFix.*` | PASS |
| Partial init | `TestPartialInit\|TestNilMetrics\|TestNilCognee\|TestNilBackend` | PASS |
| Edge cases | `TestEdgeCase\|TestBank\|TestQueue\|TestAtomic` | PASS |
| Deep & E2E | `TestDeep\|TestE2E\|TestSSE` | PASS |
| Pool | `TestPool` | PASS (no tests to run — pool code removed) |
| Cloud | `TestCloud` | PASS (no tests to run) |
| Adversarial | `TestAdversarial\|TestPass1\|TestPass2\|TestPass3` | PASS (no tests to run) |
| Download/Makefile/Resolve | `TestResolve\|TestMakefile\|TestGitignore\|TestBackwardCompat` | 5 pre-existing failures (unrelated to M1) |

---

## BUG: SEVERE — TestChaos_LockOrderingDeadlockAnalysis reads deleted workers.go

**File**: `tester_pass3_chaos_test.go:523-528`
**Impact**: `TestChaos_LockOrderingDeadlockAnalysis` fails at runtime because it tries to `os.ReadFile("workers.go")` which was deleted in M1. The `t.Fatal(err)` immediately fails the test.

**Root Cause**: The coder's M1 test cleanup did not update `TestChaos_LockOrderingDeadlockAnalysis` to handle the deleted `workers.go` file. The test performs static lock analysis on `workers.go` which no longer exists.

**Evidence**:
```
tester_pass3_chaos_test.go:523: open workers.go: no such file or directory
--- FAIL: TestChaos_LockOrderingDeadlockAnalysis (0.00s)
```

**Fix**: Remove or guard the workers.go lock analysis section (lines 521-532). Since workers.go is deleted, its lock ordering is irrelevant. The test should either:
- Skip the workers.go block when the file doesn't exist, or
- Remove the workers.go block entirely

**Affected code** (tester_pass3_chaos_test.go):
```go
// Check workers.go
wkSrc, err := os.ReadFile("workers.go")
if err != nil {
    t.Fatal(err)
}
```

---

## Pre-existing Failures (Not M1-Related)

| Test | Reason |
|------|--------|
| `TestMakefile_Pipefail_TarFailureSilentlyIgnored` | Makefile download pipefail test — pre-existing |
| `TestBackwardCompat_NoLlamaAnywhere` | llama-server discovery — pre-existing |
| `TestGitignore_VendorIgnored` | .gitignore pattern matching — pre-existing |
| `TestMakefile_MktempFailure` | Makefile mktemp failure test — pre-existing |
| `TestGitignore_VendorPatternMatchesSubdirectory` | .gitignore pattern matching — pre-existing |

These 5 failures are pre-existing Makefile/download/gitignore tests, unrelated to M1 Hindsight removal.

---

## Conclusion

**M1 PASS 1: 1 BUG FOUND (SEVERE)**

The coder's M1 implementation is clean: all Hindsight code is removed, Cognee infra is unconditional, `session_cleaner.go` is extracted, all structural ACs pass, and `go build ./...` compiles with zero errors.

The one bug is `TestChaos_LockOrderingDeadlockAnalysis` in `tester_pass3_chaos_test.go` which tries to read the deleted `workers.go` file, causing `t.Fatal` on runtime. This is a test update oversight — the workers.go lock analysis section should be guarded or removed.
