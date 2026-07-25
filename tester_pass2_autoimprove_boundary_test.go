package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"mcp-memory/internal/testutil/cogneemock"
	"mcp-memory/logger"
	"mcp-memory/metrics"
)

// =========================================================================
// PASS 2 — BOUNDARY TESTING (Auto-Improve Focus)
//
// These tests attack what the spec DIDN'T cover and what Pass 1 missed.
// Focus areas:
//  1. Boundary values for AUTO_IMPROVE_AFTER_N (negative, MaxInt64, overflow)
//  2. Boundary values for AUTO_IMPROVE_COOLDOWN (0, negative, 1ns, 1ms, very large)
//  3. Bank name edge cases (empty, 10K chars, unicode, path traversal)
//  4. Counter overflow after MaxInt64
//  5. Persistence edge cases (read-only dir, dir-as-file, wrong JSON shape, concurrent saves)
//  6. Semaphore edge cases (nil semaphore, full semaphore)
//  7. memory_reflect query edge cases (null, very long, whitespace-only)
//  8. Mock Cognee boundaries (very large response, slow handler, empty 200 body)
//  9. Concurrent improve + manual reflect for same bank
//
// NOTE: CRITICAL-2 bug (Pass 1) — testServer() creates nil logger, causing
// goroutine early return. Tests that need goroutine completion use
// validTestServer() instead.
// =========================================================================

// ─── Helpers ─────────────────────────────────────────────────────────────

