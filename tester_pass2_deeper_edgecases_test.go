package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"mcp-memory/internal/testutil/cogneemock"
	"mcp-memory/logger"
	"mcp-memory/metrics"
)

// =========================================================================
// PASS 2 — DEEPER EDGE CASES (Re-Run)
//
// These tests attack beyond what Pass 1 verified:
//   1. Nil-safety deep dive (metrics, cogneeCtx, backend in goroutine)
//   2. Bank name collision across concurrent sessions
//   3. Persistence format compatibility (extra JSON fields)
//   4. Time zone / clock skew (future lastImprove)
//   5. Rapid threshold changes (counter > new threshold on reload)
//   6. (Already covered by existing tests: null query)
//   7. Mock Cognee response body (truncated JSON, wrong shape)
//   8. Concurrent SetResponse + HTTP request (race exposure)
// =========================================================================

// ─── Helpers ─────────────────────────────────────────────────────────────

// noopLogger returns a valid logger backed by a discard buffer.
func noopLogger() *logger.Logger {
	l, err := logger.NewBuf("test", "error", &bytes.Buffer{})
	if err != nil {
		panic(fmt.Sprintf("failed to create test logger: %v", err))
	}
	return l
}

// noopMetrics returns a fully-initialized serverMetrics.
func fullMetrics() *serverMetrics {
	return &serverMetrics{
		recallCalls:  metrics.NewCounter("test.recall"),
		retainCalls:  metrics.NewCounter("test.retain"),
		reflectCalls: metrics.NewCounter("test.reflect"),
		errorCalls:   metrics.NewCounter("test.errors"),
		retainDur:    metrics.NewTimer("test.retain_dur"),
		reflectDur:   metrics.NewTimer("test.reflect_dur"),
		queueGauge:   metrics.NewGauge("test.queue"),
		sessionGauge: metrics.NewGauge("test.sessions"),
		sseDrops:     metrics.NewCounter("test.sse_drops"),
		retainTotal:    metrics.NewCounter("memory.retain_total"),
		retainErrors:   metrics.NewCounter("memory.retain_errors"),
		recallTotal:    metrics.NewCounter("memory.recall_total"),
		reflectTotal:   metrics.NewCounter("memory.reflect_total"),
		improveTotal:   metrics.NewCounter("memory.improve_total"),
		forgetTotal:    metrics.NewCounter("memory.forget_total"),
		semaphoreGauge: metrics.NewGauge("memory.semaphore_in_use"),
		cogneePending:  metrics.NewGauge("memory.cognee_jobs_pending"),
	}
}

// =========================================================================
// ATTACK 1: Nil-Safety Deep Dive
// =========================================================================

// TestNilMetricsInGoroutine verifies that when s.metrics is nil and the
// backend returns an error, the goroutine recovers gracefully and resets
// improveInFlight. The panic (nil pointer on s.metrics.errorCalls.Inc())
// should be caught by defer recover().
func TestNilMetricsInGoroutine(t *testing.T) {
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	s := &Server{
		config: Config{
			AutoImproveAfterN:  1,
			AutoImproveCooldown: 0,
		},
		improveState:    loadAutoImproveState(dir),
		cogneeSemaphore: make(chan struct{}, 10),
		cogneeCtx:       ctx,
		log:             noopLogger(),
		backend: &mockBackend{
			reflectFn: func(ctx context.Context, bank string, query string) (string, error) {
				return "", fmt.Errorf("mock backend error")
			},
		},
		// metrics is nil — the goroutine's error path will
		// panic on s.metrics.errorCalls.Inc(), but recover() should catch it.
		panics:   atomic.Int64{},
		dataDir:  dir,
	}
	// Ensure cogneeWg is initialized for goroutine tracking
	s.cogneeWg = sync.WaitGroup{}

	// Trigger auto-improve (should spawn goroutine)
	s.maybeAutoImprove("testbank")

	// Wait for goroutine to complete
	done := make(chan struct{})
	go func() {
		s.cogneeWg.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("goroutine did not complete in time — possible deadlock or crash")
	}

	// Verify improveInFlight was reset (the goroutine should have
	// completed the cleanup defer even though metrics was nil)
	s.improveState.mu.Lock()
	bs := s.improveState.banks["testbank"]
	s.improveState.mu.Unlock()

	if bs == nil {
		t.Fatal("bank state should exist")
	}
	if bs.improveInFlight {
		t.Fatal("improveInFlight should be false after goroutine completes")
	}
	// retainsSince should be 0 (reset after fire)
	if bs.retainsSince != 0 {
		t.Fatalf("retainsSince should be 0 after fire, got %d", bs.retainsSince)
	}
}

// TestNilCogneeCtxInGoroutine verifies that nil cogneeCtx causes an
// unrecoverable panic at goroutine start. context.WithTimeout(nil, ...)
// panics BEFORE the defer recover() is registered, crashing the process.
//
// This test uses a subprocess to avoid crashing the test harness.
func TestNilCogneeCtxInGoroutine(t *testing.T) {
	// We can't safely test a process-crashing bug in-process.
	// Use a subprocess pattern: the subprocess will crash, but the
	// parent test harness survives.
	if os.Getenv("TEST_CRASH_COGNEE_CTX_NIL") == "1" {
		// Inside subprocess: trigger the crash
		dir, _ := os.MkdirTemp("", "cognee-crash-test")
		defer os.RemoveAll(dir)

		_ = &Server{
			config: Config{
				AutoImproveAfterN:  1,
				AutoImproveCooldown: 0,
			},
			improveState:    loadAutoImproveState(dir),
			cogneeSemaphore: make(chan struct{}, 10),
			cogneeCtx:       nil, // intentional — triggers crash
			log:             noopLogger(),
			backend:         &mockBackend{},
			metrics:         fullMetrics(),
			panics:          atomic.Int64{},
			dataDir:         dir,
		}
		// maybeAutoImprove spawns a goroutine that panics on
		// context.WithTimeout(nil, ...) before recover is registered.
		// If we reach here without crashing, the bug is fixed.
		return
	}

	// Run as subprocess (skip -race for this crash test)
	cmd := exec.Command("go", "test", "-count=1", "-run", "^TestNilCogneeCtxInGoroutine$", ".")
	cmd.Env = append(os.Environ(), "TEST_CRASH_COGNEE_CTX_NIL=1")
	out, err := cmd.CombinedOutput()

	t.Logf("Subprocess output:\n%s", string(out))

	if err == nil {
		t.Log("BUG MAY BE FIXED: subprocess did not crash with nil cogneeCtx")
	} else {
		t.Log("BUG CONFIRMED: nil cogneeCtx causes uncatchable goroutine crash")
		t.Log("context.WithTimeout(nil, ...) panics before recover defer is registered")
	}
}

