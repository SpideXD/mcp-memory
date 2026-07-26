# M4 Spec: Auto-Reflect Scheduling

**Status:** Draft | **Date:** 2026-07-23 | **Module:** M4 (additive to M3 queue system)

---

## 1. Goal

Add automatic reflect-job triggers into the M3 SQLite queue system. After N successful retains for a given memory bank, OR after X hours have elapsed since the last auto-reflect (whichever comes first), the system automatically inserts a `type="reflect"` job with payload `"_auto"` into the SQLite queue. This is a pure additive feature — no modifications to handlers, queue internals, or the backend interface.

---

## 2. Module Decomposition

This is a single module (Extra Small — one new file, two file modifications). No sub-module decomposition needed.

### 2.1 Files to Create

| # | File | Purpose |
|---|------|---------|
| 1 | `auto_reflect.go` | Per-bank reflect state tracking, trigger logic, `checkAutoReflect(bank)` method on `*Server` |

### 2.2 Files to Modify

| # | File | Change |
|---|------|--------|
| 1 | `config.go` | Add `AutoReflectAfterN` and `AutoReflectTimeout` fields to `Config` struct and `LoadConfig()` |
| 2 | `server.go` | Add `reflectStates sync.Map` field to `Server` struct; add one call to `s.checkAutoReflect(job.Bank)` in `processQueueJob` after `s.maybeAutoImprove(job.Bank)` |

---

## 3. Data Model

### 3.1 reflectState

```go
// reflectState tracks auto-reflect triggers for a single memory bank.
// Safe for concurrent use — all field access is guarded by mu.
type reflectState struct {
    mu          sync.Mutex
    retainCount int       // successful retains since last auto-reflect
    lastReflect time.Time // time of last auto-reflect trigger (UTC)
}
```

- **Fields are NOT exported.** All access goes through methods on `reflectState`.
- **Zero-value meaning:** A zero-value `reflectState` (retainCount=0, lastReflect=time.Time{}) means the bank has never had an auto-reflect triggered. The timeout trigger is suppressed when retainCount==0, and time.Time{}.IsZero() causes the timeout branch to behave correctly: `time.Since(zeroTime)` is a large positive duration, so the first retain after startup with timeout enabled will fire the timeout trigger if the timeout has elapsed since epoch. **Mitigation:** Initialize `lastReflect` to `time.Now()` on first LoadOrStore so timeout starts counting from server start, not epoch.

### 3.2 Server field

Add to `Server` struct in `server.go`:

```go
// Auto-reflect state
reflectStates sync.Map // map[string]*reflectState — per-bank reflect tracking
```

- **`sync.Map` chosen per mission design.** LoadOrStore provides atomic get-or-create semantics. The per-entry `reflectState.mu` protects the mutable fields (retainCount, lastReflect). No additional package-level or server-level mutex is needed for the map itself.
- **Callers provide no synchronization beyond calling the exported method.**

### 3.3 Config fields

Add to `Config` struct in `config.go`:

```go
// Auto-reflect
AutoReflectAfterN int           // AUTO_REFLECT_AFTER_N, 0=disabled, default 10
AutoReflectTimeout time.Duration // AUTO_REFLECT_TIMEOUT, 0=disabled, default 6h
```

Add to `LoadConfig()`:

```go
AutoReflectAfterN:  getEnvInt("AUTO_REFLECT_AFTER_N", 10),
AutoReflectTimeout: getEnvDuration("AUTO_REFLECT_TIMEOUT", 6*time.Hour),
```

**Validation rules (applied inline, not in `Validate()`):**

| Field | Rule | Invalid Behavior |
|-------|------|------------------|
| `AutoReflectAfterN` | negative → clamped to 0 (disabled) | Applied in `checkAutoReflect` |
| `AutoReflectTimeout` | negative → clamped to 0 (disabled) | Applied in `checkAutoReflect` |
| `AutoReflectAfterN` | non-parseable env → default 10 | `getEnvInt` returns default |
| `AutoReflectTimeout` | non-parseable env → default 6h | `getEnvDuration` returns default |

---

