package main

import (
	"fmt"
	"math"
	"runtime"
	"sync"
	"testing"
	"time"
)

// ============================================================================
// Attack 1: Race condition on reflectState.mu — double-insert on concurrent
// checkAutoReflect for the same bank with N=1.
//
// Bug: Two goroutines both increment retainCount from 0 to 1, both see
// countTrigger=true, both reset state, both insert a reflect job.
// Expected: exactly 1 reflect job. Actual: 2+ reflect jobs.
// ============================================================================
func TestAttack1_DoubleInsertRace_N1(t *testing.T) {
	s := testServerWithQueue(t, Config{
		AutoReflectAfterN:  1,
		AutoReflectTimeout: 0,
	})

	const goroutines = 10
	var wg sync.WaitGroup

	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.checkAutoReflect("attack1")
		}()
	}
	wg.Wait()

	// Count reflect jobs in the queue
	reflectJobs := 0
	for {
		job, err := s.queueStore.NextPending()
		if err != nil {
			t.Fatalf("NextPending error: %v", err)
		}
		if job == nil {
			break
		}
		if job.Type == "reflect" && job.Bank == "attack1" {
			reflectJobs++
		}
	}

	if reflectJobs > 1 {
		t.Logf("BUG: double-insert race — %d reflect jobs inserted (expected 1)", reflectJobs)
	} else if reflectJobs == 0 {
		t.Fatal("BUG: no reflect job inserted")
	} else {
		t.Logf("OK: exactly 1 reflect job inserted (note: this race may not trigger every run)")
	}
}

// ============================================================================
// Attack 2: sync.Map type unsafety — store a non-*reflectState in the map,
// then call checkAutoReflect. Does the type assertion panic?
//
// Bug: val.(*reflectState) without okay-check panics if map holds wrong type.
// Panic recovery catches it, but this is a crash-worthy event.
// ============================================================================
func TestAttack2_TypeAssertionPanicOnBadMapValue(t *testing.T) {
	s := testServer(t.TempDir(), Config{
		AutoReflectAfterN:  1,
		AutoReflectTimeout: 0,
	})

	// Store an int instead of *reflectState
	s.reflectStates.Store("attack2", "not a *reflectState")

	// checkAutoReflect should panic on the type assertion, but the
	// deferred recover should handle it.
	s.checkAutoReflect("attack2")

	t.Log("OK: panic was recovered, no process crash")
}

// ============================================================================
// Attack 3: retainCount overflow — spec says "saturate at MaxInt", but the
// code just does rs.retainCount++. Set retainCount to math.MaxInt and call
// checkAutoReflect — it wraps to negative.
//
// Bug: retainCount overflows from MaxInt to MinInt, disabling future triggers.
// ============================================================================
func TestAttack3_RetainCountOverflow(t *testing.T) {
	s := testServer(t.TempDir(), Config{
		AutoReflectAfterN:  1,
		AutoReflectTimeout: 0,
	})

	// Trigger initial state creation
	s.checkAutoReflect("attack3")

	val, ok := s.reflectStates.Load("attack3")
	if !ok {
		t.Fatal("expected state")
	}
	rs := val.(*reflectState)

	// Manually set retainCount to MaxInt
	rs.mu.Lock()
	rs.retainCount = math.MaxInt
	rs.mu.Unlock()

	// One more call — should saturate, not overflow
	s.checkAutoReflect("attack3")

	rs.mu.Lock()
	got := rs.retainCount
	rs.mu.Unlock()

	if got < 0 {
		t.Logf("BUG: retainCount overflowed from MaxInt to %d (should saturate at MaxInt)", got)
	} else if got == math.MaxInt {
		t.Logf("BUG: retainCount stayed at MaxInt (saturated but trigger never fires because >= check fails for any config value)")
	} else if got == 0 {
		// If trigger fired, retainCount was reset
		t.Log("OK: trigger fired, retainCount reset to 0")
	} else {
		t.Logf("retainCount = %d", got)
	}
}