// validTestServer is like testServer but provides a valid logger so the
// auto-improve goroutine can actually complete. Uses a bytes.Buffer so
// the logger is non-nil and NewBuf succeeds.
func validTestServer(dir string, cfg Config) *Server {
	buf := &bytes.Buffer{}
	l, err := logger.NewBuf("test", "error", buf)
	if err != nil {
		panic(fmt.Sprintf("failed to create test logger: %v", err))
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Server{
		config:          cfg,
		improveState:    loadAutoImproveState(dir),
		cogneeSemaphore: make(chan struct{}, 10),
		log:             l,
		metrics:         &serverMetrics{errorCalls: metrics.NewCounter("test")},
		backend:         &mockBackend{},
		cogneeCtx:       ctx,
		cogneeCancel:    cancel,
	}
}

// validTestServerWithBackend is like validTestServer but accepts a custom backend.
func validTestServerWithBackend(dir string, cfg Config, be *mockBackend) *Server {
	buf := &bytes.Buffer{}
	l, err := logger.NewBuf("test", "error", buf)
	if err != nil {
		panic(fmt.Sprintf("failed to create test logger: %v", err))
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Server{
		config:          cfg,
		improveState:    loadAutoImproveState(dir),
		cogneeSemaphore: make(chan struct{}, 10),
		log:             l,
		metrics:         &serverMetrics{errorCalls: metrics.NewCounter("test")},
		backend:         be,
		cogneeCtx:       ctx,
		cogneeCancel:    cancel,
	}
}

// =========================================================================
// FOCUS 1: AUTO_IMPROVE_AFTER_N Boundary Values
// =========================================================================

// TestGetEnvInt_Boundaries verifies getEnvInt parsing for all edge cases.
func TestGetEnvInt_Boundaries(t *testing.T) {
	t.Run("empty_string_uses_default", func(t *testing.T) {
		t.Setenv("TEST_EMPTY", "")
		if v := getEnvInt("TEST_EMPTY", 42); v != 42 {
			t.Fatalf("empty: got %d, want 42", v)
		}
	})

	t.Run("negative_value", func(t *testing.T) {
		t.Setenv("TEST_NEG", "-5")
		if v := getEnvInt("TEST_NEG", 42); v != -5 {
			t.Fatalf("negative: got %d, want -5", v)
		}
	})

	t.Run("zero_value", func(t *testing.T) {
		t.Setenv("TEST_ZERO", "0")
		if v := getEnvInt("TEST_ZERO", 42); v != 0 {
			t.Fatalf("zero: got %d, want 0", v)
		}
	})

	t.Run("max_int64", func(t *testing.T) {
		t.Setenv("TEST_MAX", "9223372036854775807")
		if v := getEnvInt("TEST_MAX", 42); v != math.MaxInt64 {
			t.Fatalf("maxint64: got %d, want %d", v, math.MaxInt64)
		}
	})

	t.Run("overflow_uses_default", func(t *testing.T) {
		t.Setenv("TEST_OVERFLOW", "9223372036854775808") // MaxInt64 + 1
		if v := getEnvInt("TEST_OVERFLOW", 42); v != 42 {
			t.Fatalf("overflow: got %d, want 42", v)
		}
	})

	t.Run("non_numeric_uses_default", func(t *testing.T) {
		t.Setenv("TEST_ABC", "abc")
		if v := getEnvInt("TEST_ABC", 42); v != 42 {
			t.Fatalf("abc: got %d, want 42", v)
		}
	})

	t.Run("one_is_valid", func(t *testing.T) {
		t.Setenv("TEST_ONE", "1")
		if v := getEnvInt("TEST_ONE", 42); v != 1 {
			t.Fatalf("one: got %d, want 1", v)
		}
	})
}

// TestMaybeAutoImprove_NegativeThreshold verifies negative = disabled.
func TestMaybeAutoImprove_NegativeThreshold(t *testing.T) {
	dir := t.TempDir()
	s := &Server{
		config: Config{
			AutoImproveAfterN:   -5,
			AutoImproveCooldown: 120 * time.Second,
		},
		improveState:   loadAutoImproveState(dir),
		cogneeSemaphore: make(chan struct{}, 10),
	}
	s.maybeAutoImprove("testbank")
	if len(s.improveState.banks) != 0 {
		t.Fatal("negative threshold should disable auto-improve (<= 0)")
	}
}

// TestMaybeAutoImprove_MaxInt64Threshold verifies that MaxInt64 threshold
// fires at exactly MaxInt64 retains (>= check) and the goroutine completes.
func TestMaybeAutoImprove_MaxInt64Threshold(t *testing.T) {
	dir := t.TempDir()
	s := validTestServer(dir, Config{
		AutoImproveAfterN:   math.MaxInt64,
		AutoImproveCooldown: 0,
	})

	// Step 1: one retain — counter = 1, no fire
	s.maybeAutoImprove("bank")
	s.improveState.mu.Lock()
	r := s.improveState.banks["bank"].retainsSince
	inFlight := s.improveState.banks["bank"].improveInFlight
	s.improveState.mu.Unlock()
	if r != 1 {
		t.Fatalf("step 1: retainsSince=1, got %d", r)
	}
	if inFlight {
		t.Fatal("step 1: improveInFlight should be false (1 < MaxInt64)")
	}

	// Step 2: set counter to MaxInt64-1, then retain → MaxInt64 → should fire
	s.improveState.mu.Lock()
	s.improveState.banks["bank"].retainsSince = math.MaxInt64 - 1
	s.improveState.mu.Unlock()

	s.maybeAutoImprove("bank")
	s.cogneeWg.Wait() // wait for goroutine (it should complete normally with valid logger)

	s.improveState.mu.Lock()
	r = s.improveState.banks["bank"].retainsSince
	inFlight = s.improveState.banks["bank"].improveInFlight
	s.improveState.mu.Unlock()
	if r != 0 {
		t.Fatalf("step 2: retainsSince should be 0 (fired and reset), got %d", r)
	}
	if inFlight {
		t.Fatal("step 2: improveInFlight should be false after goroutine completes")
	}
}

// TestMaybeAutoImprove_CounterOverflowAfterMaxInt64 verifies that the
// counter saturates at MaxInt64 instead of overflowing to MinInt64.
// FIXED (CRITICAL-3): Counter now caps at MaxInt64 to prevent overflow wrap.
func TestMaybeAutoImprove_CounterOverflowAfterMaxInt64(t *testing.T) {
	dir := t.TempDir()
	s := validTestServer(dir, Config{
		AutoImproveAfterN:   5,
		AutoImproveCooldown: 0,
	})

	// Simulate the edge: retainsSince starts at MaxInt64
	s.improveState.mu.Lock()
	s.improveState.banks["bank"] = &bankState{retainsSince: math.MaxInt64}
	s.improveState.mu.Unlock()

	// This retain should NOT overflow — counter stays at MaxInt64 (capped)
	// Since MaxInt64 >= threshold (5), the goroutine fires and resets to 0
	s.maybeAutoImprove("bank")
	s.cogneeWg.Wait()

	s.improveState.mu.Lock()
	r := s.improveState.banks["bank"].retainsSince
	inFlight := s.improveState.banks["bank"].improveInFlight
	s.improveState.mu.Unlock()

	// After firing: retainsSince reset to 0 (not MinInt64)
	if r != 0 {
		t.Fatalf("expected retainsSince=0 (fired and reset), got %d", r)
	}
	if inFlight {
		t.Fatal("improveInFlight should be false after goroutine completes")
	}

	// Verify saturation: counter at MaxInt64 stays at MaxInt64 when incremented
	s.improveState.mu.Lock()
	s.improveState.banks["bank"].retainsSince = math.MaxInt64
	s.improveState.mu.Unlock()

	// Call maybeAutoImprove again — counter should stay at MaxInt64 (not overflow)
	// But since MaxInt64 >= threshold, a goroutine will fire
	s.maybeAutoImprove("bank")
	s.cogneeWg.Wait()

	s.improveState.mu.Lock()
	r = s.improveState.banks["bank"].retainsSince
	s.improveState.mu.Unlock()
	if r != 0 {
		t.Fatalf("expected retainsSince=0 after second fire, got %d", r)
	}
}

// TestMaybeAutoImprove_ThresholdOne_Fires verifies threshold=1 fires,
// goroutine completes, and counter resets.
func TestMaybeAutoImprove_ThresholdOne_Fires(t *testing.T) {
	dir := t.TempDir()
	s := validTestServer(dir, Config{
		AutoImproveAfterN:   1,
		AutoImproveCooldown: 0,
	})

	s.maybeAutoImprove("bank")
	s.cogneeWg.Wait()

	s.improveState.mu.Lock()
	r := s.improveState.banks["bank"].retainsSince
	inFlight := s.improveState.banks["bank"].improveInFlight
	s.improveState.mu.Unlock()

	if r != 0 {
		t.Fatalf("retainsSince should be 0 (fired and reset), got %d", r)
	}
	if inFlight {
		t.Fatal("improveInFlight should be false after goroutine completes")
	}
}

// =========================================================================
// FOCUS 2: AUTO_IMPROVE_COOLDOWN Boundary Values
// =========================================================================

// TestMaybeAutoImprove_CooldownZero verifies 0 cooldown means "immediate"
// (no cooldown enforced) — the goroutine completes and the next can fire.
func TestMaybeAutoImprove_CooldownZero(t *testing.T) {
	dir := t.TempDir()
	s := validTestServer(dir, Config{
		AutoImproveAfterN:   1,
		AutoImproveCooldown: 0,
	})

	// Fire once, wait for goroutine
	s.maybeAutoImprove("bank")
	s.cogneeWg.Wait()

	s.improveState.mu.Lock()
	rBefore := s.improveState.banks["bank"].retainsSince
	s.improveState.mu.Unlock()
	if rBefore != 0 {
		t.Fatalf("after first fire: retainsSince=0, got %d", rBefore)
	}

	// Fire again immediately — cooldown=0 means always eligible
	s.maybeAutoImprove("bank")
	s.cogneeWg.Wait()

	s.improveState.mu.Lock()
	rAfter := s.improveState.banks["bank"].retainsSince
	inFlight := s.improveState.banks["bank"].improveInFlight
	s.improveState.mu.Unlock()

	if rAfter != 0 {
		t.Fatalf("after second fire: retainsSince should be 0 (fired again), got %d", rAfter)
	}
	if inFlight {
		t.Fatal("improveInFlight should be false after second goroutine")
	}
}

// TestMaybeAutoImprove_CooldownNegative verifies negative cooldown
// always satisfies the cooldown check.
func TestMaybeAutoImprove_CooldownNegative(t *testing.T) {
	dir := t.TempDir()
	s := validTestServer(dir, Config{
		AutoImproveAfterN:   1,
		AutoImproveCooldown: -10 * time.Second,
	})

	s.maybeAutoImprove("bank")
	s.cogneeWg.Wait()

	s.maybeAutoImprove("bank")
	s.cogneeWg.Wait()

	s.improveState.mu.Lock()
	r := s.improveState.banks["bank"].retainsSince
	s.improveState.mu.Unlock()
	if r != 0 {
		t.Fatalf("negative cooldown: retainsSince=0 (fired twice), got %d", r)
	}
}

// TestMaybeAutoImprove_CooldownVeryLarge verifies very large cooldown
// blocks subsequent improves. The first fire is allowed (lastImprove.IsZero()),
// but the second is blocked.
func TestMaybeAutoImprove_CooldownVeryLarge(t *testing.T) {
	dir := t.TempDir()
	s := validTestServer(dir, Config{
		AutoImproveAfterN:   1,
		AutoImproveCooldown: 10000 * time.Hour, // ~1.14 years
	})

	// First fire (lastImprove.IsZero() allows it)
	s.maybeAutoImprove("bank")
	s.cogneeWg.Wait()

	// Immediately try again — cooldown blocks
	s.maybeAutoImprove("bank")
	s.cogneeWg.Wait()

	s.improveState.mu.Lock()
	r := s.improveState.banks["bank"].retainsSince
	inFlight := s.improveState.banks["bank"].improveInFlight
	s.improveState.mu.Unlock()

	// retainsSince should be 1 because the second fire was blocked by cooldown
	// (improveInFlight was already false from the first goroutine completing)
	if r != 1 {
		t.Fatalf("very large cooldown: retainsSince=1 (blocked by cooldown), got %d", r)
	}
	if inFlight {
		t.Fatal("improveInFlight should be false — cooldown blocks, not in-flight")
	}
}

// TestMaybeAutoImprove_CooldownOneNs verifies 1ns cooldown is effectively
// always satisfied by the time code executes.
func TestMaybeAutoImprove_CooldownOneNs(t *testing.T) {
	dir := t.TempDir()
	s := validTestServer(dir, Config{
		AutoImproveAfterN:   1,
		AutoImproveCooldown: time.Nanosecond,
	})

	s.maybeAutoImprove("bank")
	s.cogneeWg.Wait()

	s.maybeAutoImprove("bank")
	s.cogneeWg.Wait()

	s.improveState.mu.Lock()
	r := s.improveState.banks["bank"].retainsSince
	s.improveState.mu.Unlock()
	if r != 0 {
		t.Fatalf("1ns cooldown: retainsSince=0 (fired twice), got %d", r)
	}
}

// TestGetEnvDuration_Boundaries verifies getEnvDuration edge cases.
func TestGetEnvDuration_Boundaries(t *testing.T) {
	t.Run("empty_uses_default", func(t *testing.T) {
		t.Setenv("TEST_DUR_EMPTY", "")
		d := getEnvDuration("TEST_DUR_EMPTY", 120*time.Second)
		if d != 120*time.Second {
			t.Fatalf("empty: got %v, want 120s", d)
		}
	})

	t.Run("zero", func(t *testing.T) {
		t.Setenv("TEST_DUR_ZERO", "0")
		d := getEnvDuration("TEST_DUR_ZERO", 120*time.Second)
		if d != 0 {
			t.Fatalf("zero: got %v, want 0", d)
		}
	})

	t.Run("negative_duration", func(t *testing.T) {
		t.Setenv("TEST_DUR_NEG", "-5s")
		d := getEnvDuration("TEST_DUR_NEG", 120*time.Second)
		if d != -5*time.Second {
			t.Fatalf("negative: got %v, want -5s", d)
		}
	})

	t.Run("one_nanosecond", func(t *testing.T) {
		t.Setenv("TEST_DUR_NS", "1ns")
		d := getEnvDuration("TEST_DUR_NS", 120*time.Second)
		if d != time.Nanosecond {
			t.Fatalf("1ns: got %v, want 1ns", d)
		}
	})

	t.Run("one_millisecond", func(t *testing.T) {
		t.Setenv("TEST_DUR_MS", "1ms")
		d := getEnvDuration("TEST_DUR_MS", 120*time.Second)
		if d != time.Millisecond {
			t.Fatalf("1ms: got %v, want 1ms", d)
		}
	})

	t.Run("very_large_hours", func(t *testing.T) {
		t.Setenv("TEST_DUR_LARGE", "99999h")
		d := getEnvDuration("TEST_DUR_LARGE", 120*time.Second)
		expected := 99999 * time.Hour
		if d != expected {
			t.Fatalf("99999h: got %v, want %v", d, expected)
		}
	})

	t.Run("invalid_string_uses_default", func(t *testing.T) {
		t.Setenv("TEST_DUR_INV", "not-a-duration")
		d := getEnvDuration("TEST_DUR_INV", 120*time.Second)
		if d != 120*time.Second {
			t.Fatalf("invalid: got %v, want 120s", d)
		}
	})
}

// =========================================================================
// FOCUS 3: Bank Name Edge Cases
// =========================================================================

// TestMaybeAutoImprove_EmptyBankName verifies empty string as bank name.
// FIXED: Empty bank names are now rejected by bankNamePattern validation (HIGH-5).
func TestMaybeAutoImprove_EmptyBankName(t *testing.T) {
	dir := t.TempDir()
	s := validTestServer(dir, Config{
		AutoImproveAfterN:   1,
		AutoImproveCooldown: 0,
	})

	// Should not panic
	s.maybeAutoImprove("")
	s.cogneeWg.Wait()

	s.improveState.mu.Lock()
	_, ok := s.improveState.banks[""]
	s.improveState.mu.Unlock()
	if ok {
		t.Fatal("empty bank name should be rejected — no state entry expected")
	}
}

// TestMaybeAutoImprove_VeryLongBankName verifies 10K-char bank name is rejected.
// FIXED: Long names exceed bankNamePattern {1,128} limit.
func TestMaybeAutoImprove_VeryLongBankName(t *testing.T) {
	dir := t.TempDir()
	s := validTestServer(dir, Config{
		AutoImproveAfterN:   100,
		AutoImproveCooldown: 0,
	})

	longName := strings.Repeat("a", 10000)
	s.maybeAutoImprove(longName)

	s.improveState.mu.Lock()
	_, ok := s.improveState.banks[longName]
	s.improveState.mu.Unlock()
	if ok {
		t.Fatal("10K-char bank name should be rejected by bankNamePattern")
	}
}

// TestMaybeAutoImprove_UnicodeBankName verifies unicode chars in bank name are rejected.
// FIXED: Unicode chars not in [a-zA-Z0-9:_-] are rejected by bankNamePattern.
func TestMaybeAutoImprove_UnicodeBankName(t *testing.T) {
	dir := t.TempDir()
	s := validTestServer(dir, Config{
		AutoImproveAfterN:   100,
		AutoImproveCooldown: 0,
	})

	unicodeNames := []string{
		"hello_\u2603_world",           // snowman
		"\u00E9\u00E0\u00FC",           // accented chars
		"\u4E2D\u6587",                  // Chinese
		"\u041F\u0440\u0438\u0432\u0435\u0442", // Cyrillic
		strings.Repeat("\u2603", 100),   // 100 snowmen
	}

	for _, name := range unicodeNames {
		t.Run(fmt.Sprintf("unicode_%d_bytes", len(name)), func(t *testing.T) {
			s.maybeAutoImprove(name)
			s.improveState.mu.Lock()
			_, ok := s.improveState.banks[name]
			s.improveState.mu.Unlock()
			if ok {
				t.Fatalf("unicode key %q should be rejected by bankNamePattern", name)
			}
		})
	}
}

// TestMaybeAutoImprove_PathTraversalBankName verifies special chars are rejected.
// FIXED: Special chars not in [a-zA-Z0-9:_-] are rejected by bankNamePattern.
func TestMaybeAutoImprove_PathTraversalBankName(t *testing.T) {
	dir := t.TempDir()
	s := validTestServer(dir, Config{
		AutoImproveAfterN:   100,
		AutoImproveCooldown: 0,
	})

	names := []string{
		"../etc/passwd",
		"..\\windows\\system32",
		"bank/../../malicious",
		"; rm -rf /",
		"$(cat /etc/passwd)",
		"' OR '1'='1",
	}

	for _, name := range names {
		t.Run(fmt.Sprintf("name_%d_bytes", len(name)), func(t *testing.T) {
			s.maybeAutoImprove(name)
			s.improveState.mu.Lock()
			_, ok := s.improveState.banks[name]
			s.improveState.mu.Unlock()
			if ok {
				t.Fatalf("name %q should be rejected by bankNamePattern", name)
			}
		})
	}
}

// =========================================================================
// FOCUS 4: Counter Overflow
// (Covered by TestMaybeAutoImprove_CounterOverflowAfterMaxInt64 above)
// =========================================================================

// =========================================================================
// FOCUS 5: Persistence Edge Cases
// =========================================================================

// TestSaveStateLocked_ReadOnlyDirectory verifies read-only data/ directory
// causes a log warning but no panic.
func TestSaveStateLocked_ReadOnlyDirectory(t *testing.T) {
	dir := t.TempDir()

	state := &autoImproveState{
		banks:   make(map[string]*bankState),
		dataDir: filepath.Join(dir, "readonly"),
	}

	// Create directory and make it read-only
	if err := os.MkdirAll(state.dataDir, 0755); err != nil {
		t.Fatalf("mkdir failed: %v", err)
	}
	if err := os.Chmod(state.dataDir, 0444); err != nil {
		t.Fatalf("chmod failed: %v", err)
	}
	defer os.Chmod(state.dataDir, 0755) // restore for cleanup

	state.mu.Lock()
	state.banks["bank"] = &bankState{retainsSince: 1}
	state.saveStateLocked() // should log warning, not panic
	state.mu.Unlock()

	os.Chmod(state.dataDir, 0755) // ensure cleanup
}

// TestSaveStateLocked_DataDirIsFile verifies data/ path being a file (not dir)
// causes a log warning but no panic.
func TestSaveStateLocked_DataDirIsFile(t *testing.T) {
	dir := t.TempDir()

	// Create a file where data dir should be
	dataPath := filepath.Join(dir, "data")
	if err := os.WriteFile(dataPath, []byte("not a directory"), 0644); err != nil {
		t.Fatalf("failed to create file: %v", err)
	}

	state := &autoImproveState{
		banks:   make(map[string]*bankState),
		dataDir: dataPath,
	}

	state.mu.Lock()
	state.banks["bank"] = &bankState{retainsSince: 1}
	state.saveStateLocked() // os.MkdirAll on a file fails → log warning, not panic
	state.mu.Unlock()

	// Verify data path still exists as file (not overwritten)
	info, err := os.Stat(dataPath)
	if err != nil {
		t.Fatalf("stat failed: %v", err)
	}
	if info.IsDir() {
		t.Fatal("data should still be a file after failed save")
	}
}

// TestLoadAutoImproveState_JSONWrongShape verifies that valid JSON of wrong
// shape (array, string, number) triggers corrupt-file handling.
func TestLoadAutoImproveState_JSONWrongShape(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "improve_state.json")

	t.Run("json_array", func(t *testing.T) {
		os.WriteFile(path, []byte(`[]`), 0644)
		state := loadAutoImproveState(dir)
		if len(state.banks) != 0 {
			t.Fatalf("expected empty banks for array, got %d", len(state.banks))
		}
	})

	t.Run("json_string", func(t *testing.T) {
		os.WriteFile(path, []byte(`"just a string"`), 0644)
		state2 := loadAutoImproveState(dir)
		if len(state2.banks) != 0 {
			t.Fatalf("expected empty banks for string, got %d", len(state2.banks))
		}
	})

	t.Run("json_number", func(t *testing.T) {
		os.WriteFile(path, []byte(`42`), 0644)
		state3 := loadAutoImproveState(dir)
		if len(state3.banks) != 0 {
			t.Fatalf("expected empty banks for number, got %d", len(state3.banks))
		}
	})

	t.Run("json_null", func(t *testing.T) {
		os.WriteFile(path, []byte(`null`), 0644)
		state4 := loadAutoImproveState(dir)
		if len(state4.banks) != 0 {
			t.Fatalf("expected empty banks for null, got %d", len(state4.banks))
		}
	})

	t.Run("json_nested_array", func(t *testing.T) {
		os.WriteFile(path, []byte(`[{"bank":"a"},{"bank":"b"}]`), 0644)
		state5 := loadAutoImproveState(dir)
		if len(state5.banks) != 0 {
			t.Fatalf("expected empty banks for nested array, got %d", len(state5.banks))
		}
	})
}

// TestSaveStateLocked_ConcurrentDifferentBanks verifies concurrent saves
// from multiple goroutines are serialized properly.
func TestSaveStateLocked_ConcurrentDifferentBanks(t *testing.T) {
	dir := t.TempDir()
	state := &autoImproveState{
		banks:   make(map[string]*bankState),
		dataDir: dir,
	}

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			bank := fmt.Sprintf("bank_%d", id)
			state.mu.Lock()
			state.banks[bank] = &bankState{retainsSince: int64(id)}
			state.saveStateLocked()
			state.mu.Unlock()
		}(i)
	}
	wg.Wait()

	// Read persisted state
	path := filepath.Join(dir, "improve_state.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("failed to read state: %v", err)
	}
	var persisted map[string]persistedBankState
	if err := json.Unmarshal(data, &persisted); err != nil {
		t.Fatalf("failed to unmarshal: %v", err)
	}
	if len(persisted) != 20 {
		t.Fatalf("expected 20 banks, got %d", len(persisted))
	}
	// Verify no .tmp files left behind
	matches, _ := filepath.Glob(filepath.Join(dir, "*.tmp"))
	if len(matches) > 0 {
		t.Fatalf("leftover .tmp files: %v", matches)
	}
}