## 4. Core Algorithm: checkAutoReflect

### 4.1 Signature

```go
func (s *Server) checkAutoReflect(bank string)
```

### 4.2 Pseudocode

```
function checkAutoReflect(bank):
    // Guard 1: both triggers disabled → fast return
    if s.config.AutoReflectAfterN <= 0 AND s.config.AutoReflectTimeout <= 0:
        return

    // Guard 2: defensive bank validation
    if bank is empty OR does not match bankNamePattern:
        return

    // Get or create per-bank state, initialize lastReflect if new
    stateI, _ := s.reflectStates.LoadOrStore(bank, &reflectState{lastReflect: time.Now()})
    st := stateI.(*reflectState)

    st.mu.Lock()
    defer st.mu.Unlock()

    // Increment retain counter (saturate at MaxInt to prevent overflow wrap)
    if st.retainCount < math.MaxInt:
        st.retainCount++

    // Check count-based trigger
    countTrigger := false
    if s.config.AutoReflectAfterN > 0 AND st.retainCount >= s.config.AutoReflectAfterN:
        countTrigger = true

    // Check timeout-based trigger
    timeoutTrigger := false
    if s.config.AutoReflectTimeout > 0 AND st.retainCount > 0:
        if time.Since(st.lastReflect) > s.config.AutoReflectTimeout:
            timeoutTrigger = true

    // No trigger condition met → return
    if NOT countTrigger AND NOT timeoutTrigger:
        return

    // Fire: reset state BEFORE Insert (so re-queue on Insert failure is safe)
    st.retainCount = 0
    st.lastReflect = time.Now().UTC()
    st.mu.Unlock()  // release lock before I/O

    // Guard 3: queue store may be nil (server starting up)
    if s.queueStore == nil:
        s.log.Warn("auto_reflect: queue store not available", "bank", bank)
        return

    // Insert reflect job
    job := &queue.Job{
        ID:         newJobID(),
        Bank:       bank,
        Type:       "reflect",
        Payload:    "_auto",
        MaxRetries: 0,  // uses default (3)
    }
    err := s.queueStore.Insert(job)
    if err != nil:
        if errors.Is(err, queue.ErrQueueFull):
            s.log.Warn("auto_reflect: queue full", "bank", bank)
        else:
            s.log.Error("auto_reflect: insert failed", "bank", bank, logger.Error(err))
        // Note: state already reset. Reflect will be retried on next trigger cycle.
        return

    s.log.Info("auto_reflect: job inserted", "bank", bank, "job_id", job.ID,
               "trigger", triggerReason(countTrigger, timeoutTrigger))
    s.metrics.cogneePending.Set(pendingCount(s.queueStore))
```

### 4.3 Trigger reason logging

The `triggerReason` helper returns:
- `"count"` when only countTrigger is true
- `"timeout"` when only timeoutTrigger is true
- `"both"` when both are true (possible if N=1 and timeout also elapsed — fires once, logged as "both")

### 4.4 Locking detail

The mutex is released BEFORE calling `s.queueStore.Insert()`. This is critical:
- `Insert` may block on the store's mutex and perform disk I/O.
- Holding `st.mu` across I/O would serialize all auto-reflect checks across all banks.
- State is reset before the Insert attempt. If Insert fails (queue full, DB error), the reflect is lost for this cycle but will be retried on the next trigger cycle (either count or timeout). This is intentional — no persistent "pending reflect" flag needed.

### 4.5 Why reset state before Insert (not after)

| Order | Risk |
|-------|------|
| Reset after Insert success | If Insert panics (unlikely), state is stale but safe. However, inserting then resetting means a crash between Insert and reset would leave the job in queue AND not reset counters, causing duplicate reflects on restart. |
| **Reset before Insert (chosen)** | If Insert fails, state is already reset. The reflect opportunity is lost for this cycle but will trigger again naturally. No duplicate risk on crash. |

---

## 5. Integration Point

### 5.1 Call site

In `server.go`, `processQueueJob()` method, inside the `case "retain":` block, after `s.maybeAutoImprove(job.Bank)` and before `return nil`:

```go
// Existing (do not remove):
s.maybeAutoImprove(job.Bank)

// NEW — M4: check auto-reflect triggers
s.checkAutoReflect(job.Bank)

return nil
```

### 5.2 Why after maybeAutoImprove

Order does not matter functionally (auto-improve and auto-reflect are independent), but:
- `maybeAutoImprove` was there first — placing new code after it minimizes diff and preserves blame.
- Both must run. Neither should block the other. If `checkAutoReflect` panics, we must not lose the auto-improve work already done. See panic recovery below.

### 5.3 Execution context

`checkAutoReflect` runs inside the queue worker goroutine's `processJob` → `processQueueJob` call chain. The worker already has panic recovery at the `processJob` level. However, a panic in `checkAutoReflect` would mark the retain job as FAILED (since the worker's defer catches panics and marks the current job as failed). **This is unacceptable:** a successfully completed retain must not be marked failed because auto-reflect tripped.

**Mitigation:** `checkAutoReflect` must have its own deferred panic recovery. See Section 7.3.

---

## 6. Disabled States

### 6.1 Both disabled (N=0 AND TIMEOUT=0)

The guard at the top of `checkAutoReflect` returns immediately. Zero allocations, zero `sync.Map` operations. Equivalent to the feature not existing.

### 6.2 N=0 only (count disabled, timeout active)

Count-based trigger is suppressed (`AutoReflectAfterN > 0` check fails). Only timeout trigger is active. `retainCount` is still incremented because the timeout trigger requires `retainCount > 0`.

### 6.3 TIMEOUT=0 only (timeout disabled, count active)

Timeout-based trigger is suppressed (`AutoReflectTimeout > 0` check fails). Only count trigger is active.

### 6.4 Auto-improve vs Auto-reflect independence

Auto-reflect and auto-improve are completely independent. Disabling one has zero effect on the other. They share no state, no mutexes, no code paths beyond the call site ordering in `processQueueJob`.

---

## 7. Goroutine Safety and Panic Recovery

### 7.1 Enumerated goroutines

| # | Goroutine | Spawned By | Recovery |
|---|-----------|------------|----------|
| 1 | Queue worker goroutines (N workers) | `queue.Worker.Start()` | YES — `workerLoop` defer recover |

`checkAutoReflect` does NOT spawn any new goroutines. All work is synchronous within the caller's goroutine.

### 7.2 Concurrency safety

| Resource | Protection | Rationale |
|----------|-----------|-----------|
| `s.reflectStates` (sync.Map) | Built-in sync.Map concurrency | LoadOrStore is atomic |
| `reflectState.retainCount` | `reflectState.mu` | Multiple workers may process retains for the same bank simultaneously |
| `reflectState.lastReflect` | `reflectState.mu` | Same mutex as retainCount |
| `s.queueStore.Insert()` | `queue.Store.mu` (internal) | Store serializes writes |
| `s.config` fields | Read-only after startup | Config never mutates after LoadConfig |

### 7.3 Panic recovery in checkAutoReflect

```go
func (s *Server) checkAutoReflect(bank string) {
    defer func() {
        if r := recover(); r != nil {
            s.panics.Add(1)
            s.log.Error("auto_reflect: panic", "bank", bank, "panic", fmt.Sprintf("%v", r))
            // Do NOT re-panic. The retain job succeeded and must stay completed.
        }
    }()
    // ... body ...
}
```

**Critical:** The deferred recover MUST NOT re-panic, MUST NOT return an error that would propagate to the worker, and MUST NOT affect the retain job's status. The retain already succeeded.

### 7.4 Lock ordering

Only one lock is held at any time:
1. `reflectState.mu` is acquired, work done, released — before any store operation.
2. `queue.Store.mu` is acquired internally by `Store.Insert()`.

No nested locks. No ABBA deadlock potential.

---

## 8. Edge Cases

