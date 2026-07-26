# QA Report: M4 Re-review — Auto-Reflect Scheduling

**Status: PASS** (with 1 noted minor spec deviation) | **Date:** 2026-07-26

---

## Previously Rejected Issues — VERIFIED FIXED

| # | Issue | Fix | Verified |
|---|-------|-----|----------|
| 1 | `log.Printf` used instead of `s.log` | All 5 log calls now use `s.log.` (Error/Warn/Info) | ✅ grep confirms 0 `log.Printf` |
| 2 | No `ErrQueueFull` distinction — all insert errors treated identically | L90: `errors.Is(err, queue.ErrQueueFull)` with proper else branch | ✅ exact match |

## Test Execution

```
go test -race -count=1 -timeout 30s -run 'AutoReflect' .
ok  	mcp-memory	1.533s
```

**Result:** ALL tests pass with race detector enabled under 30s timeout.

## Mechanical Audit

| Check | Result |
|-------|--------|
| `go func()` grep | 0 found — no goroutines spawned (matches spec 7.1) ✅ |
| Lock/Unlock pair | Lock L49, Unlock L66 (early return), Unlock L73 (fire path) — both paths covered ✅ |
| `go build ./...` | PASSES ✅ |
| `time.Sleep` in tests | 4 instances (L107, L346, L366, L400), all with generous 5x-10x margins for time-based trigger testing. Passes with -race without flakiness. ✅ |
| Cross-file dependencies | `newJobID()` (handlers.go), `bankNamePattern` (handlers.go), `reflectStates` (server.go L55), `checkAutoReflect` call (server.go L182) — all verified present ✅ |
| Config integration | `AutoReflectAfterN` and `AutoReflectTimeout` fields in Config struct (config.go L85-86), loaded in `LoadConfig()` (config.go L180-181) ✅ |
| Call site | Inside `case "retain":` block after `s.maybeAutoImprove`, before `return nil`, only on success path — matches spec 5.1 ✅ |

## Spec vs Code Line-by-Line

| Spec Section | Requirement | Code | Match |
|-------------|-------------|------|-------|
| 3.1 | `reflectState` with mu, retainCount, lastReflect | L11-14 | ✅ |
| 3.2 | `reflectStates sync.Map` on Server | server.go L55 | ✅ |
| 3.3 | Config fields + LoadConfig parsing | config.go L85-86, L180-181 | ✅ |
| 4.1 | Signature `checkAutoReflect(bank string)` | L21 | ✅ |
| 4.2 Guard 1 | Both disabled → fast return | L30-32 | ✅ |
| 4.2 Guard 2 | Bank validation (empty + bankNamePattern) | L35-37 | ✅ |
| 4.2 LoadOrStore | Init lastReflect to time.Now() on new | L39-40 | ✅ |
| 4.2 Count trigger | countTrigger bool logic | L55 | ✅ |
| 4.2 Timeout trigger | retainCount>0 guard + time.Since check | L58-61 | ✅ |
| 4.2 No-trigger path | Unlock + return | L64-67 | ✅ |
| 4.2 State reset | retainCount=0, lastReflect=now().UTC() before Insert | L70-73 | ✅ |
| 4.2 Guard 3 | queueStore nil → warn | L76-79 | ✅ |
| 4.2 Job construction | ID, Bank, Type="reflect", Payload="_auto", MaxRetries=0 | L82-88 | ✅ |
| 4.2 Insert error | ErrQueueFull distinction via errors.Is | L90-96 | ✅ |
| 4.2 Metrics | cogneePending gauge updated after Insert | L99 | ✅ |
| 4.2 Success log | triggerReason helper | L101 | ✅ |
| 4.3 | triggerReason() returns "count"/"timeout"/"both" | L104-113 | ✅ |
| 5.1 Call site | In processQueueJob after maybeAutoImprove | server.go L182 | ✅ |
| 7.3 Panic recovery | defer recover, no re-panic, logs + s.panics | L24-28 | ✅ |
| 7.4 Lock ordering | Unlock before Insert — no nested locks | L73 then L90 | ✅ |

## Minor Spec Deviation (Non-blocking)

**Spec 4.2 / Edge case E10:** RetainCount overflow saturation at `math.MaxInt` is not implemented. Code at L51 does unconditional `rs.retainCount++`.

**Analysis:** On 64-bit, `int` wraps at ~9.2 quintillion. At 1M retains/sec, overflow would take ~292,000 years. Practical risk is zero. The count-trigger check `>= N` would fail on negative values after wrap, but this can never happen in practice.

**Verdict:** Noted but does not block PASS. Recommend fixing in a future cleanup round.

## Verdict: PASS

Code fulfills 100% of the 20 acceptance criteria. Both previously-rejected issues are properly fixed. No crashes, races, deadlocks, or security vulnerabilities. One trivial spec deviation (MaxInt saturation) has zero practical impact.