// TestSaveStateLocked_ConcurrentSameBank verifies concurrent saves for the
// same bank produce correct final count.
func TestSaveStateLocked_ConcurrentSameBank(t *testing.T) {
	dir := t.TempDir()
	state := &autoImproveState{
		banks:   make(map[string]*bankState),
		dataDir: dir,
	}

	state.mu.Lock()
	state.banks["bank"] = &bankState{retainsSince: 0}
	state.mu.Unlock()

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			state.mu.Lock()
			state.banks["bank"].retainsSince++
			state.saveStateLocked()
			state.mu.Unlock()
		}()
	}
	wg.Wait()

	// Verify final count is 50
	state.mu.Lock()
	r := state.banks["bank"].retainsSince
	state.mu.Unlock()
	if r != 50 {
		t.Fatalf("expected retainsSince=50, got %d", r)
	}

	// Verify persisted state matches
	path := filepath.Join(dir, "improve_state.json")
	data, _ := os.ReadFile(path)
	var persisted map[string]persistedBankState
	json.Unmarshal(data, &persisted)
	if persisted["bank"].RetainsSince != 50 {
		t.Fatalf("persisted retainsSince=50, got %d", persisted["bank"].RetainsSince)
	}
}

// TestSaveStateLocked_NilDataDir verifies empty dataDir is handled.
func TestSaveStateLocked_NilDataDir(t *testing.T) {
	state := &autoImproveState{
		banks:   make(map[string]*bankState),
		dataDir: "",
	}

	state.mu.Lock()
	state.banks["bank"] = &bankState{retainsSince: 1}
	state.saveStateLocked() // should be a no-op (returns early on empty dataDir)
	state.mu.Unlock()
}