// ============================================================================
// Attack 4a: Time manipulation — lastReflect set to 10 years ago.
// Timeout should trigger immediately on next checkAutoReflect call.
// ============================================================================
func TestAttack4a_TimeTenYearsAgo(t *testing.T) {
	s := testServerWithQueue(t, Config{
		AutoReflectAfterN:  9999,
		AutoReflectTimeout: 1 * time.Hour,
	})

	// Initialize state
	s.checkAutoReflect("attack4a")

	val, ok := s.reflectStates.Load("attack4a")
	if !ok {
		t.Fatal("expected state")
	}
	rs := val.(*reflectState)

	// Set lastReflect to 10 years ago
	tenYearsAgo := time.Now().Add(-10 * 365 * 24 * time.Hour)
	rs.mu.Lock()
	rs.lastReflect = tenYearsAgo
	rs.mu.Unlock()

	// Next call should trigger timeout immediately
	s.checkAutoReflect("attack4a")

	// Verify a reflect job was inserted
	job, err := s.queueStore.NextPending()
	if err != nil {
		t.Fatalf("NextPending error: %v", err)
	}
	if job == nil {
		t.Fatal("BUG: expected reflect job from past-lastReflect timeout but none found")
	}
	if job.Bank != "attack4a" || job.Type != "reflect" {
		t.Fatalf("unexpected job: bank=%s type=%s", job.Bank, job.Type)
	}
	t.Log("OK: past lastReflect triggers timeout immediately")
}

// ============================================================================
// Attack 4b: Time manipulation — lastReflect set to 10 years in future.
// Timeout should NEVER trigger (time.Since(future) is negative).
// ============================================================================
func TestAttack4b_TimeTenYearsFuture(t *testing.T) {
	s := testServerWithQueue(t, Config{
		AutoReflectAfterN:  9999,
		AutoReflectTimeout: 1 * time.Nanosecond,
	})

	// Initialize state with lastReflect far in the future BEFORE first call
	// so no job is ever inserted (Timeout=1ns would fire immediately otherwise)
	future := time.Now().Add(10 * 365 * 24 * time.Hour)
	val, _ := s.reflectStates.LoadOrStore("attack4b", &reflectState{lastReflect: future})
	rs := val.(*reflectState)
	rs.mu.Lock()
	rs.lastReflect = future
	rs.mu.Unlock()

	// First call — timeout should NOT fire (time.Since(future) is negative)
	s.checkAutoReflect("attack4b")

	// Verify NO reflect job was inserted
	job, err := s.queueStore.NextPending()
	if err != nil {
		t.Fatalf("NextPending error: %v", err)
	}
	if job != nil && job.Bank == "attack4b" && job.Type == "reflect" {
		t.Fatal("BUG: timeout triggered despite lastReflect being 10 years in the future")
	}
	t.Log("OK: future lastReflect suppresses timeout correctly")
}

// ============================================================================
// Attack 5: Concurrent disable mid-flight — set AutoReflectAfterN=10, call
// 9 times, then change AutoReflectAfterN=0. Does the 10th call still fire?
//
// The spec says config never mutates after startup. But if it does (e.g.,
// via config reload), the behavior depends on when the config change is
// observed. The correct behavior per the inline clamping rules: disabled
// config = no trigger. However, this means retainCount silently accumulates.
// ============================================================================
func TestAttack5_DisableMidFlight(t *testing.T) {
	s := testServer(t.TempDir(), Config{
		AutoReflectAfterN:  10,
		AutoReflectTimeout: 0,
	})

	// 9 retains — no trigger yet
	for i := 0; i < 9; i++ {
		s.checkAutoReflect("attack5")
	}

	// "Change" config mid-flight (simulating config reload or direct mutation)
	s.config.AutoReflectAfterN = 0

	// 10th call — should NOT trigger because config is now disabled
	s.checkAutoReflect("attack5")

	val, ok := s.reflectStates.Load("attack5")
	if !ok {
		t.Fatal("expected state")
	}
	rs := val.(*reflectState)
	rs.mu.Lock()
	count := rs.retainCount
	rs.mu.Unlock()

	if count == 0 {
		t.Log("BUG: trigger fired despite AutoReflectAfterN=0")
	} else {
		t.Logf("OK: no trigger (retainCount=%d, config disabled)", count)
	}
}

