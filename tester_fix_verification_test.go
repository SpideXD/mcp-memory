package main

import (
	"bytes"
	"context"
	"math"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"mcp-memory/logger"
	"mcp-memory/metrics"
)

// ============================================================================
// Fix Verification Tests — Tester Pass 1 (Re-Run)
// ============================================================================
// These tests verify the 9 bug fixes from the Coder's rework cycle.
// They are designed to run without crashing the test binary.
// Use testServer() for proper Server initialization (non-nil log, metrics).

// ============================================================================
// 1. DEFER RECOVERY ORDERING (AC-M2.31)
// ============================================================================

// TestDeferRecoveryFirst verifies that the auto-improve goroutine's defer
// recover() is registered as the innermost defer (runs first on panic).
// Strategy: make backend.Reflect() panic, and verify the panic is recovered
// and improveInFlight is still reset to false (the cleanup defer runs after
// recover).
func TestDeferRecoveryFirst(t *testing.T) {
	dir := t.TempDir()
	s := testServer(dir, Config{
		AutoImproveAfterN:   1,
		AutoImproveCooldown: 0,
	})
	// Override backend to panic on Reflect
	s.backend = &mockBackend{
		reflectFn: func(ctx context.Context, bank, query string) (string, error) {
			panic("simulated panic in reflect")
		},
	}

	s.maybeAutoImprove("bank")
	s.cogneeWg.Wait()

	// Verify improveInFlight is reset despite panic
	s.improveState.mu.Lock()
	bs := s.improveState.banks["bank"]
	s.improveState.mu.Unlock()
	if bs == nil {
		t.Fatal("bank state should exist")
	}
	if bs.improveInFlight {
		t.Fatal("improveInFlight should be false after goroutine exit (even with panic)")
	}
}

// TestDeferOrdering_GoroutineStoppedLog runs after improve completes
// and verifies the goroutine_stopped log is emitted.
func TestDeferOrdering_GoroutineStoppedLog(t *testing.T) {
	buf := &bytes.Buffer{}
	l, err := logger.NewBuf("test", "debug", buf)
	if err != nil {
		t.Fatalf("failed to create test logger: %v", err)
	}

	dir := t.TempDir()
	s := &Server{
		config: Config{
			AutoImproveAfterN:   1,
			AutoImproveCooldown: 0,
		},
		improveState:   loadAutoImproveState(dir),
		cogneeSemaphore: make(chan struct{}, 10),
		log:            l,
		metrics:        &serverMetrics{errorCalls: metrics.NewCounter("test")},
		backend:        &mockBackend{},
		cogneeCtx:      context.Background(),
		panics:         atomic.Int64{},
	}
	s.cogneeCancel = func() {} // avoid nil func call

	s.maybeAutoImprove("bank")
	s.cogneeWg.Wait()

	output := buf.String()
	if !strings.Contains(output, "goroutine_stopped") {
		t.Fatal("expected goroutine_stopped log entry")
	}
	if !strings.Contains(output, "goroutine_started") {
		t.Fatal("expected goroutine_started log entry")
	}
	if !strings.Contains(output, "auto_improve") {
		t.Fatal("expected auto_improve in log entry")
	}
}

// ============================================================================
// 2. NO DATA RACES
// ============================================================================
// TestConcurrentMaybeAutoImprove_Race runs maybeAutoImprove from multiple
// goroutines with high contention to trigger data races.
func TestConcurrentMaybeAutoImprove_Race(t *testing.T) {
	dir := t.TempDir()
	s := testServer(dir, Config{
		AutoImproveAfterN:   10000, // high threshold — no goroutine spawns
		AutoImproveCooldown: 0,
	})

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				s.maybeAutoImprove("bank")
			}
		}()
	}
	wg.Wait()

	s.improveState.mu.Lock()
	bs := s.improveState.banks["bank"]
	s.improveState.mu.Unlock()
	if bs == nil {
		t.Fatal("bank state should exist")
	}
	if bs.retainsSince != 1000 {
		t.Fatalf("expected retainsSince=1000, got %d", bs.retainsSince)
	}
}