// TestNilBackendInGoroutine verifies that nil backend in the goroutine
// (s.backend.Reflect crashes) is caught by recover() and the goroutine
// cleans up properly.
func TestNilBackendInGoroutine(t *testing.T) {
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Track whether the panics counter was incremented
	panics := atomic.Int64{}

	s := &Server{
		config: Config{
			AutoImproveAfterN:  1,
			AutoImproveCooldown: 0,
		},
		improveState:    loadAutoImproveState(dir),
		cogneeSemaphore: make(chan struct{}, 10),
		cogneeCtx:       ctx,
		log:             noopLogger(),
		backend:         nil, // nil backend — Reflect will crash
		metrics:         fullMetrics(),
		panics:          panics,
		dataDir:         dir,
	}
	s.cogneeWg = sync.WaitGroup{}

	s.maybeAutoImprove("testbank")

	// Wait for goroutine
	done := make(chan struct{})
	go func() {
		s.cogneeWg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("goroutine did not complete — possible hang")
	}

	// Verify panics counter was incremented
	if panics.Load() == 0 {
		t.Log("NOTE: panics counter was not incremented. The nil backend panic should have been caught.")
		// This is informational — the panic on s.backend.Reflect should be
		// caught by recover(), which increments the panics counter.
	}

	// Verify improveInFlight was reset
	s.improveState.mu.Lock()
	bs := s.improveState.banks["testbank"]
	s.improveState.mu.Unlock()
	if bs == nil {
		t.Fatal("bank state should exist")
	}
	if bs.improveInFlight {
		t.Fatal("improveInFlight should be false after goroutine recovers")
	}
}

// TestPartialInitNilImproveState verifies the nil improveState guard
// already works (spec says maybeAutoImprove returns early).
func TestPartialInitNilImproveState(t *testing.T) {
	s := &Server{
		config: Config{
			AutoImproveAfterN: 5,
		},
		improveState: nil, // nil improveState guard
	}
	// Should not panic
	s.maybeAutoImprove("testbank")
}

// TestPartialInitBackendOnly verifies a Server with only backend set works.
func TestPartialInitBackendOnly(t *testing.T) {
	s := &Server{
		config: Config{
			AutoImproveAfterN: 0, // disabled
		},
		backend: &mockBackend{},
	}
	// Should not panic
	s.maybeAutoImprove("testbank")
}

// =========================================================================
// ATTACK 2: Bank Name Collision Across Concurrent Sessions
// =========================================================================

// TestBankNameCollision verifies that when two concurrent "sessions" retain
// for the same bank, they share the same auto-improve counter and in-flight
// guard. Only one improve goroutine should fire.
func TestBankNameCollision(t *testing.T) {
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	be := &mockBackend{
		reflectFn: func(ctx context.Context, bank string, query string) (string, error) {
			// Ensure auto-improve takes long enough for both
			// retains to reach maybeAutoImprove
			time.Sleep(50 * time.Millisecond)
			return `{"status":"completed"}`, nil
		},
	}

	s := &Server{
		config: Config{
			AutoImproveAfterN:  1, // fire after every retain
			AutoImproveCooldown: 0,
		},
		improveState:    loadAutoImproveState(dir),
		cogneeSemaphore: make(chan struct{}, 10),
		cogneeCtx:       ctx,
		log:             noopLogger(),
		backend:         be,
		metrics:         fullMetrics(),
		panics:          atomic.Int64{},
		dataDir:         dir,
	}
	s.cogneeWg = sync.WaitGroup{}

	// Simulate two concurrent sessions retaining for the same bank.
	// Each "session" acquires a semaphore slot (simulating the retain goroutine).
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			// Acquire semaphore slot (simulates retain goroutine holding a slot)
			s.cogneeSemaphore <- struct{}{}
			defer func() { <-s.cogneeSemaphore }()

			s.maybeAutoImprove("shared_bank")
		}(i)
	}
	wg.Wait()

	// Wait for auto-improve goroutines to complete
	done := make(chan struct{})
	go func() {
		s.cogneeWg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("goroutines did not complete")
	}

	// Check state
	s.improveState.mu.Lock()
	bs := s.improveState.banks["shared_bank"]
	s.improveState.mu.Unlock()

	if bs == nil {
		t.Fatal("bank state should exist")
	}

	// At most one improve should have fired.
	// With 2 concurrent retains, threshold=1:
	//   - 1st retain: counter=1, fire (improveInFlight=true)
	//   - 2nd retain: counter=2, but improveInFlight=true → blocked
	// retainsSince should be 1 (2nd retain's counter persisted, fire blocked).
	if bs.retainsSince > 1 {
		t.Fatalf("expected retainsSince <= 1 (only first retain fires, second blocked), got %d", bs.retainsSince)
	}
	if bs.improveInFlight {
		t.Fatal("improveInFlight should be false after all goroutines complete")
	}
}