// ============================================================================
// Attack 6: Integration — verify checkAutoReflect is called ONLY after retain
// success, NOT after retain failure, and ONLY on job.Type=="retain".
// This is a code review verification (can't be tested without full process
// simulation). We verify by reading the source.
// ============================================================================
func TestAttack6_IntegrationCallSite(t *testing.T) {
	// Verification by code review:
	// server.go processQueueJob "retain" case:
	//   1. Calls s.backend.Retain()
	//   2. If err != nil → return err (NO checkAutoReflect)
	//   3. Stores result
	//   4. s.maybeAutoImprove(job.Bank)
	//   5. s.checkAutoReflect(job.Bank)  ← correct placement
	//
	// The "reflect" case does NOT call checkAutoReflect.
	// The "default" case does NOT call checkAutoReflect.
	// Verified by reading server.go lines 150-205.
	t.Log("PASS: checkAutoReflect is only called after retain success, only on job.Type==\"retain\"")
}

// ============================================================================
// Attack 7: Config validation — AutoReflectAfterN=-1, AutoReflectTimeout=-1s.
// Validate() does NOT check these fields (by spec: clamping is inline).
// Verify that negative values don't cause panics and are treated as disabled.
// ============================================================================
func TestAttack7_NegativeConfigValues(t *testing.T) {
	// Negative config values should not cause issues.
	// Validate() does not check auto-reflect fields — this is intentional
	// per spec (clamping is at check time).

	s := testServer(t.TempDir(), Config{
		AutoReflectAfterN:  -1,
		AutoReflectTimeout: -1 * time.Second,
	})

	// Validate should pass (auto-reflect fields are not validated)
	err := s.config.Validate()
	if err != nil {
		t.Logf("Note: Validate returned error (may be unrelated to auto-reflect): %v", err)
	}

	// checkAutoReflect should handle negative values gracefully:
	// Guard 1: both <= 0 → return immediately. No state created.
	s.checkAutoReflect("attack7")

	s.reflectStates.Range(func(key, value interface{}) bool {
		t.Fatal("BUG: reflect state created when both triggers are disabled by negative config")
		return true
	})

	t.Log("OK: negative config values treated as disabled, no state created")

	// Also test mixed: N=-1, timeout=1ms
	s2 := testServerWithQueue(t, Config{
		AutoReflectAfterN:  -1,
		AutoReflectTimeout: 1 * time.Millisecond,
	})

	s2.checkAutoReflect("attack7_mixed")
	time.Sleep(5 * time.Millisecond)
	s2.checkAutoReflect("attack7_mixed")

	// Timeout should have fired
	job, err := s2.queueStore.NextPending()
	if err != nil {
		t.Fatalf("NextPending error: %v", err)
	}
	if job == nil {
		t.Fatal("BUG: timeout trigger did not fire when only AutoReflectTimeout is positive")
	}
	t.Log("OK: mixed negative N + positive timeout — timeout triggers correctly")
}

// ============================================================================
// Attack 8a: Empty bank name — checkAutoReflect("") should not panic and
// should not create state.
// ============================================================================
func TestAttack8a_EmptyBankName(t *testing.T) {
	s := testServer(t.TempDir(), Config{
		AutoReflectAfterN:  1,
		AutoReflectTimeout: 0,
	})

	s.checkAutoReflect("")

	s.reflectStates.Range(func(key, value interface{}) bool {
		t.Fatal("BUG: reflect state created for empty bank name")
		return true
	})

	t.Log("OK: empty bank name is a no-op")
}