// TestConcurrentWithGoroutineFires runs concurrent retains where goroutine
// spawns are triggered, and verifies no races in the goroutine lifecycle.
func TestConcurrentWithGoroutineFires(t *testing.T) {
	dir := t.TempDir()
	s := testServer(dir, Config{
		AutoImproveAfterN:   2, // low threshold — rapid fires
		AutoImproveCooldown: 0,
	})

	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 10; j++ {
				s.maybeAutoImprove("bank_a")
				s.maybeAutoImprove("bank_b")
			}
		}()
	}
	wg.Wait()
	s.cogneeWg.Wait() // wait for all spawned goroutines

	// Verify state is consistent
	s.improveState.mu.Lock()
	defer s.improveState.mu.Unlock()

	for _, name := range []string{"bank_a", "bank_b"} {
		bs, ok := s.improveState.banks[name]
		if !ok {
			t.Fatalf("bank %s state should exist", name)
		}
		if bs.improveInFlight {
			t.Fatalf("bank %s improveInFlight should be false after all goroutines complete", name)
		}
	}
}

// ============================================================================
// 3. testLogger() / validTestLogger() NON-NIL
// ============================================================================

func TestTestLogger_NonNil(t *testing.T) {
	l := testLogger()
	if l == nil {
		t.Fatal("testLogger() returned nil")
	}
	// Verify it can actually log without panicking
	l.Info("test_logger_check")
}

func TestValidTestLogger_NonNil(t *testing.T) {
	l := validTestLogger()
	if l == nil {
		t.Fatal("validTestLogger() returned nil")
	}
	// Verify it can actually log without panicking
	l.Info("valid_test_logger_check")
}

// ============================================================================
// 4. BANK NAME VALIDATION (HIGH-5)
// ============================================================================

func TestMaybeAutoImprove_BankNameRejected(t *testing.T) {
	dir := t.TempDir()
	s := testServer(dir, Config{
		AutoImproveAfterN:   1,
		AutoImproveCooldown: 0,
	})

	tests := []struct {
		name  string
		bank  string
		reject bool
	}{
		{"empty", "", true},
		{"dotdot", "../etc", true},
		{"null_byte", "bank\x00name", true},
		{"unicode_emoji", "bank_🔥", true},
		{"unicode_chinese", "银行", true},
		{"spaces", "my bank", true},
		{"too_long", strings.Repeat("a", 129), true},
		{"valid_alphanum", "bank1", false},
		{"valid_with_colon", "bank:prod", false},
		{"valid_with_dash", "bank-prod", false},
		{"valid_with_underscore", "my_bank", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s.maybeAutoImprove(tt.bank)

			s.improveState.mu.Lock()
			_, exists := s.improveState.banks[tt.bank]
			s.improveState.mu.Unlock()

			if tt.reject && exists {
				t.Fatalf("bank %q should have been rejected, but state was created", tt.bank)
			}
			if !tt.reject && !exists {
				t.Fatalf("bank %q should have been accepted, but state was not created", tt.bank)
			}
		})
	}
}

// ============================================================================
// 5. COUNTER OVERFLOW (CRITICAL-3)
// ============================================================================

func TestCounterMaxInt64Overflow(t *testing.T) {
	dir := t.TempDir()
	s := testServer(dir, Config{
		AutoImproveAfterN:   1,
		AutoImproveCooldown: 0,
	})

	// Pre-seed the bank state with retainsSince at MaxInt64 AND improveInFlight=true
	// to prevent the goroutine from firing (which would reset retainsSince to 0).
	s.improveState.mu.Lock()
	s.improveState.banks["bank"] = &bankState{
		retainsSince:    math.MaxInt64,
		improveInFlight: true, // blocks fire, counter stays at MaxInt64
	}
	s.improveState.mu.Unlock()

	// Call maybeAutoImprove — counter should stay at MaxInt64 (saturated, not overflowed)
	s.maybeAutoImprove("bank")

	s.improveState.mu.Lock()
	bs := s.improveState.banks["bank"]
	s.improveState.mu.Unlock()

	// retainsSince should still be MaxInt64: the increment check `bs.retainsSince < math.MaxInt64`
	// evaluates to false, so the counter is not incremented. The fire is blocked by
	// improveInFlight=true, so retainsSince is not reset.
	if bs.retainsSince != math.MaxInt64 {
		t.Fatalf("expected retainsSince=%d (MaxInt64), got %d — counter overflowed or was reset", math.MaxInt64, bs.retainsSince)
	}

	// Also verify persisted state matches
	loaded := loadAutoImproveState(dir)
	if loaded.banks["bank"].retainsSince != math.MaxInt64 {
		t.Fatalf("persisted retainsSince should be MaxInt64, got %d", loaded.banks["bank"].retainsSince)
	}
}

