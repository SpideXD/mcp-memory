# QA Report — M2 AC-M2.27 Blocker Re-review

**Date**: 2026-07-26
**Verdict**: **PASS**

## Scope

Single-blocker re-review: worker goroutine must survive panics in ProcessFunc (AC-M2.27). Previous QA round rejected the code because `defer recover()` was at the `workerLoop` function scope — a single panic would kill the entire worker goroutine.

## Fix Verification

### 1. Recovery placement — PASS

In `queue/worker.go`, `workerLoop` (line 95-134), the loop body is now wrapped in:

```go
func() {
    defer func() {
        if r := recover(); r != nil {
            log.Printf("queue: worker %d panicked: %v", id, r)
        }
    }()
    // ... all work: NextPending, semaphore acquire, processJob ...
}()
```

This closure is called on every iteration of the `for` loop. A panic inside `processJob` (or anywhere in the closure body) is caught by this `defer recover()`, the closure exits, and the outer `for` loop advances to the next iteration. The worker goroutine stays alive.

### 2. Semaphore safety on panic — PASS

`processWithSemaphore` (line 137-139) has `defer func() { <-w.sem }()` before calling `processJob`. When `processJob` panics:
1. `processWithSemaphore`'s deferred semaphore release runs (slot freed)
2. Panic propagates to the closure
3. Closure's `defer recover()` catches it
4. Worker continues

No semaphore leak on panic.

### 3. `continue` → `return` migration — PASS

Zero `continue` statements remain in `queue/worker.go`. All control flow statements inside the closure use `return`, which correctly exits only the closure, not the outer `for` loop. This is the correct pattern for closure-wrapped iteration bodies.

### 4. Test execution — PASS

```
TestAdversarial_SemaphoreLeakOnPanic: PASS (1.606s, -race)
Full suite: PASS (183.030s, -race)
```

The `SemaphoreLeakOnPanic` test confirms: `workerCount` jobs are all processed despite each one panicking, and no jobs remain stuck in `StatusPending` (proving semaphore slots are freed after panic).

### 5. Mechanical audit — PASS

| Check | Result |
|---|---|
| `go func()` without `recover()` in worker.go | None — only `go w.workerLoop(...)`, which has per-iteration recovery |
| `Lock()` without `Unlock()` in worker.go | Both `Start()` (deferred) and `Stop()` (explicit on both branches) correct |
| `continue` remnants | Zero |
| `time.Sleep` in test path | `50ms` polling loop with 10s deadline — acceptable for adversarial test |
| Race detector | Clean on both targeted and full suite |

### 6. Edge case: select statement outside closure

The `select { case <-ctx.Done(): return; default: }` block lives outside the closure. A channel receive in a select cannot panic under normal Go runtime operation, so this is acceptable. The panic-prone work body is fully enclosed.

## Verdict

**PASS.** The fix correctly implements per-iteration panic recovery via a closure wrapping the loop body. The worker goroutine survives panics, the semaphore is properly released, and all tests pass with the race detector. AC-M2.27 is satisfied.