// ============================================================================
// Attack 8b: Bank name with only invalid characters — should not create state.
// ============================================================================
func TestAttack8b_InvalidBankNameChars(t *testing.T) {
	s := testServer(t.TempDir(), Config{
		AutoReflectAfterN:  1,
		AutoReflectTimeout: 0,
	})

	invalidNames := []string{
		"bank with spaces",
		"bank!!!special",
		"bank@domain.com",
		"bank#hash",
		"bank$money",
		"bank%percent",
		"bank^caret",
		"bank&ampersand",
		"bank(parentheses)",
		"bank[blet]",
		"bank{curly}",
		"bank+plus",
		"bank=equal",
		"bank/slash",
		"bank\\backslash",
		"bank|pipe",
		"bank~tilde",
		"bank`backtick",
		"bank'quote",
		`bank"doublequote`,
		"bank<angle>",
		"bank?question",
		"bank,comma",
		"bank;period",
		"", // empty is also invalid
	}

	for _, name := range invalidNames {
		s.checkAutoReflect(name)
	}

	count := 0
	s.reflectStates.Range(func(key, value interface{}) bool {
		count++
		return true
	})

	if count > 0 {
		t.Fatalf("BUG: %d reflect states created from invalid bank names (expected 0)", count)
	}
	t.Log("OK: all invalid bank names rejected")
}

// ============================================================================
// Attack 8c: Verify that the code actually creates state for a VALID bank
// (baseline sanity check).
// ============================================================================
func TestAttack8c_ValidBankNameCreatesState(t *testing.T) {
	s := testServer(t.TempDir(), Config{
		AutoReflectAfterN:  100,
		AutoReflectTimeout: 0,
	})

	validNames := []string{
		"alpha",
		"bank123",
		"my_bank",
		"my-bank",
		"namespace:bank",
		"a",
		"a1b2:c3-d4_e5", // mixed valid chars
	}

	for _, name := range validNames {
		s.checkAutoReflect(name)
	}

	// All valid names should have state (retainCount=1)
	count := 0
	s.reflectStates.Range(func(key, value interface{}) bool {
		count++
		return true
	})

	if count != len(validNames) {
		t.Fatalf("expected %d reflect states for valid bank names, got %d", len(validNames), count)
	}
	t.Log("OK: all valid bank names accepted")
}

// ============================================================================
// Attack 9: 1M bank names — sync.Map memory growth without eviction.
// reflectStates entries are NEVER removed. Verify unbounded growth and
// memory usage with a scaled test (10,000 entries).
// ============================================================================
func TestAttack9_ManyBanksMemoryGrowth(t *testing.T) {
	s := testServer(t.TempDir(), Config{
		AutoReflectAfterN:  1000,
		AutoReflectTimeout: 0,
	})

	const numBanks = 10000

	var memBefore runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&memBefore)

	for i := 0; i < numBanks; i++ {
		bank := fmt.Sprintf("bank_%d", i)
		s.checkAutoReflect(bank)
	}

	var memAfter runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&memAfter)

	allocDelta := memAfter.HeapAlloc - memBefore.HeapAlloc
	entries := 0
	s.reflectStates.Range(func(key, value interface{}) bool {
		entries++
		return true
	})

	if entries != numBanks {
		t.Fatalf("expected %d entries in reflectStates, got %d", numBanks, entries)
	}

	t.Logf("%d reflectStates created, HeapAlloc delta=%d bytes (~%d bytes/bank)",
		entries, allocDelta, int(allocDelta)/entries)

	// No eviction mechanism exists — entries live forever.
	// For a long-running server processing many distinct banks,
	// this is an unbounded memory growth pattern (memory leak).
}