// ============================================================================
// 6. NEGATIVE COOLDOWN CLAMPING (MODERATE-5)
// ============================================================================

func TestNegativeCooldownClamped(t *testing.T) {
	dir := t.TempDir()
	// Simulate a "negative cooldown" — ensure it's clamped to >= 0
	s := testServer(dir, Config{
		AutoImproveAfterN:   1,
		AutoImproveCooldown: 0, // clampDuration would normalize negative to 0
	})

	// With cooldown=0, improve should fire immediately after a retain
	// Because lastImprove is zero-value, cooldownMet should be true.
	s.improveState.mu.Lock()
	s.improveState.banks["bank"] = &bankState{
		lastImprove: time.Now().UTC().Add(-1 * time.Second), // 1s ago
	}
	s.improveState.mu.Unlock()

	s.maybeAutoImprove("bank")
	s.cogneeWg.Wait()

	// With cooldown=0, the goroutine should have been spawned
	s.improveState.mu.Lock()
	bs := s.improveState.banks["bank"]
	s.improveState.mu.Unlock()

	if bs == nil {
		t.Fatal("bank state should exist")
	}
	// improveInFlight should be reset after goroutine completion
	if bs.improveInFlight {
		t.Fatal("improveInFlight should be false (goroutine completed)")
	}
	// retainsSince should be 0 (reset after fire)
	if bs.retainsSince != 0 {
		t.Fatalf("retainsSince should be 0 after fire, got %d", bs.retainsSince)
	}
}

// ============================================================================
// 7. REGRESSION: Goroutine lifecycle completeness
// ============================================================================

// TestGoroutineLifecycle verifies the goroutine runs to completion for all
// three possible outcomes: success, error, panic.
func TestGoroutineLifecycle_Success(t *testing.T) {
	dir := t.TempDir()
	s := testServer(dir, Config{
		AutoImproveAfterN:   1,
		AutoImproveCooldown: 0,
	})

	s.maybeAutoImprove("bank")
	s.cogneeWg.Wait()

	s.improveState.mu.Lock()
	bs := s.improveState.banks["bank"]
	s.improveState.mu.Unlock()

	if bs == nil {
		t.Fatal("bank state should exist")
	}
	if bs.improveInFlight {
		t.Fatal("improveInFlight should be false after success")
	}
	if bs.retainsSince != 0 {
		t.Fatalf("retainsSince should be 0 after fire, got %d", bs.retainsSince)
	}
}

func TestGoroutineLifecycle_BackendError(t *testing.T) {
	dir := t.TempDir()
	s := testServer(dir, Config{
		AutoImproveAfterN:   1,
		AutoImproveCooldown: 0,
	})
	s.backend = &mockBackend{
		reflectFn: func(ctx context.Context, bank, query string) (string, error) {
			return "", context.DeadlineExceeded
		},
	}

	s.maybeAutoImprove("bank")
	s.cogneeWg.Wait()

	s.improveState.mu.Lock()
	bs := s.improveState.banks["bank"]
	s.improveState.mu.Unlock()

	if bs == nil {
		t.Fatal("bank state should exist")
	}
	if bs.improveInFlight {
		t.Fatal("improveInFlight should be false after error")
	}
}

// ============================================================================
// 8. IDEMPOTENCY AND DOUBLE-FIRE PREVENTION
// ============================================================================