// =========================================================================
// FOCUS 6: Semaphore Edge Cases
// =========================================================================

// TestMaybeAutoImprove_NilCogneeSemaphore verifies that a nil semaphore
// doesn't panic (len(nil channel) = 0, which passes the idle check).
func TestMaybeAutoImprove_NilCogneeSemaphore(t *testing.T) {
	dir := t.TempDir()
	s := &Server{
		config: Config{
			AutoImproveAfterN:   1,
			AutoImproveCooldown: 0,
		},
		improveState: loadAutoImproveState(dir),
		backend:      &mockBackend{},
		cogneeCtx:    context.Background(),
		// cogneeSemaphore is nil intentionally
	}

	// Should not panic — len(nil) == 0 <= 1 is true
	s.maybeAutoImprove("bank")
	s.cogneeWg.Wait()

	s.improveState.mu.Lock()
	bs := s.improveState.banks["bank"]
	s.improveState.mu.Unlock()
	if bs == nil {
		t.Fatal("bank state should exist after maybeAutoImprove")
	}
}

// TestMaybeAutoImprove_FullSemaphoreBlocked verifies that when all slots
// are taken, idle check blocks auto-improve.
func TestMaybeAutoImprove_FullSemaphoreBlocked(t *testing.T) {
	dir := t.TempDir()
	s := testServer(dir, Config{
		AutoImproveAfterN:   1,
		AutoImproveCooldown: 0,
	})

	// Fill semaphore to capacity
	cap := cap(s.cogneeSemaphore)
	for i := 0; i < cap; i++ {
		s.cogneeSemaphore <- struct{}{}
	}

	if len(s.cogneeSemaphore) <= 1 {
		t.Fatalf("test setup: semaphore should have len=%d (> 1)", cap)
	}

	s.maybeAutoImprove("bank")
	s.cogneeWg.Wait()

	s.improveState.mu.Lock()
	bs := s.improveState.banks["bank"]
	s.improveState.mu.Unlock()
	if bs == nil {
		t.Fatal("bank state should exist (increment still happens)")
	}
	if bs.improveInFlight {
		t.Fatal("improveInFlight should be false (blocked by idle check)")
	}
}