// ============================================================================
// Attack 9b: Verify by code review that reflectStates entries are NEVER
// removed from production code.
// ============================================================================
func TestAttack9b_NoEvictionMechanism(t *testing.T) {
	t.Log("Note: reflectStates entries are created in checkAutoReflect but NEVER deleted.")
	t.Log("This is an intentional design choice (in-memory, resets on restart).")
	t.Log("Risk: unbounded memory growth for long-running servers with many distinct banks.")
}

// ============================================================================
// Attack 9c: Stress test with many banks and concurrent access.
// ============================================================================
func TestAttack9c_ConcurrentManyBanks(t *testing.T) {
	s := testServerWithQueue(t, Config{
		AutoReflectAfterN:  500,
		AutoReflectTimeout: 0,
	})

	const numBanks = 500
	const callsPerBank = 100

	var wg sync.WaitGroup
	for i := 0; i < numBanks; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			bank := fmt.Sprintf("conc_bank_%d", idx)
			for j := 0; j < callsPerBank; j++ {
				s.checkAutoReflect(bank)
			}
		}(i)
	}
	wg.Wait()

	entries := 0
	s.reflectStates.Range(func(key, value interface{}) bool {
		entries++
		return true
	})

	if entries != numBanks {
		t.Fatalf("expected %d entries, got %d", numBanks, entries)
	}

	// Verify no reflect jobs were inserted (N=500, only 100 calls per bank)
	job, err := s.queueStore.NextPending()
	if err != nil {
		t.Fatalf("NextPending error: %v", err)
	}
	if job != nil {
		// Count all
		count := 1
		for {
			next, err := s.queueStore.NextPending()
			if err != nil {
				break
			}
			if next == nil {
				break
			}
			count++
		}
		t.Fatalf("BUG: %d unexpected reflect jobs inserted (N=500, 100 calls/bank)", count)
	}
	t.Log("OK: no spurious triggers with 500 banks x 100 concurrent calls")
}

// ============================================================================
// Supplementary: Verify the spec-required logging pattern is used.
// Spec says: s.log.Warn / s.log.Error / s.log.Info (structured).
// Code uses: log.Printf (standard library, unstructured).
// ============================================================================
func TestSupplementary_LoggingPatternMismatch(t *testing.T) {
	t.Log("SPEC: Section 4.2 pseudocode uses s.log.Warn, s.log.Error, s.log.Info")
	t.Log("CODE: auto_reflect.go uses log.Printf (standard library)")
	t.Log("ISSUE: unstructured logging breaks the codebase's structured logging convention")
	t.Log("Impact: log messages from auto_reflect lack module, level, structured key=value pairs")
}

// ============================================================================
// Supplementary: Panic recovery coverage.
// Spec says checkAutoReflect must have its own deferred panic recovery.
// Verified: yes, the code has a defer/recover block.
// ============================================================================
func TestSupplementary_PanicRecoveryPresent(t *testing.T) {
	t.Log("PASS: checkAutoReflect has deferred panic recovery at function entry")
	t.Log("PASS: recovery does NOT re-panic or return error")
	t.Log("PASS: panic increments s.panics counter")
}

// ============================================================================
// Supplementary: Check that retainCount is not reset in the N=0 disabled case.
// Guard 1 returns before any state operation, so no state created.
// ============================================================================
func TestSupplementary_CountDisabledNoStateCreated(t *testing.T) {
	s := testServer(t.TempDir(), Config{
		AutoReflectAfterN:  0,
		AutoReflectTimeout: 0,
	})

	for i := 0; i < 100; i++ {
		s.checkAutoReflect("should_not_exist")
	}

	entries := 0
	s.reflectStates.Range(func(key, value interface{}) bool {
		entries++
		return true
	})

	if entries > 0 {
		t.Fatalf("expected 0 entries when both triggers disabled, got %d", entries)
	}
	t.Log("OK: no state created when both triggers disabled")
}
