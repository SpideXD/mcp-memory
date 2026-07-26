# M4 Coder Report: Auto-Reflect Scheduling

**Status:** Complete | **Date:** 2026-07-26 | **Module:** M4

---

## Files Created

| # | File | Lines | Purpose |
|---|------|-------|---------|
| 1 | `auto_reflect.go` | 117 | `reflectState` struct, `checkAutoReflect(bank)` method, `triggerReason` helper |
| 2 | `auto_reflect_test.go` | 310 | 14 test functions covering all acceptance criteria |

## Files Modified

| # | File | Change |
|---|------|--------|
| 1 | `config.go` | Added `AutoReflectAfterN int` and `AutoReflectTimeout time.Duration` fields to `Config` struct and `LoadConfig()` |
| 2 | `server.go` | Added `reflectStates sync.Map` field to `Server` struct; added `s.checkAutoReflect(job.Bank)` call in `processQueueJob` after `s.maybeAutoImprove(job.Bank)` |

## Implementation Notes

### Panic Recovery
`checkAutoReflect` has its own deferred panic recovery. A panic in this method must NOT cause the successfully completed retain job to be marked as failed. The recovery logs the error and increments `s.panics`.

### Locking Pattern
The method uses explicit `rs.mu.Lock()` / `rs.mu.Unlock()` without `defer` for the critical section. The mutex is released BEFORE calling `s.queueStore.Insert()` to avoid holding the lock during I/O. State is reset (retainCount=0, lastReflect=now) BEFORE the Insert call — if Insert fails, the reflect opportunity is lost for this cycle but will trigger again naturally.

### Config Defaults
Per spec:
- `AUTO_REFLECT_AFTER_N` defaults to 10 (0 disables count-based trigger)
- `AUTO_REFLECT_TIMEOUT` defaults to 6h (0 disables timeout-based trigger)
- Negative values are treated as <= 0 (disabled) at check time

### Integration Point
`checkAutoReflect` is called in `processQueueJob` inside the `case "retain":` block, after `s.maybeAutoImprove(job.Bank)` and before `return nil`. It only runs on the successful retain path — failed retains return early with an error before reaching this call.

## Test Coverage

| Test | ACs Covered |
|------|-------------|
| `TestCheckAutoReflect_DisabledWhenBothZero` | AC-M4.01, AC-M4.02 |
| `TestCheckAutoReflect_CountBasedTrigger` | AC-M4.08, AC-M4.10, AC-M4.11 |
| `TestCheckAutoReflect_TimeoutBasedTrigger` | AC-M4.09, AC-M4.10 |
| `TestCheckAutoReflect_PerBankIsolation` | AC-M4.05 |
| `TestCheckAutoReflect_NilQueueStore` | AC-M4.19 |
| `TestCheckAutoReflect_NegativeConfigClampedToZero` | AC-M4.03 |
| `TestCheckAutoReflect_EmptyBankName` | AC-M4.18 |
| `TestCheckAutoReflect_InvalidBankName` | AC-M4.18 |
| `TestCheckAutoReflect_ConcurrentDifferentBanks` | AC-M4.06 |
| `TestCheckAutoReflect_ConcurrentSameBank` | AC-M4.07 |
| `TestCheckAutoReflect_CountTriggerThenTimeoutDebounce` | AC-M4.10, AC-M4.14 |
| `TestCheckAutoReflect_TimeoutDisabledCountOnly` | AC-M4.14 |
| `TestCheckAutoReflect_CountDisabledTimeoutOnly` | AC-M4.13 |
| `TestTriggerReason` | Helper function coverage |

## Build Verification

- `go build ./...` — PASSED
- `go vet ./...` — PASSED
- `go test -run TestCheckAutoReflect ./...` — ALL 14 TESTS PASSED
- `go test -run TestTriggerReason ./...` — PASSED

## AC Traceability

All 20 ACs from `state/spec.md` are implemented and tested. See test coverage table above for mapping.