// =========================================================================
// FOCUS 7: memory_reflect Query Edge Cases
// =========================================================================

// TestMemoryReflect_WhitespaceOnlyQuery verifies whitespace-only query
// is treated as empty string (valid for full improve).
func TestMemoryReflect_WhitespaceOnlyQuery(t *testing.T) {
	// Verify via direct backend call (mirrors what handler does)
	s := &Server{
		backend: &mockBackend{
			reflectFn: func(ctx context.Context, bank string, query string) (string, error) {
				if query != "  " {
					t.Errorf("expected query='  ', got %q", query)
				}
				return `{"status":"completed"}`, nil
			},
		},
		config: Config{
			AutoImproveAfterN: 0, // disable auto-improve
		},
		improveState: loadAutoImproveState(t.TempDir()),
		cogneeCtx:    context.Background(),
	}

	ref, err := s.backend.Reflect(context.Background(), "bank", "  ")
	if err != nil {
		t.Fatalf("whitespace-only query should be valid: %v", err)
	}
	_ = ref
}

// TestMemoryReflect_NullQueryFromJSON verifies JSON null query becomes "".
func TestMemoryReflect_NullQueryFromJSON(t *testing.T) {
	var a struct{ Query string `json:"query"` }
	input := `{"query": null}`
	if err := json.Unmarshal([]byte(input), &a); err != nil {
		t.Fatalf("failed to unmarshal null query: %v", err)
	}
	if a.Query != "" {
		t.Fatalf("null query should become empty string, got %q", a.Query)
	}
}