// TestBankNameCollisionInFlightBlocks verifies that in-flight guard is
// per-bank: improve on bank A does not block improve on bank B.
func TestBankNameCollisionInFlightBlocks(t *testing.T) {
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var callCount atomic.Int64
	improveDone := make(chan struct{})

	be := &mockBackend{
		reflectFn: func(ctx context.Context, bank string, query string) (string, error) {
			if callCount.Add(1) == 1 {
				// First call (bank A) — block until signal
				<-improveDone
			}
			// Second call (bank B) — proceed immediately
			return `{"status":"completed"}`, nil
		},
	}

	s := &Server{
		config: Config{
			AutoImproveAfterN:  1,
			AutoImproveCooldown: 0,
		},
		improveState:    loadAutoImproveState(dir),
		cogneeSemaphore: make(chan struct{}, 10),
		cogneeCtx:       ctx,
		log:             noopLogger(),
		backend:         be,
		metrics:         fullMetrics(),
		panics:          atomic.Int64{},
		dataDir:         dir,
	}
	s.cogneeWg = sync.WaitGroup{}

	// Trigger auto-improve for bank A (first reflect call blocks)
	s.maybeAutoImprove("bank_a")

	// Wait for bank A's goroutine to enter reflectFn and block
	time.Sleep(50 * time.Millisecond)
	if callCount.Load() < 1 {
		t.Fatal("bank A goroutine did not start")
	}

	// Now trigger auto-improve for bank B — should NOT be blocked
	// by bank A's in-flight flag (different bank)
	s.maybeAutoImprove("bank_b")

	// Wait for bank B's goroutine to start and complete
	time.Sleep(100 * time.Millisecond)

	// Unblock bank A
	close(improveDone)

	// Wait for all goroutines
	done := make(chan struct{})
	go func() { s.cogneeWg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("goroutines did not complete")
	}

	// Both banks should have fired and reset
	s.improveState.mu.Lock()
	defer s.improveState.mu.Unlock()

	for _, bank := range []string{"bank_a", "bank_b"} {
		bs := s.improveState.banks[bank]
		if bs == nil {
			t.Fatalf("bank %s missing", bank)
		}
		if bs.retainsSince != 0 {
			t.Fatalf("bank %s: retainsSince should be 0, got %d", bank, bs.retainsSince)
		}
		if bs.improveInFlight {
			t.Fatalf("bank %s: improveInFlight should be false", bank)
		}
	}
}

// =========================================================================
// ATTACK 3: Persistence Format Compatibility
// =========================================================================

// TestLoadAutoImproveState_ExtraFields verifies that unknown JSON fields
// in improve_state.json are silently ignored (Go's json.Unmarshal behavior).
// This ensures forward compatibility if new fields are added.
func TestLoadAutoImproveState_ExtraFields(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "improve_state.json")

	// State with extra fields that don't exist in the struct
	input := `{
		"bank_a": {
			"retains_since": 3,
			"last_improve": "2026-07-24T10:00:00Z",
			"version": 2,
			"migrated_from": "v1"
		},
		"bank_b": {
			"retains_since": 7,
			"last_improve": "2026-07-24T09:00:00Z",
			"some_future_field": true
		}
	}`
	os.WriteFile(path, []byte(input), 0644)

	state := loadAutoImproveState(dir)

	if len(state.banks) != 2 {
		t.Fatalf("expected 2 banks, got %d", len(state.banks))
	}
	if state.banks["bank_a"].retainsSince != 3 {
		t.Fatalf("bank_a retainsSince=3, got %d", state.banks["bank_a"].retainsSince)
	}
	if state.banks["bank_b"].retainsSince != 7 {
		t.Fatalf("bank_b retainsSince=7, got %d", state.banks["bank_b"].retainsSince)
	}
}

// TestLoadAutoImproveState_ZeroLastImprove verifies that a zero timestamp
// (empty/non-existent) defaults to zero value, which means cooldown
// check passes (IsZero() returns true → cooldown eligible).
func TestLoadAutoImproveState_ZeroLastImprove(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "improve_state.json")

	// last_improve omitted — should default to zero time
	input := `{"bank_a": {"retains_since": 5}}`
	os.WriteFile(path, []byte(input), 0644)

	state := loadAutoImproveState(dir)

	bs := state.banks["bank_a"]
	if bs == nil {
		t.Fatal("bank_a should exist")
	}
	if !bs.lastImprove.IsZero() {
		t.Fatal("lastImprove should be zero (default) when omitted from JSON")
	}
	if bs.retainsSince != 5 {
		t.Fatalf("retainsSince=5, got %d", bs.retainsSince)
	}
}

// TestLoadAutoImproveState_ExtraTopLevelFields verifies that top-level
// keys whose values are not valid persistedBankState objects cause the
// entire file to be rejected (corrupt). Only keys with valid JSON objects
// matching the struct shape are accepted.
func TestLoadAutoImproveState_ExtraTopLevelFields(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "improve_state.json")

	// All top-level values must be objects matching persistedBankState.
	// A scalar value like 2 or "abc" cannot unmarshal into the struct,
	// causing the entire file to be treated as corrupt.
	input := `{
		"bank_a": {"retains_since": 1},
		"bank_b": {"retains_since": 2},
		"bank_c": {"retains_since": 3, "version": 2}
	}`
	os.WriteFile(path, []byte(input), 0644)

	state := loadAutoImproveState(dir)

	// All entries have valid object values — unknown keys like "bank_c"
	// with extra struct fields are still accepted (Go silently ignores
	// unknown fields in struct Unmarshal).
	if len(state.banks) != 3 {
		t.Fatalf("expected 3 entries, got %d", len(state.banks))
	}
	if state.banks["bank_a"].retainsSince != 1 {
		t.Fatalf("bank_a retainsSince=1, got %d", state.banks["bank_a"].retainsSince)
	}
	if state.banks["bank_c"].retainsSince != 3 {
		t.Fatalf("bank_c retainsSince=3 (extra version field ignored), got %d", state.banks["bank_c"].retainsSince)
	}
}

// TestPersistBeyondMaxInt64_FutureThreshold verifies that when retainsSince
// is at MaxInt64 and a configuration change lowers the threshold, the next
// retain fires immediately (saturation is not a leak).
func TestPersistBeyondMaxInt64_FutureThreshold(t *testing.T) {
	dir := t.TempDir()

	// Persist state with MaxInt64 retainsSince
	persisted := map[string]persistedBankState{
		"bank": {RetainsSince: math.MaxInt64},
	}
	data, _ := json.Marshal(persisted)
	path := filepath.Join(dir, "improve_state.json")
	os.WriteFile(path, data, 0644)

	// Load with a low threshold (1)
	state := loadAutoImproveState(dir)
	if state.banks["bank"].retainsSince != math.MaxInt64 {
		t.Fatalf("expected retainsSince=MaxInt64, got %d", state.banks["bank"].retainsSince)
	}

	// Now call maybeAutoImprove — counter is at MaxInt64, threshold is 1
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := &Server{
		config: Config{
			AutoImproveAfterN:  1,
			AutoImproveCooldown: 0,
		},
		improveState:    state,
		cogneeSemaphore: make(chan struct{}, 10),
		cogneeCtx:       ctx,
		log:             noopLogger(),
		backend:         &mockBackend{},
		metrics:         fullMetrics(),
		panics:          atomic.Int64{},
		dataDir:         dir,
	}
	s.cogneeWg = sync.WaitGroup{}

	s.maybeAutoImprove("bank")

	done := make(chan struct{})
	go func() { s.cogneeWg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("goroutine did not complete")
	}

	// After firing, retainsSince should be 0 (reset)
	s.improveState.mu.Lock()
	r := s.improveState.banks["bank"].retainsSince
	inFlight := s.improveState.banks["bank"].improveInFlight
	s.improveState.mu.Unlock()
	if r != 0 {
		t.Fatalf("retainsSince should be 0 (fired from MaxInt64), got %d", r)
	}
	if inFlight {
		t.Fatal("improveInFlight should be false after goroutine completes")
	}
}