| # | Scenario | Behavior |
|---|----------|----------|
| E1 | Bank name is empty string | Guard returns immediately, no state created |
| E2 | Bank name contains invalid chars | Guard returns immediately (bankNamePattern mismatch) |
| E3 | `queueStore` is nil (server starting up) | Guard after state reset: log warning, return, no panic |
| E4 | Queue is full (ErrQueueFull) | Log warning, return. State already reset — retries on next cycle |
| E5 | DB error on Insert | Log error, return. Same as E4 |
| E6 | `AUTO_REFLECT_AFTER_N=1` and `AUTO_REFLECT_TIMEOUT=1ns` | Both triggers fire simultaneously. State reset once (they share the same reset). One reflect job inserted. Logged as trigger="both". |
| E7 | Bank receives 0 retains after server start, timeout elapses | Timeout check: retainCount is 0, suppressed. No reflect. Correct — no work to reflect on. |
| E8 | Server restart: counters reset to 0 | Acceptable — `reflectStates` is in-memory only. Timeout starts counting from first retain after restart (lastReflect initialized to `time.Now()` on first LoadOrStore). |
| E9 | Negative config values | Clamped to 0 (disabled) at check time in `checkAutoReflect` |
| E10 | `retainCount` overflow after math.MaxInt retains | Saturates at `math.MaxInt`. No wrap to negative. Trigger fires on next check. |
| E11 | Auto-reflect inserts reflect job, and user manually calls memory_reflect | Both jobs are legitimate. Both are queued and processed independently. No conflict. |
| E12 | Auto-reflect job is in queue when server stops | `queue.Worker.Stop()` drains in-flight jobs. Remaining pending jobs survive in SQLite and are recovered on restart. Auto-reflect state (counters) is lost on restart — acceptable. |
| E13 | Bank with exactly 0 successful retains since startup, N=10 | retainCount=0 after increment → 1. 1 < 10, no trigger. Correct. |
| E14 | Timeout fires, then next retain comes 1ms later | State was reset (retainCount=0, lastReflect=now). Next retain: increment to 1, check timeout → time.Since(now) ≈ 1ms < 6h → no trigger. Correct debounce. |

---

## 9. Metrics Impact

No new metrics are introduced. Existing metric `memory.cognee_jobs_pending` is updated via `s.metrics.cogneePending.Set(pendingCount(s.queueStore))` after Insert, consistent with how manual retains/reflects update the gauge.

---

## 10. Acceptance Criteria (20 ACs)

### Config

| ID | Criterion | Verification |
|----|-----------|-------------|
| **AC-M4.01** | `AUTO_REFLECT_AFTER_N` env var is parsed as int, default 10. Value 0 disables count-based trigger. | `LoadConfig()` returns Config with AutoReflectAfterN=0 when env is "0". checkAutoReflect returns immediately for count branch when N=0. |
| **AC-M4.02** | `AUTO_REFLECT_TIMEOUT` env var is parsed as duration, default 6h. Value 0 disables timeout-based trigger. | `LoadConfig()` returns Config with AutoReflectTimeout=0 when env is "0". checkAutoReflect returns immediately for timeout branch when TIMEOUT=0. |
| **AC-M4.03** | Negative config values for either field are clamped to 0 at check time (disabled). | Pass -1 via env → config field is -1 (or default). checkAutoReflect treats -1 as <= 0 → disabled. No panic, no negative counters. |

### State Management

| ID | Criterion | Verification |
|----|-----------|-------------|
| **AC-M4.04** | `reflectState` struct contains `retainCount int` and `lastReflect time.Time`, each guarded by an embedded `sync.Mutex`. | Type assertion: `reflectState` has mu, retainCount, lastReflect fields. All field writes happen under mu.Lock(). |
| **AC-M4.05** | Per-bank isolation: retains in bank "alpha" do not increment the counter for bank "beta". | Call checkAutoReflect("alpha") N-1 times, then checkAutoReflect("beta") once. Verify only "alpha" triggers (its count reaches N), "beta" does not (its count is 1). |
| **AC-M4.06** | Concurrent workers incrementing different banks produce no data races. | `go test -race`: 4 goroutines, each calling checkAutoReflect for a different bank 100 times. Zero races. |
| **AC-M4.07** | Concurrent workers incrementing the SAME bank produce no data races. | `go test -race`: 4 goroutines, each calling checkAutoReflect for bank "shared" 25 times (N=100). All increments counted correctly, trigger fires exactly once. |