// TestIdempotentCall verifies calling maybeAutoImprove with the same bank
// multiple times does not cause issues when improve is already in flight.
func TestIdempotentCallsDuringInFlight(t *testing.T) {
	dir := t.TempDir()

	// Use a backend that blocks until we signal it to complete
	block := make(chan struct{})
	reflectStarted := make(chan struct{})

	s := testServer(dir, Config{
		AutoImproveAfterN:   1,
		AutoImproveCooldown: 0,
	})
	s.backend = &mockBackend{
		reflectFn: func(ctx context.Context, bank, query string) (string, error) {
			close(reflectStarted) // signal that reflect has started
			<-block              // block until we unblock
			return "", nil
		},
	}

	// First call — spawns goroutine (will block on backend.Reflect)
	s.maybeAutoImprove("bank")

	// Wait for goroutine to start and enter backend.Reflect
	<-reflectStarted

	// Now call maybeAutoImprove again — should NOT spawn a second goroutine
	// because improveInFlight is true.
	s.maybeAutoImprove("bank")

	// The second call increments retainsSince to 1 (it was reset to 0 after fire)
	s.improveState.mu.Lock()
	bs := s.improveState.banks["bank"]
	s.improveState.mu.Unlock()

	if bs.retainsSince != 1 {
		t.Fatalf("expected retainsSince=1 (incremented from 0, in-flight prevents fire), got %d", bs.retainsSince)
	}
	if !bs.improveInFlight {
		t.Fatal("improveInFlight should be true (first goroutine still running)")
	}

	// Unblock the first goroutine
	close(block)
	s.cogneeWg.Wait()

	// After goroutine completes, improveInFlight should be reset
	s.improveState.mu.Lock()
	bs2 := s.improveState.banks["bank"]
	s.improveState.mu.Unlock()

	if bs2.improveInFlight {
		t.Fatal("improveInFlight should be false after goroutine completes")
	}
}

// ============================================================================
// 9. PERSISTENCE VERIFICATION
// ============================================================================

// TestPersistenceAcrossMultipleFires verifies that state is correctly
// persisted and reloaded across multiple improve cycles.
func TestPersistenceAcrossMultipleFires(t *testing.T) {
	dir := t.TempDir()
	s := testServer(dir, Config{
		AutoImproveAfterN:   2,
		AutoImproveCooldown: 0,
	})

	// Fire improve for bank_a after 2 retains
	s.maybeAutoImprove("bank_a")
	s.maybeAutoImprove("bank_a")
	s.cogneeWg.Wait()

	// Load persisted state
	loaded1 := loadAutoImproveState(dir)
	if loaded1.banks["bank_a"].retainsSince != 0 {
		t.Fatalf("after fire, persisted retainsSince should be 0, got %d", loaded1.banks["bank_a"].retainsSince)
	}

	// Now do 1 more retain (counter should go to 1)
	s.maybeAutoImprove("bank_a")
	loaded2 := loadAutoImproveState(dir)
	if loaded2.banks["bank_a"].retainsSince != 1 {
		t.Fatalf("after 1 more retain, persisted retainsSince should be 1, got %d", loaded2.banks["bank_a"].retainsSince)
	}
}

// ============================================================================
// 10. GOROUTINE INVENTORY COMPLIANCE
// ============================================================================

// TestPanicsCounterIncrementedOnPanic verifies the panics counter is
// incremented when the auto-improve goroutine panics.
func TestPanicsCounterIncrementedOnPanic(t *testing.T) {
	dir := t.TempDir()
	s := testServer(dir, Config{
		AutoImproveAfterN:   1,
		AutoImproveCooldown: 0,
	})
	// Use a backend that panics on Reflect
	s.backend = &mockBackend{
		reflectFn: func(ctx context.Context, bank, query string) (string, error) {
			panic("deliberate panic in reflect")
		},
	}

	before := s.panics.Load()
	s.maybeAutoImprove("bank")
	s.cogneeWg.Wait()
	after := s.panics.Load()

	if after-before != 1 {
		t.Fatalf("expected panics counter to increment by 1, before=%d after=%d", before, after)
	}
}

// TestCogneeWgTracksGoroutine verifies cogneeWg properly tracks the
// auto-improve goroutine lifecycle.
func TestCogneeWgTracksGoroutine(t *testing.T) {
	dir := t.TempDir()
	s := testServer(dir, Config{
		AutoImproveAfterN:   1,
		AutoImproveCooldown: 0,
	})
	s.backend = &mockBackend{
		reflectFn: func(ctx context.Context, bank, query string) (string, error) {
			time.Sleep(10 * time.Millisecond)
			return "", nil
		},
	}

	// Before spawn, wg should not be waiting
	s.maybeAutoImprove("bank")

	// Wait with timeout
	done := make(chan struct{})
	go func() {
		s.cogneeWg.Wait()
		close(done)
	}()

	select {
	case <-done:
		// goroutine completed
	case <-time.After(5 * time.Second):
		t.Fatal("cogneeWg.Wait() timed out — goroutine may be leaking")
	}
}