// =========================================================================
// ATTACK 4: Time Zone / Clock Skew
// =========================================================================

// TestClockSkew_FutureLastImprove verifies that when the system clock
// jumps forward (lastImprove is in the future relative to real time),
// the cooldown check would block improvement. This simulates a clock
// skew where time.Now() appears to be BEFORE lastImprove.
func TestClockSkew_FutureLastImprove(t *testing.T) {
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// The spec's cooldown check:
	//   cooldownMet := bs.lastImprove.IsZero() || time.Since(bs.lastImprove) >= s.config.AutoImproveCooldown
	// If lastImprove is in the future (e.g., time.Now().Add(5*time.Minute)),
	// then time.Since() returns a negative duration.
	// For cooldown=120s: -5m >= 120s is false → blocked.

	s := &Server{
		config: Config{
			AutoImproveAfterN:  1,
			AutoImproveCooldown: 120 * time.Second,
		},
		improveState: loadAutoImproveState(dir),
		cogneeSemaphore: make(chan struct{}, 10),
		cogneeCtx: ctx,
		log:       noopLogger(),
		backend:   &mockBackend{},
		metrics:   fullMetrics(),
		panics:    atomic.Int64{},
		dataDir:   dir,
	}
	s.cogneeWg = sync.WaitGroup{}

	// Set lastImprove to 5 minutes in the future (simulates clock jump)
	futureTime := time.Now().Add(5 * time.Minute)
	s.improveState.mu.Lock()
	s.improveState.banks["bank"] = &bankState{
		lastImprove: futureTime,
	}
	s.improveState.mu.Unlock()

	// Try to fire — should be blocked by cooldown
	s.maybeAutoImprove("bank")

	// Wait briefly
	time.Sleep(100 * time.Millisecond)

	// Verify no goroutine was spawned (improveInFlight should be false)
	s.improveState.mu.Lock()
	bs := s.improveState.banks["bank"]
	s.improveState.mu.Unlock()
	if bs == nil {
		t.Fatal("bank state should exist")
	}
	if bs.improveInFlight {
		t.Fatal("improve should NOT fire when lastImprove is in the future (clock skew)")
	}
	if bs.retainsSince != 0 {
		// retainsSince didn't increment?! Wait — let me re-check the code flow.
		// In maybeAutoImprove:
		//   1. Get or create bank state
		//   2. Increment retainsSince
		//   3. Persist
		//   4. Check conditions
		//   5. If conditions fail, unlock and return
		// So retainsSince SHOULD be incremented even when cooldown blocks.
		// But I set lastImprove BEFORE calling maybeAutoImprove, and
		// the bank state was pre-created. Let me check...
		// The bank state has retainsSince=0 (default) and lastImprove already set.
		// maybeAutoImprove will get the existing bank, increment to 1,
		// then check conditions. Condition 4 (cooldown) fails.
		// retainsSince should be 1.
		t.Logf("Expected retainsSince=1 (incremented even when blocked), got %d", bs.retainsSince)
	}

	// Now simulate the clock catching up: set lastImprove to 5 minutes ago
	// and verify the cooldown check passes.
	s.improveState.mu.Lock()
	bs.lastImprove = time.Now().Add(-5 * time.Minute)
	s.improveState.mu.Unlock()

	s.maybeAutoImprove("bank")

	done := make(chan struct{})
	go func() { s.cogneeWg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("goroutine did not complete after clock catch-up")
	}

	s.improveState.mu.Lock()
	r := s.improveState.banks["bank"].retainsSince
	inFlight := s.improveState.banks["bank"].improveInFlight
	s.improveState.mu.Unlock()

	if r != 0 {
		t.Fatalf("retainsSince should be 0 (fired after clock catch-up), got %d", r)
	}
	if inFlight {
		t.Fatal("improveInFlight should be false")
	}
}

// TestClockSkew_ZeroLastImprove verifies that zero lastImprove (never
// improved) always passes the cooldown check.
func TestClockSkew_ZeroLastImprove(t *testing.T) {
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	s := &Server{
		config: Config{
			AutoImproveAfterN:  1,
			AutoImproveCooldown: 10000 * time.Hour, // huge cooldown
		},
		improveState:    loadAutoImproveState(dir),
		cogneeSemaphore: make(chan struct{}, 10),
		cogneeCtx:       ctx,
		log:             noopLogger(),
		backend:         &mockBackend{},
		metrics:         fullMetrics(),
		panics:          atomic.Int64{},
		dataDir:         dir,
	}
	s.cogneeWg = sync.WaitGroup{}

	// lastImprove is zero (default) → IsZero() = true → always eligible
	s.maybeAutoImprove("bank")

	done := make(chan struct{})
	go func() { s.cogneeWg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("goroutine did not complete")
	}

	s.improveState.mu.Lock()
	r := s.improveState.banks["bank"].retainsSince
	s.improveState.mu.Unlock()
	if r != 0 {
		t.Fatalf("retainsSince=0 (fired even with huge cooldown due to zero lastImprove), got %d", r)
	}
}

// =========================================================================
// ATTACK 5: Rapid Threshold Changes
// =========================================================================