### Trigger Logic — Count

| ID | Criterion | Verification |
|----|-----------|-------------|
| **AC-M4.08** | After exactly N successful retain calls for a bank, a reflect job is inserted into the queue with Type="reflect", Bank=<bank>, Payload="_auto". | Set N=5. Call checkAutoReflect 5 times. On 5th call, verify `queueStore.Insert` is called with a Job matching those fields. Verify the job appears in the store. |
| **AC-M4.09** | After TIMEOUT elapses since lastReflect AND retainCount > 0, a reflect job is inserted. | Set N=9999 (effectively infinite), TIMEOUT=1ms. Call checkAutoReflect once. Advance clock by 2ms. Call checkAutoReflect again. Verify reflect job inserted on 2nd call. |
| **AC-M4.10** | After a trigger fires, retainCount is reset to 0 and lastReflect is set to current time. | Trigger fire. Immediately inspect reflectState fields. retainCount==0 AND lastReflect ≈ time.Now() (within 1s tolerance). |
| **AC-M4.11** | Inserted auto-reflect job has MaxRetries=0 (uses default 3), Status="" (defaults to pending in Insert). | Query queue store for the job after insert. Verify MaxRetries==3 (default applied by Store.Insert), Status=="pending". |
| **AC-M4.12** | When retainCount < N AND timeout has not elapsed, no reflect job is inserted and counters are preserved. | Set N=10, TIMEOUT=1h. Call checkAutoReflect 5 times. Verify 0 jobs in queue. Verify retainCount==5 (not reset). |

### Disabled States

| ID | Criterion | Verification |
|----|-----------|-------------|
| **AC-M4.13** | When AUTO_REFLECT_AFTER_N=0, count-based trigger never fires regardless of retainCount. | Set N=0, TIMEOUT=1h. Call checkAutoReflect 100 times. Verify 0 reflect jobs inserted. Count-based branch never entered. |
| **AC-M4.14** | When AUTO_REFLECT_TIMEOUT=0, timeout-based trigger never fires regardless of elapsed time. | Set N=5, TIMEOUT=0. Call checkAutoReflect 1 time. Advance clock by 100h. Call checkAutoReflect 1 more time. No timeout trigger on 2nd call. Only fires when count reaches 5. |

### Integration

| ID | Criterion | Verification |
|----|-----------|-------------|
| **AC-M4.15** | checkAutoReflect is called ONLY after backend.Retain returns success (nil error). Failed retains skip the check. | Mock backend.Retain to return error. Verify checkAutoReflect is never invoked. Mock backend.Retain to succeed. Verify checkAutoReflect IS invoked. |
| **AC-M4.16** | checkAutoReflect executes in the queue worker goroutine, not in the HTTP handler. | Add a goroutine ID assertion or trace log in checkAutoReflect. Verify the goroutine is a worker goroutine (not the HTTP handler goroutine). |
| **AC-M4.17** | A failed retain (backend.Retain returns error) does NOT increment retainCount. | Mock retain to fail 5 times, then succeed once. Verify retainCount==1 after the success, not 6. Timeout trigger does not fire spuriously from failed attempts. |

### Edge Cases & Robustness

| ID | Criterion | Verification |
|----|-----------|-------------|
| **AC-M4.18** | Empty bank name or bank name failing `bankNamePattern` causes immediate no-op return. No state created, no panic. | Call checkAutoReflect(""). Call checkAutoReflect("bank with spaces!!!"). Verify no entry in reflectStates, no log at ERROR level, no panic. |
| **AC-M4.19** | When `s.queueStore` is nil, reflect job insertion is skipped with a warning log. No panic. | Set queueStore=nil. Call checkAutoReflect enough times to trigger. Verify WARN log emitted, no nil pointer dereference, no panic. |
| **AC-M4.20** | When QueueMaxPending is reached, `Store.Insert` returns ErrQueueFull. checkAutoReflect logs a warning and returns cleanly. State was already reset — next trigger cycle will retry. | Fill queue to max. Trigger auto-reflect. Verify WARN log, no panic, state is reset (retainCount==0, lastReflect updated). |