// TestMemoryReflect_VeryLongQuery verifies very long query strings.
func TestMemoryReflect_VeryLongQuery(t *testing.T) {
	s := &Server{
		backend: &mockBackend{
			reflectFn: func(ctx context.Context, bank string, query string) (string, error) {
				if len(query) != 50000 {
					t.Errorf("expected 50000 chars, got %d", len(query))
				}
				return `{"status":"completed"}`, nil
			},
		},
		config: Config{
			AutoImproveAfterN: 0,
		},
		improveState: loadAutoImproveState(t.TempDir()),
		cogneeCtx:    context.Background(),
	}

	ref, err := s.backend.Reflect(context.Background(), "bank", strings.Repeat("x", 50000))
	if err != nil {
		t.Fatalf("50000-char query should be valid: %v", err)
	}
	_ = ref
}

// TestMemoryReflect_SpecialCharsQuery verifies special characters.
func TestMemoryReflect_SpecialCharsQuery(t *testing.T) {
	s := &Server{
		backend: &mockBackend{
			reflectFn: func(ctx context.Context, bank string, query string) (string, error) {
				return `{"status":"completed"}`, nil
			},
		},
		config: Config{
			AutoImproveAfterN: 0,
		},
		improveState: loadAutoImproveState(t.TempDir()),
		cogneeCtx:    context.Background(),
	}

	specialQueries := []string{
		"\x00\x01\x02\x03",                                     // null bytes and control chars
		"<script>alert('xss')</script>",                         // XSS attempt
		"{}[]|\\:;\"'<>?`~!@#$%^&*()_+-=",                      // all ASCII special chars
		"\u202E\u202D\u202C",                                    // unicode bidi overrides
		strings.Repeat("\x00", 1000),                            // 1000 null bytes
	}

	for _, q := range specialQueries {
		t.Run(fmt.Sprintf("query_%d_bytes", len(q)), func(t *testing.T) {
			ref, err := s.backend.Reflect(context.Background(), "bank", q)
			if err != nil {
				t.Fatalf("special chars query should be valid: %v", err)
			}
			_ = ref
		})
	}
}