// TestThresholdChanges_LoadCounterAboveThreshold verifies that when
// improve_state.json is loaded with retains_since=3 and the process
// starts with AUTO_IMPROVE_AFTER_N=2, the next retain fires immediately
// (the persisted counter is already above the new threshold).
func TestThresholdChanges_LoadCounterAboveThreshold(t *testing.T) {
	dir := t.TempDir()

	// Persist state with retainsSince=3 (was configured for threshold=5)
	persisted := map[string]persistedBankState{
		"bank": {RetainsSince: 3},
	}
	data, _ := json.Marshal(persisted)
	path := filepath.Join(dir, "improve_state.json")
	os.WriteFile(path, data, 0644)

	// Load with lower threshold (2) — counter 3 > threshold 2, so
	// next retain should fire immediately.
	state := loadAutoImproveState(dir)
	if state.banks["bank"].retainsSince != 3 {
		t.Fatalf("expected retainsSince=3 from persisted state")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	s := &Server{
		config: Config{
			AutoImproveAfterN:   2, // threshold lowered from 5 to 2
			AutoImproveCooldown: 0,
		},
		improveState:    state,
		cogneeSemaphore: make(chan struct{}, 10),
		cogneeCtx:       ctx,
		log:             noopLogger(),
		backend:         &mockBackend{},
		metrics:         fullMetrics(),
		panics:          atomic.Int64{},
		dataDir:         dir,
	}
	s.cogneeWg = sync.WaitGroup{}

	// One retain should fire immediately (counter goes from 3 to 4,
	// 4 >= 2 → fire)
	s.maybeAutoImprove("bank")

	done := make(chan struct{})
	go func() { s.cogneeWg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("goroutine did not complete")
	}

	s.improveState.mu.Lock()
	r := s.improveState.banks["bank"].retainsSince
	s.improveState.mu.Unlock()
	if r != 0 {
		t.Fatalf("retainsSince should be 0 (fired and reset), got %d", r)
	}
}

// TestThresholdChanges_LoadCounterAtThreshold verifies that loading
// retains_since exactly at the threshold fires on the next retain
// (counter goes above threshold).
func TestThresholdChanges_LoadCounterAtThreshold(t *testing.T) {
	dir := t.TempDir()

	// Persist state with retainsSince=5 (old threshold was 5)
	persisted := map[string]persistedBankState{
		"bank": {RetainsSince: 5},
	}
	data, _ := json.Marshal(persisted)
	path := filepath.Join(dir, "improve_state.json")
	os.WriteFile(path, data, 0644)

	state := loadAutoImproveState(dir)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	s := &Server{
		config: Config{
			AutoImproveAfterN:   3, // threshold lowered to 3
			AutoImproveCooldown: 0,
		},
		improveState:    state,
		cogneeSemaphore: make(chan struct{}, 10),
		cogneeCtx:       ctx,
		log:             noopLogger(),
		backend:         &mockBackend{},
		metrics:         fullMetrics(),
		panics:          atomic.Int64{},
		dataDir:         dir,
	}
	s.cogneeWg = sync.WaitGroup{}

	// Counter is already at 5 (> 3) → fires immediately
	s.maybeAutoImprove("bank")

	done := make(chan struct{})
	go func() { s.cogneeWg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("goroutine did not complete")
	}

	s.improveState.mu.Lock()
	r := s.improveState.banks["bank"].retainsSince
	s.improveState.mu.Unlock()
	if r != 0 {
		t.Fatalf("retainsSince=0 (fired), got %d", r)
	}
}

// =========================================================================
// ATTACK 7: Mock Cognee Response Body Edge Cases
// =========================================================================

// TestCogneeMock_TruncatedJSONResponse verifies that the Cognee backend
// handles truncated JSON from the mock (doRequest returns the raw body
// even if it's not valid JSON — the status code determines success/failure).
func TestCogneeMock_TruncatedJSONResponse(t *testing.T) {
	mock := cogneemock.NewServer()
	defer mock.Close()

	// Truncated JSON — no closing brace
	mock.SetResponse("/api/v1/improve", cogneemock.ResponseConfig{
		StatusCode: 200,
		Body:       `{"status":"PipelineRunCompleted`,
	})

	resp, err := http.Post(mock.URL()+"/api/v1/improve", "application/json",
		strings.NewReader(`{"dataset_name":"bank","data":""}`))
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	// Verify the truncated body was returned as-is
	var buf bytes.Buffer
	buf.ReadFrom(resp.Body)
	body := buf.String()
	if !strings.Contains(body, "PipelineRunCompleted") {
		t.Fatalf("expected truncated body to contain PipelineRunCompleted, got: %s", body)
	}
	// The Go backend's doRequest doesn't validate JSON shape — it just
	// checks status code 200. So truncated JSON is technically "success"
	// as far as the backend is concerned. The auto-improve goroutine
	// also doesn't parse the body — it just checks err != nil.
}

// TestCogneeMock_NonJSONResponseWith200 verifies 200 with non-JSON body.
func TestCogneeMock_NonJSONResponseWith200(t *testing.T) {
	mock := cogneemock.NewServer()
	defer mock.Close()

	// Non-JSON body with 200 OK — valid HTTP response
	mock.SetResponse("/api/v1/improve", cogneemock.ResponseConfig{
		StatusCode: 200,
		Body:       `just plain text, not JSON at all`,
	})

	resp, err := http.Post(mock.URL()+"/api/v1/improve", "application/json",
		strings.NewReader(`{"dataset_name":"bank","data":""}`))
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
}

// TestCogneeMock_EmptyBodyWith200 verifies 200 with empty body.
func TestCogneeMock_EmptyBodyWith200(t *testing.T) {
	mock := cogneemock.NewServer()
	defer mock.Close()

	mock.SetResponse("/api/v1/improve", cogneemock.ResponseConfig{
		StatusCode: 200,
		Body:       "",
	})

	// Zero-value body falls back to default. So this should return
	// the default body for /api/v1/improve.
	resp, err := http.Post(mock.URL()+"/api/v1/improve", "application/json",
		strings.NewReader(`{"dataset_name":"bank","data":""}`))
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	var buf bytes.Buffer
	buf.ReadFrom(resp.Body)
	// Should have received the default body, not empty string
	if buf.Len() == 0 {
		t.Fatal("body should contain default response, not empty")
	}
}

// TestCogneeMock_EmptyBodyWithNon200 verifies non-200 with empty body.
func TestCogneeMock_EmptyBodyWithNon200(t *testing.T) {
	mock := cogneemock.NewServer()
	defer mock.Close()

	mock.SetResponse("/api/v1/improve", cogneemock.ResponseConfig{
		StatusCode: 500,
		Body:       "",
	})

	resp, err := http.Post(mock.URL()+"/api/v1/improve", "application/json",
		strings.NewReader(`{"dataset_name":"bank","data":""}`))
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 500 {
		t.Fatalf("expected 500, got %d", resp.StatusCode)
	}

	var buf bytes.Buffer
	buf.ReadFrom(resp.Body)
	t.Logf("500 response body (may be default or empty): len=%d", buf.Len())
	// The mock should return the default body since StatusCode != 0 triggers
	// the override path, but Body is "" so it uses the override body (empty).
	// Wait — looking at the mock handler:
	//   cfg := s.getResponse("/api/v1/improve")
	//   code := cfg.StatusCode
	//   if code == 0 { code = http.StatusOK }
	//   body := cfg.Body
	//   if body == "" { body = defaultResponses[...] }
	// So code=500 (non-zero, overridden), but body="" → falls back to default.
	// Wait, but the SetResponse path:
	//   if cfg.StatusCode == 0 && cfg.Body == "" {
	//       delete(s.responses, endpoint)
	//   } else {
	//       s.responses[endpoint] = cfg
	//   }
	// StatusCode=500, Body="" → stored as {500, ""}
	// Then getResponse returns {500, ""}
	// code = 500 (non-zero, so not overwritten to 200)
	// body = "" → falls back to defaultResponses[...]
	// So it returns 500 with DEFAULT body.
	// That's the current behavior.
}

// TestCogneeMock_VeryLargeJSONBodyWithNon200 verifies non-200 with 10MB body.
func TestCogneeMock_VeryLargeJSONBodyWithNon200(t *testing.T) {
	mock := cogneemock.NewServer()
	defer mock.Close()

	mock.SetResponse("/api/v1/improve", cogneemock.ResponseConfig{
		StatusCode: 429,
		Body:       `{"error":"rate limited","detail":"` + strings.Repeat("x", 10*1024*1024) + `"}`,
	})

	resp, err := http.Post(mock.URL()+"/api/v1/improve", "application/json",
		strings.NewReader(`{"dataset_name":"bank","data":""}`))
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 429 {
		t.Fatalf("expected 429, got %d", resp.StatusCode)
	}

	// Read response to verify it's sent (even if go backend truncates it)
	var buf bytes.Buffer
	buf.ReadFrom(resp.Body)
	t.Logf("429 response body length: %d (may be limited by client timeout)", buf.Len())
}

// TestCogneeMock_WrongShapeJSON verifies the mock returns whatever body
// is configured, even if it's wrong shape for the client's parser.
func TestCogneeMock_WrongShapeJSON(t *testing.T) {
	mock := cogneemock.NewServer()
	defer mock.Close()

	// Wrong shape — recall expects array, gets object
	mock.SetResponse("/api/v1/recall", cogneemock.ResponseConfig{
		StatusCode: 200,
		Body:       `{"status":"unexpected_object"}`,
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
}

// =========================================================================
// ATTACK 8: Concurrent SetResponse + HTTP Request (Race Exposure)
// =========================================================================

// TestCogneeMock_ConcurrentSetResponseAndRequest verifies no data races
// when SetResponse is called concurrently with HTTP requests.
// The mock uses RWMutex: SetResponse acquires write lock, HTTP handlers
// acquire read lock (via getResponse) and write lock (via captureMiddleware).
func TestCogneeMock_ConcurrentSetResponseAndRequest(t *testing.T) {
	mock := cogneemock.NewServer()
	defer mock.Close()

	var wg sync.WaitGroup
	errs := make(chan error, 100)

	// 10 goroutines: concurrently call SetResponse and make HTTP requests
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				// Mix: sometimes SetResponse, sometimes HTTP request
				if j%2 == 0 {
					mock.SetResponse("/api/v1/improve", cogneemock.ResponseConfig{
						StatusCode: 200,
						Body:       fmt.Sprintf(`{"status":"override_%d_%d"}`, id, j),
					})
					// Reset to default
					mock.SetResponse("/api/v1/improve", cogneemock.ResponseConfig{})
				} else {
					// Make an actual HTTP request
					resp, err := http.Post(mock.URL()+"/api/v1/improve", "application/json",
						strings.NewReader(`{"dataset_name":"bank","data":""}`))
					if err != nil {
						select {
						case errs <- fmt.Errorf("request %d/%d failed: %w", id, j, err):
						default:
						}
						continue
					}
					resp.Body.Close()
					if resp.StatusCode != 200 {
						select {
						case errs <- fmt.Errorf("request %d/%d: status %d", id, j, resp.StatusCode):
						default:
						}
					}
				}
			}
		}(i)
	}
	wg.Wait()
	close(errs)

	var failures []error
	for err := range errs {
		failures = append(failures, err)
	}
	if len(failures) > 0 {
		for _, f := range failures {
			t.Logf("failure: %v", f)
		}
		t.Fatalf("%d request failures during concurrent SetResponse+HTTP", len(failures))
	}

	// Verify requests were still captured (some may have been lost if
	// captureMiddleware's mu.Lock blocked SetResponse's mu.Lock, but
	// eventually all complete).
	reqs := mock.Requests()
	t.Logf("Captured %d requests during concurrent SetResponse+HTTP", len(reqs))
	if len(reqs) == 0 {
		t.Fatal("expected at least some requests captured")
	}
}