---

## 11. Spec-implementation Consistency Verification

### 11.1 Pairwise AC ↔ Body trace

| AC | Body Section | Consistent? |
|----|-------------|-------------|
| AC-M4.01 | 3.3, 6.2 | YES — config field + disabled check |
| AC-M4.02 | 3.3, 6.3 | YES — config field + disabled check |
| AC-M4.03 | 3.3 (validation table), 4.2 (clamp in pseudocode) | YES |
| AC-M4.04 | 3.1 (struct definition) | YES |
| AC-M4.05 | 4.2 (LoadOrStore keyed by bank) | YES |
| AC-M4.06 | 7.2 (concurrency table) | YES |
| AC-M4.07 | 7.2 (concurrency table) | YES |
| AC-M4.08 | 4.2 (pseudocode: countTrigger branch) | YES |
| AC-M4.09 | 4.2 (pseudocode: timeoutTrigger branch) | YES |
| AC-M4.10 | 4.2 (pseudocode: reset block) | YES |
| AC-M4.11 | 4.2 (pseudocode: Job construction), 3.2 (Job.Validate rules) | YES |
| AC-M4.12 | 4.2 (pseudocode: early return when neither trigger fires) | YES |
| AC-M4.13 | 6.2 (N=0 only) | YES |
| AC-M4.14 | 6.3 (TIMEOUT=0 only) | YES |
| AC-M4.15 | 5.1 (call site in processQueueJob), 5.3 (execution context) | YES |
| AC-M4.16 | 5.3 (worker goroutine context) | YES |
| AC-M4.17 | 5.1 (call site: only after retain success path, error path returns early) | YES |
| AC-M4.18 | 4.2 (Guard 2: bank validation) | YES |
| AC-M4.19 | 4.2 (Guard 3: queueStore nil) | YES |
| AC-M4.20 | 4.2 (Insert error handling), Edge case E4 | YES |

### 11.2 Goroutine enumeration

| Goroutine | Recovery | Body Ref |
|-----------|----------|----------|
| Queue worker goroutines (N workers) — existing, unchanged | YES — `workerLoop` defer recover | 7.1 |

No NEW goroutines are spawned. `checkAutoReflect` runs synchronously in the caller's goroutine.

### 11.3 Lock ordering verification

Single lock path: `reflectState.mu` → release → `queue.Store.mu` (inside Insert). No nested locks. No ABBA potential. Documented in Section 7.4.

### 11.4 Concurrency safety declaration

| Type | Declaration | Section |
|------|------------|---------|
| `reflectState` | "Safe for concurrent use — mu guards all field access" | 3.1 |
| `Server.reflectStates` (sync.Map) | "Safe for concurrent use — sync.Map" | 3.2 |
| `Config.AutoReflectAfterN` / `AutoReflectTimeout` | "Read-only after startup" | 7.2 |

---

## 12. Deliverable Checklist

- [ ] `auto_reflect.go`: New file with `checkAutoReflect(bank string)` method on `*Server`
- [ ] `config.go`: Add `AutoReflectAfterN int` and `AutoReflectTimeout time.Duration` fields
- [ ] `config.go`: Add env var parsing in `LoadConfig()`
- [ ] `server.go`: Add `reflectStates sync.Map` to `Server` struct
- [ ] `server.go`: Add `s.checkAutoReflect(job.Bank)` call in `processQueueJob` after `s.maybeAutoImprove`
- [ ] All 20 ACs are implemented and traceable to spec body

---

## 13. What M4 Does NOT Do

- Does NOT modify `handlers.go`, `queue/`, `backend/`, `auto_improve.go`, `services.go`, or any test file.
- Does NOT introduce new metrics.
- Does NOT persist reflect state to disk (in-memory only, resets on restart).
- Does NOT spawn new goroutines.
- Does NOT change the queue schema, worker loop, or job processing.
- Does NOT affect manual `memory_reflect` MCP tool behavior.
- Does NOT require changes to `Validate()` in config.go (clamping is inline).