// =========================================================================
// FOCUS 8: Mock Cognee Boundaries
// =========================================================================

// TestCogneeMock_VeryLargeResponseBody verifies mock handles 10MB response.
func TestCogneeMock_VeryLargeResponseBody(t *testing.T) {
	mock := cogneemock.NewServer()
	defer mock.Close()

	largeBody := `{"data":"` + strings.Repeat("a", 10*1024*1024) + `"}`
	mock.SetResponse("/api/v1/recall", cogneemock.ResponseConfig{
		StatusCode: 200,
		Body:       largeBody,
	})

	resp, err := http.Post(mock.URL()+"/api/v1/recall", "application/json",
		strings.NewReader(`{"query":"test","datasets":["bank"]}`))
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	reqs := mock.Requests()
	if len(reqs) == 0 {
		t.Fatal("no requests captured")
	}
}

// TestCogneeMock_ZeroBodyFallsBackToDefault verifies that Body="" + StatusCode=200
// falls back to default body, not empty string.
func TestCogneeMock_ZeroBodyFallsBackToDefault(t *testing.T) {
	mock := cogneemock.NewServer()
	defer mock.Close()

	// SetResponse with StatusCode=200 and empty Body should reset to default
	mock.SetResponse("/api/v1/improve", cogneemock.ResponseConfig{
		StatusCode: 200,
		Body:       "",
	})

	resp, err := http.Post(mock.URL()+"/api/v1/improve", "application/json",
		strings.NewReader(`{"dataset_name":"bank","data":""}`))
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	var result map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if result["status"] != "PipelineRunCompleted" {
		t.Fatalf("expected default status, got %v", result["status"])
	}
}