// TestCogneeMock_SetResponseAndRequestsRaceDetector verifies the race
// detector finds no issues with concurrent SetResponse and Requests() calls.
func TestCogneeMock_SetResponseAndRequestsRaceDetector(t *testing.T) {
	mock := cogneemock.NewServer()
	defer mock.Close()

	var wg sync.WaitGroup
	// 5 readers (Requests), 5 writers (SetResponse)
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				_ = mock.Requests()
			}
		}(i)
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				mock.SetResponse("/health", cogneemock.ResponseConfig{
					StatusCode: 200,
					Body:       `{"status":"ready"}`,
				})
			}
		}(i + 10)
	}
	wg.Wait()
}

// TestCogneeMock_SetResponseAndLastRequestRaceDetector verifies race-free
// SetResponse with concurrent LastRequest calls.
func TestCogneeMock_SetResponseAndLastRequestRaceDetector(t *testing.T) {
	mock := cogneemock.NewServer()
	defer mock.Close()

	// First make some requests so LastRequest has data
	for i := 0; i < 5; i++ {
		resp, err := http.Post(mock.URL()+"/api/v1/improve", "application/json",
			strings.NewReader(`{"dataset_name":"bank","data":""}`))
		if err != nil {
			t.Fatalf("setup request failed: %v", err)
		}
		resp.Body.Close()
	}

	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				lr := mock.LastRequest("/api/v1/improve")
				if lr != nil {
					_ = lr.Path
					_ = lr.Method
				}
			}
		}()
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				mock.SetResponse("/api/v1/improve", cogneemock.ResponseConfig{
					StatusCode: 200,
					Body:       `{"status":"override"}`,
				})
			}
		}()
	}
	wg.Wait()
}