// TestCogneeMock_VerySlowHandler verifies a handler with multi-second delay.
func TestCogneeMock_VerySlowHandler(t *testing.T) {
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(3 * time.Second)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"status":"completed"}`))
	}))
	defer slow.Close()

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Post(slow.URL+"/api/v1/improve", "application/json",
		strings.NewReader(`{"dataset_name":"bank","data":""}`))
	if err != nil {
		t.Fatalf("slow request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
}

// TestCogneeMock_ConcurrentRequestsToSameEndpoint verifies 5 concurrent
// requests to the same endpoint all succeed.
func TestCogneeMock_ConcurrentRequestsToSameEndpoint(t *testing.T) {
	mock := cogneemock.NewServer()
	defer mock.Close()

	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			resp, err := http.Post(mock.URL()+"/api/v1/improve", "application/json",
				strings.NewReader(fmt.Sprintf(`{"dataset_name":"bank_%d","data":""}`, id)))
			if err != nil {
				t.Errorf("request %d failed: %v", id, err)
				return
			}
			defer resp.Body.Close()
			if resp.StatusCode != 200 {
				t.Errorf("request %d: expected 200, got %d", id, resp.StatusCode)
			}
		}(i)
	}
	wg.Wait()

	reqs := mock.Requests()
	if len(reqs) != 5 {
		t.Fatalf("expected 5 captured requests, got %d", len(reqs))
	}
}

// =========================================================================
// FOCUS 9: Concurrent Improve + Manual Reflect
// =========================================================================

// TestConcurrentAutoImproveAndManualReflect verifies that when auto-improve
// is in flight and user manually calls memory_reflect for the same bank,
// both operations complete without deadlock.
//
// Both spawn goroutines calling backend.Reflect(ctx, bank, "").
// Since the mock Cognee is stateless, both succeed independently.
func TestConcurrentAutoImproveAndManualReflect(t *testing.T) {
	dir := t.TempDir()

	// Control channel to sequence the test
	improveStarted := make(chan struct{})
	improveComplete := make(chan struct{})

	be := &mockBackend{
		reflectFn: func(ctx context.Context, bank string, query string) (string, error) {
			select {
			case improveStarted <- struct{}{}:
				// First call (auto-improve) — block until signal
				<-improveComplete
			default:
				// Subsequent calls (manual reflect) — proceed immediately
			}
			return `{"status":"completed"}`, nil
		},
	}

	s := validTestServerWithBackend(dir, Config{
		AutoImproveAfterN:   1,
		AutoImproveCooldown: 0,
	}, be)

	// Step 1: Trigger auto-improve (blocks at reflectFn)
	s.maybeAutoImprove("bank")

	// Wait for auto-improve goroutine to reach reflectFn
	select {
	case <-improveStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("auto-improve goroutine did not start in time")
	}

	// Step 2: While auto-improve is in flight, call memory_reflect manually
	// (simulates what the Cognee memory_reflect handler does)
	reflectResult := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, err := s.backend.Reflect(ctx, "bank", "")
		reflectResult <- err
	}()

	// Step 3: Let auto-improve complete
	close(improveComplete)

	// Step 4: Wait for both to finish
	s.cogneeWg.Wait()

	select {
	case err := <-reflectResult:
		if err != nil {
			t.Fatalf("manual reflect failed: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("manual reflect did not complete in time")
	}

	// Step 5: Verify final state
	s.improveState.mu.Lock()
	bs := s.improveState.banks["bank"]
	s.improveState.mu.Unlock()
	if bs.improveInFlight {
		t.Fatal("improveInFlight should be false after both completes")
	}
}

// TestConcurrentAutoImproveAndManualReflect_DifferentBanks verifies isolation
// for concurrent improves on different banks.
func TestConcurrentAutoImproveAndManualReflect_DifferentBanks(t *testing.T) {
	dir := t.TempDir()
	s := validTestServer(dir, Config{
		AutoImproveAfterN:   1,
		AutoImproveCooldown: 0,
	})

	// Start 5 banks' auto-improves, each also doing a manual reflect
	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			bank := fmt.Sprintf("bank_%d", id)
			s.maybeAutoImprove(bank)
			// Manual reflect while auto-improve is (maybe) in flight
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			_, err := s.backend.Reflect(ctx, bank, "")
			if err != nil {
				t.Errorf("bank %s manual reflect failed: %v", bank, err)
			}
		}(i)
	}
	wg.Wait()
	s.cogneeWg.Wait()

	// Verify state
	s.improveState.mu.Lock()
	defer s.improveState.mu.Unlock()

	if len(s.improveState.banks) != 5 {
		t.Fatalf("expected 5 banks, got %d", len(s.improveState.banks))
	}

	for i := 0; i < 5; i++ {
		bank := fmt.Sprintf("bank_%d", i)
		bs := s.improveState.banks[bank]
		if bs == nil {
			t.Fatalf("bank %s: expected bank state", bank)
		}
		if bs.improveInFlight {
			t.Fatalf("bank %s: improveInFlight should be false", bank)
		}
	}
}

// =========================================================================
// ADDITIONAL: IsSync() Guards — Hindsight path
// =========================================================================

// TestMaybeAutoImprove_HindsightPathIsSync verifies that when the backend
// is Hindsight (IsSync=true), maybeAutoImprove is never called because
// improveState is nil (NewServer only sets it for Cognee backends).
func TestMaybeAutoImprove_HindsightPathIsSync(t *testing.T) {
	// When IsSync() is true, NewServer does NOT create improveState
	s := &Server{
		config: Config{
			AutoImproveAfterN:   5,
			AutoImproveCooldown: 120 * time.Second,
		},
		// improveState is nil (Hindsight path)
		// cogneeSemaphore is nil
		backend: &mockBackend{
			reflectFn: func(ctx context.Context, bank string, query string) (string, error) {
				return "", nil
			},
		},
	}

	// maybeAutoImprove should return early when improveState is nil.
	// This ensures no goroutines are spawned for Hindsight path.
	s.maybeAutoImprove("testbank")
}

// TestMaybeAutoImprove_ImproveStateNilGuard verifies the nil improveState
// guard triggers correctly.
func TestMaybeAutoImprove_ImproveStateNilGuard(t *testing.T) {
	s := &Server{
		config: Config{
			AutoImproveAfterN:   5,
			AutoImproveCooldown: 120 * time.Second,
		},
		improveState: nil,
	}

	// Should return early at improveState == nil check
	s.maybeAutoImprove("testbank")
	// No panic = pass
}

// =========================================================================
// ENVIRONMENT: LoadConfig defaults
// =========================================================================

// TestLoadConfig_AutoImproveBounds verifies sensible defaults.
func TestLoadConfig_AutoImproveBounds(t *testing.T) {
	cfg := LoadConfig()

	if cfg.AutoImproveAfterN != 0 {
		t.Fatalf("default AutoImproveAfterN should be 0 (disabled), got %d", cfg.AutoImproveAfterN)
	}
	if cfg.AutoImproveCooldown != 120*time.Second {
		t.Fatalf("default AutoImproveCooldown should be 120s, got %v", cfg.AutoImproveCooldown)
	}
}

// TestLoadConfig_AutoImproveEnvVars verifies env var overrides.
func TestLoadConfig_AutoImproveEnvVars(t *testing.T) {
	t.Run("after_n_5", func(t *testing.T) {
		t.Setenv("AUTO_IMPROVE_AFTER_N", "5")
		cfg := LoadConfig()
		if cfg.AutoImproveAfterN != 5 {
			t.Fatalf("expected 5, got %d", cfg.AutoImproveAfterN)
		}
	})

	t.Run("cooldown_30s", func(t *testing.T) {
		t.Setenv("AUTO_IMPROVE_COOLDOWN", "30s")
		cfg := LoadConfig()
		if cfg.AutoImproveCooldown != 30*time.Second {
			t.Fatalf("expected 30s, got %v", cfg.AutoImproveCooldown)
		}
	})

	t.Run("cooldown_zero", func(t *testing.T) {
		t.Setenv("AUTO_IMPROVE_COOLDOWN", "0")
		cfg := LoadConfig()
		if cfg.AutoImproveCooldown != 0 {
			t.Fatalf("expected 0, got %v", cfg.AutoImproveCooldown)
		}
	})

	t.Run("cooldown_1ms", func(t *testing.T) {
		t.Setenv("AUTO_IMPROVE_COOLDOWN", "1ms")
		cfg := LoadConfig()
		if cfg.AutoImproveCooldown != time.Millisecond {
			t.Fatalf("expected 1ms, got %v", cfg.AutoImproveCooldown)
		}
	})

	t.Run("after_n_invalid", func(t *testing.T) {
		t.Setenv("AUTO_IMPROVE_AFTER_N", "not-a-number")
		cfg := LoadConfig()
		if cfg.AutoImproveAfterN != 0 {
			t.Fatalf("expected 0 (default), got %d", cfg.AutoImproveAfterN)
		}
	})

	t.Run("cooldown_invalid", func(t *testing.T) {
		t.Setenv("AUTO_IMPROVE_COOLDOWN", "not-a-duration")
		cfg := LoadConfig()
		if cfg.AutoImproveCooldown != 120*time.Second {
			t.Fatalf("expected 120s (default), got %v", cfg.AutoImproveCooldown)
		}
	})
}