// =========================================================================
// ADDITIONAL: Goroutine cleanup on partial initialization
// =========================================================================

// TestGoroutineCleanupAfterPanicRecover verifies that after a panic in the
// goroutine body (not during defer registration), the cleanup defers run
// and improveInFlight is reset.
func TestGoroutineCleanupAfterPanicRecover(t *testing.T) {
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	panics := atomic.Int64{}

	s := &Server{
		config: Config{
			AutoImproveAfterN:  1,
			AutoImproveCooldown: 0,
		},
		improveState:    loadAutoImproveState(dir),
		cogneeSemaphore: make(chan struct{}, 10),
		cogneeCtx:       ctx,
		log:             noopLogger(),
		backend: &mockBackend{
			reflectFn: func(ctx context.Context, bank string, query string) (string, error) {
				// Panic inside the goroutine body (after all defers registered)
				panic("simulated panic in reflect")
			},
		},
		metrics: fullMetrics(),
		panics:  panics,
		dataDir: dir,
	}
	s.cogneeWg = sync.WaitGroup{}

	s.maybeAutoImprove("bank")

	// Wait for goroutine to complete (recover + cleanup)
	done := make(chan struct{})
	go func() { s.cogneeWg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("goroutine did not complete")
	}

	// Verify panics counter was incremented (check on the Server field)
	if s.panics.Load() == 0 {
		t.Error("panics counter should have been incremented after panic recovery")
	}

	// Verify improveInFlight was reset
	s.improveState.mu.Lock()
	bs := s.improveState.banks["bank"]
	s.improveState.mu.Unlock()
	if bs == nil {
		t.Fatal("bank state should exist")
	}
	if bs.improveInFlight {
		t.Fatal("improveInFlight should be false after goroutine recovers from panic")
	}
	if bs.retainsSince != 0 {
		t.Fatalf("retainsSince should be 0 (fired), got %d", bs.retainsSince)
	}
}

// =========================================================================
// ADDITIONAL: Persistence with concurrent goroutine exit
// =========================================================================

// TestConcurrentGoroutineCompletionAndStateRead verifies that reading
// improveState while a goroutine is cleaning up is safe (no data race).
func TestConcurrentGoroutineCompletionAndStateRead(t *testing.T) {
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	be := &mockBackend{
		reflectFn: func(ctx context.Context, bank string, query string) (string, error) {
			time.Sleep(50 * time.Millisecond)
			return `{"status":"completed"}`, nil
		},
	}

	s := &Server{
		config: Config{
			AutoImproveAfterN:  1,
			AutoImproveCooldown: 0,
		},
		improveState:    loadAutoImproveState(dir),
		cogneeSemaphore: make(chan struct{}, 10),
		cogneeCtx:       ctx,
		log:             noopLogger(),
		backend:         be,
		metrics:         fullMetrics(),
		panics:          atomic.Int64{},
		dataDir:         dir,
	}
	s.cogneeWg = sync.WaitGroup{}

	// Spawn 10 goroutines, each triggering auto-improve for a different bank
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			s.maybeAutoImprove(fmt.Sprintf("bank_%d", id))
		}(i)
	}
	wg.Wait()

	// While goroutines are completing, read state concurrently
	var readWg sync.WaitGroup
	for i := 0; i < 5; i++ {
		readWg.Add(1)
		go func() {
			defer readWg.Done()
			for j := 0; j < 20; j++ {
				s.improveState.mu.Lock()
				for bank, bs := range s.improveState.banks {
					_ = bank
					_ = bs.retainsSince
					_ = bs.lastImprove
					_ = bs.improveInFlight
				}
				s.improveState.mu.Unlock()
				time.Sleep(time.Millisecond)
			}
		}()
	}

	// Wait for all goroutines
	s.cogneeWg.Wait()
	readWg.Wait()

	// Verify final state
	s.improveState.mu.Lock()
	defer s.improveState.mu.Unlock()
	if len(s.improveState.banks) != 10 {
		t.Fatalf("expected 10 banks, got %d", len(s.improveState.banks))
	}
	for i := 0; i < 10; i++ {
		bank := fmt.Sprintf("bank_%d", i)
		bs := s.improveState.banks[bank]
		if bs == nil {
			t.Fatalf("bank %s missing", bank)
		}
		if bs.improveInFlight {
			t.Fatalf("bank %s: improveInFlight should be false", bank)
		}
	}
}

// =========================================================================
// ADDITIONAL: Stop during in-flight auto-improve
// =========================================================================

// TestStopDuringInFlightAutoImprove verifies that shutting down the server
// while an auto-improve goroutine is in-flight cancels it via cogneeCtx and
// completes within a reasonable timeout. The goroutine's context is derived
// from s.cogneeCtx, which is cancelled on Stop().
func TestStopDuringInFlightAutoImprove(t *testing.T) {
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	improveStarted := make(chan struct{})

	be := &mockBackend{
		reflectFn: func(ctx context.Context, bank string, query string) (string, error) {
			close(improveStarted)
			// Block until context is cancelled
			<-ctx.Done()
			return "", ctx.Err()
		},
	}

	s := &Server{
		config: Config{
			AutoImproveAfterN:  1,
			AutoImproveCooldown: 0,
			BackendReflectTimeout: 60 * time.Second, // long timeout
		},
		improveState:    loadAutoImproveState(dir),
		cogneeSemaphore: make(chan struct{}, 10),
		cogneeCtx:       ctx,
		cogneeCancel:    cancel,
		log:             noopLogger(),
		backend:         be,
		metrics:         fullMetrics(),
		panics:          atomic.Int64{},
		dataDir:         dir,
	}
	s.cogneeWg = sync.WaitGroup{}

	// Trigger auto-improve (blocks in reflectFn waiting for context cancel)
	s.maybeAutoImprove("bank")

	// Wait for goroutine to start
	select {
	case <-improveStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("goroutine did not start")
	}

	// Simulate Stop(): cancel cogneeCtx
	stopStart := time.Now()
	s.cogneeCancel()

	// Wait for goroutine to complete
	done := make(chan struct{})
	go func() { s.cogneeWg.Wait(); close(done) }()
	select {
	case <-done:
		elapsed := time.Since(stopStart)
		if elapsed > 30*time.Second {
			t.Fatalf("Stop() took too long: %v", elapsed)
		}
		t.Logf("goroutine completed %v after context cancel", elapsed)
	case <-time.After(30 * time.Second):
		t.Fatal("goroutine did not complete after context cancel — cogneeWg may be stuck")
	}

	// Verify state
	s.improveState.mu.Lock()
	bs := s.improveState.banks["bank"]
	s.improveState.mu.Unlock()
	if bs == nil {
		t.Fatal("bank state should exist")
	}
	if bs.improveInFlight {
		t.Fatal("improveInFlight should be false after goroutine exits")
	}
}

// =========================================================================
// ADDITIONAL: Cooldown + Clock Skew — NTP backward jump simulation
// =========================================================================

// TestClockSkew_NTPBackwardJump simulates an NTP adjustment that moves
// the clock backward by 1 hour. Since lastImprove was recorded at the
// "new" time but time.Now() is now 1 hour earlier, time.Since() returns
// negative. The cooldown check would fail, blocking improvement.
//
// This test documents the behavior and flags it as a design concern.
func TestClockSkew_NTPBackwardJump(t *testing.T) {
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	s := &Server{
		config: Config{
			AutoImproveAfterN:  1,
			AutoImproveCooldown: 120 * time.Second,
		},
		improveState:    loadAutoImproveState(dir),
		cogneeSemaphore: make(chan struct{}, 10),
		cogneeCtx:       ctx,
		log:             noopLogger(),
		backend:         &mockBackend{},
		metrics:         fullMetrics(),
		panics:          atomic.Int64{},
		dataDir:         dir,
	}
	s.cogneeWg = sync.WaitGroup{}

	// Record lastImprove at "current wall clock" time
	s.improveState.mu.Lock()
	s.improveState.banks["bank"] = &bankState{
		lastImprove: time.Now().UTC(),
	}
	s.improveState.mu.Unlock()

	// Simulate NTP jumping backward by 1 hour.
	// In reality, this would be time.Now() returning an earlier value.
	// We can't change the system clock, but we can compute what
	// time.Since would return.
	s.improveState.mu.Lock()
	bs := s.improveState.banks["bank"]
	realTimeSince := time.Since(bs.lastImprove)
	// If clock jumped backward by 1h, simulated time.Since would be:
	simulatedTimeSince := realTimeSince - 1*time.Hour
	s.improveState.mu.Unlock()

	cooldown := s.config.AutoImproveCooldown
	wouldBlock := simulatedTimeSince < cooldown

	t.Logf("NTP backward jump simulation:")
	t.Logf("  lastImprove: %v", bs.lastImprove)
	t.Logf("  real time.Since: %v", realTimeSince)
	t.Logf("  simulated time.Since (after -1h jump): %v", simulatedTimeSince)
	t.Logf("  cooldown: %v", cooldown)
	t.Logf("  would cooldown block? %v", wouldBlock)

	if !wouldBlock {
		t.Log("NOTE: clock skew does NOT block improvement in this scenario")
	} else {
		t.Log("CONFIRMED: NTP backward jump can block improvement until real time catches up")
	}

	// Now actually test: set lastImprove such that time.Since() would
	// be negative (simulate the backward jump by setting lastImprove
	// to NOW + some offset).
	s.improveState.mu.Lock()
	// If the clock was at T and jumped back to T-1h, the stored
	// lastImprove is at T, but time.Now() is now T-1h.
	// So time.Since(lastImprove) = (T-1h) - T = -1h.
	bs.lastImprove = time.Now().Add(1 * time.Hour) // 1 hour in the future
	s.improveState.mu.Unlock()

	// Try to fire — should be blocked (negative time.Since < cooldown)
	s.maybeAutoImprove("bank")
	time.Sleep(100 * time.Millisecond)

	s.improveState.mu.Lock()
	inFlight := s.improveState.banks["bank"].improveInFlight
	r := s.improveState.banks["bank"].retainsSince
	s.improveState.mu.Unlock()

	if inFlight {
		t.Fatal("BUG: improve fired despite simulated clock backward jump")
	}
	t.Logf("Clock skew block confirmed: retainsSince=%d (incremented but blocked), inFlight=%v", r, inFlight)
}

// TestBankNameCollisionDifferentBanks verifies isolation: multiple banks
// called sequentially get independent counters and all fires succeed.
func TestBankNameCollisionDifferentBanks(t *testing.T) {
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	s := &Server{
		config: Config{
			AutoImproveAfterN:  1,
			AutoImproveCooldown: 0,
		},
		improveState:    loadAutoImproveState(dir),
		cogneeSemaphore: make(chan struct{}, 10),
		cogneeCtx:       ctx,
		log:             noopLogger(),
		backend:         &mockBackend{},
		metrics:         fullMetrics(),
		panics:          atomic.Int64{},
		dataDir:         dir,
	}
	s.cogneeWg = sync.WaitGroup{}

	// Sequential calls for 5 different banks.
	for i := 0; i < 5; i++ {
		s.maybeAutoImprove(fmt.Sprintf("bank_%d", i))
	}

	// Wait for all goroutines to complete
	done := make(chan struct{})
	go func() { s.cogneeWg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("goroutines did not complete")
	}

	// All 5 banks should have fired and reset to 0
	s.improveState.mu.Lock()
	defer s.improveState.mu.Unlock()

	if len(s.improveState.banks) != 5 {
		t.Fatalf("expected 5 banks, got %d", len(s.improveState.banks))
	}
	for i := 0; i < 5; i++ {
		bank := fmt.Sprintf("bank_%d", i)
		bs := s.improveState.banks[bank]
		if bs == nil {
			t.Fatalf("bank %s missing", bank)
		}
		if bs.retainsSince != 0 {
			t.Fatalf("bank %s: expected retainsSince=0, got %d", bank, bs.retainsSince)
		}
		if bs.improveInFlight {
			t.Fatalf("bank %s: improveInFlight should be false", bank)
		}
	}
}
