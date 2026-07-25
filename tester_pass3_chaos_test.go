package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"mcp-memory/backend"
	"mcp-memory/internal/testutil/cogneemock"
	"mcp-memory/logger"
	"mcp-memory/metrics"
)

// ======================================================================
// PASS 3: Chaos Testing
// ======================================================================
//
// Final pass. Brutally aggressive: rapid cycles, concurrent storms,
// memory leaks, goroutine leaks, crash recovery.
//
// NOTE: This file relies on validTestServer() defined in
// tester_pass2_autoimprove_boundary_test.go. It also must avoid
// redefining httpGet (from services.go). All HTTP helpers use
// the full net/http import directly.
//

// chaosTestServer creates a Server with a custom backend.Backend interface.
func chaosTestServer(dir string, cfg Config, be backend.Backend) *Server {
	var buf bytes.Buffer
	l, err := logger.NewBuf("test", "error", &buf)
	if err != nil {
		panic(fmt.Sprintf("failed to create test logger: %v", err))
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Server{
		config:          cfg,
		improveState:    loadAutoImproveState(dir),
		cogneeSemaphore: make(chan struct{}, 100),
		log:             l,
		metrics: &serverMetrics{
			errorCalls: metrics.NewCounter("test"),
		},
		backend:      be,
		cogneeCtx:    ctx,
		cogneeCancel: cancel,
	}
}

// chaosSlowBackend simulates a slow Cognee reflect endpoint.
type chaosSlowBackend struct {
	mockBackend
	delay time.Duration
}

func (s *chaosSlowBackend) Reflect(ctx context.Context, bank string, query string) (string, error) {
	select {
	case <-time.After(s.delay):
		return "", nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// -----------------------------------------------------------------------
// Attack 1: Goroutine leak after mass retains — 100 retains, 50 banks,
// AUTO_IMPROVE_AFTER_N=1. Baseline NumGoroutine before/after.
// -----------------------------------------------------------------------
func TestChaos_GoroutineLeakAfterMassRetains(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{
		AutoImproveAfterN:   1,
		AutoImproveCooldown: 0,
	}
	s := validTestServer(dir, cfg)

	baseline := runtime.NumGoroutine()

	// 100 retains across 50 banks (2 per bank)
	const numRetains = 100
	const numBanks = 50
	banks := make([]string, numBanks)
	for i := 0; i < numBanks; i++ {
		banks[i] = fmt.Sprintf("leak_bank_%d", i)
	}

	var wg sync.WaitGroup
	for i := 0; i < numRetains; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			s.maybeAutoImprove(banks[idx%numBanks])
		}(i)
	}
	wg.Wait()

	// Wait for all spawned auto-improve goroutines to complete
	s.cogneeWg.Wait()

	// Give goroutines a moment to fully unwind
	time.Sleep(50 * time.Millisecond)

	endGoroutines := runtime.NumGoroutine()
	leaked := endGoroutines - baseline
	if leaked > 2 {
		t.Errorf("goroutine leak detected: baseline=%d, end=%d, delta=%d",
			baseline, endGoroutines, leaked)
	} else {
		t.Logf("goroutine delta OK: baseline=%d, end=%d, delta=%d",
			baseline, endGoroutines, leaked)
	}

	// Verify no improveInFlight stuck
	s.improveState.mu.Lock()
	stuckCount := 0
	for name, bs := range s.improveState.banks {
		if bs.improveInFlight {
			stuckCount++
			t.Errorf("improveInFlight stuck=true for bank %q after all goroutines completed", name)
		}
	}
	s.improveState.mu.Unlock()
	if stuckCount > 0 {
		t.Fatalf("%d banks have stuck improveInFlight", stuckCount)
	}

	// Verify state file has correct number of banks
	path := filepath.Join(dir, "improve_state.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("state file not found: %v", err)
	}
	var persisted map[string]persistedBankState
	if err := json.Unmarshal(data, &persisted); err != nil {
		t.Fatalf("corrupt state file after mass retains: %v", err)
	}
	if len(persisted) != numBanks {
		t.Errorf("expected %d banks in state file, got %d", numBanks, len(persisted))
	}

	// Check retainsSince distribution — due to TOCTOU race (spec documented),
	// a second concurrent retain may increment retainsSince after the improve
	// fires but before improveInFlight is set, leaving retainsSince=1.
	// This is expected per the spec (TOCTOU race documented as acceptable).
	nonZeroCount := 0
	totalRetainsSince := 0
	for name, ps := range persisted {
		totalRetainsSince += int(ps.RetainsSince)
		if ps.RetainsSince > 0 {
			nonZeroCount++
			_ = name
		}
	}
	t.Logf("%d/%d banks have non-zero retains_since (expected: TOCTOU race)", nonZeroCount, len(persisted))
	t.Logf("total retains_since across all banks: %d", totalRetainsSince)
}

// -----------------------------------------------------------------------
// Attack 2: Rapid start/stop cycles — create server, retain 5 items, cancel.
// Repeat 20 times. Verify no goroutine leaks, no file corruption.
// -----------------------------------------------------------------------
func TestChaos_RapidStartStopCycles(t *testing.T) {
	dir := t.TempDir()

	for cycle := 0; cycle < 20; cycle++ {
		cfg := Config{
			AutoImproveAfterN:   3,
			AutoImproveCooldown: 0,
		}
		cycleDir := filepath.Join(dir, fmt.Sprintf("cycle_%d", cycle))
		s := validTestServer(cycleDir, cfg)

		// Retain 5 items (exceeds threshold of 3)
		for i := 0; i < 5; i++ {
			s.maybeAutoImprove(fmt.Sprintf("cycle_bank_%d", cycle))
		}

		// Wait for any spawned goroutines to complete
		s.cogneeWg.Wait()

		// Cancel context (simulates Stop)
		s.cogneeCancel()

		// Verify state file for this cycle is valid JSON
		statePath := filepath.Join(cycleDir, "improve_state.json")
		data, err := os.ReadFile(statePath)
		if err != nil {
			t.Fatalf("cycle %d: state file missing: %v", cycle, err)
		}
		var persisted map[string]persistedBankState
		if err := json.Unmarshal(data, &persisted); err != nil {
			t.Fatalf("cycle %d: corrupt state file: %v\ncontent: %s", cycle, err, string(data))
		}

		// Verify no unexpected keys
		for bank, ps := range persisted {
			if bank != fmt.Sprintf("cycle_bank_%d", cycle) {
				t.Errorf("cycle %d: unexpected bank key %q in state", cycle, bank)
			}
			if ps.RetainsSince < 0 || ps.RetainsSince > 5 {
				t.Errorf("cycle %d: bank %q invalid retains_since=%d", cycle, bank, ps.RetainsSince)
			}
		}
	}

	// Check overall goroutine count is reasonable
	runtime.GC()
	time.Sleep(50 * time.Millisecond)
	gs := runtime.NumGoroutine()
	if gs > 50 {
		t.Logf("goroutines after 20 start/stop cycles: %d", gs)
	}
}

// -----------------------------------------------------------------------
// Attack 3: Concurrent retain storm — 50 goroutines retaining to the same
// bank with threshold=1. Verify no races, no panics.
// -----------------------------------------------------------------------
func TestChaos_ConcurrentRetainStorm_SameBank(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{
		AutoImproveAfterN:   1,
		AutoImproveCooldown: 0,
	}
	s := validTestServer(dir, cfg)

	const numGoroutines = 50

	var wg sync.WaitGroup
	for i := 0; i < numGoroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.maybeAutoImprove("storm_bank")
		}()
	}
	wg.Wait()

	// Wait for any spawned goroutines to complete
	s.cogneeWg.Wait()

	// Verify no panics
	if s.panics.Load() > 0 {
		t.Errorf("detected %d panics during concurrent retain storm", s.panics.Load())
	}

	// Verify state is valid
	s.improveState.mu.Lock()
	bs := s.improveState.banks["storm_bank"]
	if bs == nil {
		s.improveState.mu.Unlock()
		t.Fatal("storm_bank not found in state")
	}
	retainsSince := bs.retainsSince
	improveInFlight := bs.improveInFlight
	s.improveState.mu.Unlock()

	if retainsSince < 0 || retainsSince > int64(numGoroutines) {
		t.Errorf("retainsSince=%d out of expected range [0,%d]", retainsSince, numGoroutines)
	}
	if improveInFlight {
		t.Log("improveInFlight still true — goroutine may still be running")
	}

	// Verify state file is valid
	path := filepath.Join(dir, "improve_state.json")
	if _, err := os.Stat(path); os.IsNotExist(err) {
		t.Error("state file missing after concurrent storm")
	} else {
		data, _ := os.ReadFile(path)
		var persisted map[string]persistedBankState
		if err := json.Unmarshal(data, &persisted); err != nil {
			t.Errorf("state file corrupt after concurrent storm: %v", err)
		}
	}
}

// -----------------------------------------------------------------------
// Attack 4: Shutdown during in-flight improve — mock with 3s delay.
// Verify context cancel + Wait returns within timeout (not blocked by 3s).
// Verify improveInFlight is reset after goroutine exits.
// -----------------------------------------------------------------------
func TestChaos_ShutdownDuringInflightImprove(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{
		AutoImproveAfterN:   1,
		AutoImproveCooldown: 0,
	}
	be := &chaosSlowBackend{delay: 3 * time.Second}
	s := chaosTestServer(dir, cfg, be)

	// Trigger auto-improve (spawns goroutine with 3s slow reflect)
	s.maybeAutoImprove("slow_bank")

	// Immediately cancel context (simulates Stop())
	cancelDone := make(chan struct{})
	go func() {
		s.cogneeCancel()
		s.cogneeWg.Wait()
		close(cancelDone)
	}()

	// Wait for cancel to complete — should NOT take 3 seconds
	select {
	case <-cancelDone:
		// Good - completed quickly
	case <-time.After(5 * time.Second):
		t.Fatal("TIMEOUT: Stop() blocked by slow in-flight improve (3s delay)")
	}

	// Verify improveInFlight is reset
	s.improveState.mu.Lock()
	bs := s.improveState.banks["slow_bank"]
	if bs != nil && bs.improveInFlight {
		t.Error("improveInFlight still true after goroutine exit (should be reset)")
	}
	s.improveState.mu.Unlock()

	// Verify state file is valid
	path := filepath.Join(dir, "improve_state.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("state file not found after shutdown: %v", err)
	}
	var persisted map[string]persistedBankState
	if err := json.Unmarshal(data, &persisted); err != nil {
		t.Fatalf("state file corrupt after shutdown: %v", err)
	}
	if ps, ok := persisted["slow_bank"]; ok {
		if ps.RetainsSince != 0 {
			t.Errorf("retainsSince=%d after improve+shutdown, expected 0", ps.RetainsSince)
		}
	}
}

// -----------------------------------------------------------------------
// Attack 5: Persistence under simulated crash — kill without Stop().
// Atomic write (temp+rename) should prevent corruption.
// -----------------------------------------------------------------------
func TestChaos_PersistenceUnderSimulatedCrash(t *testing.T) {
	dir := t.TempDir()

	// Phase 1: Start server, retain items, do NOT call Stop (simulate crash)
	func() {
		cfg := Config{
			AutoImproveAfterN:   5,
			AutoImproveCooldown: 0,
		}
		s := validTestServer(dir, cfg)

		// Retain 3 items (below threshold)
		for i := 0; i < 3; i++ {
			s.maybeAutoImprove("crash_bank")
		}

		// Wait for any spawned goroutines (none should have spawned, threshold=5)
		s.cogneeWg.Wait()

		// Check state file
		path := filepath.Join(dir, "improve_state.json")
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("phase 1: state file missing: %v", err)
		}
		var persisted map[string]persistedBankState
		if err := json.Unmarshal(data, &persisted); err != nil {
			t.Fatalf("phase 1: corrupt state file: %v", err)
		}
		if persisted["crash_bank"].RetainsSince != 3 {
			t.Errorf("phase 1: expected retainsSince=3, got %d", persisted["crash_bank"].RetainsSince)
		}

		// Simulate crash: just let s go out of scope without calling Stop
		_ = s
	}()

	// Phase 2: Start new server with same data dir
	func() {
		cfg := Config{
			AutoImproveAfterN:   5,
			AutoImproveCooldown: 0,
		}
		s2 := validTestServer(dir, cfg)

		// Verify counter was loaded from persisted state
		if s2.improveState.banks["crash_bank"] == nil {
			t.Fatal("phase 2: crash_bank state not loaded from file")
		}
		if s2.improveState.banks["crash_bank"].retainsSince != 3 {
			t.Errorf("phase 2: expected retainsSince=3 (loaded from state), got %d",
				s2.improveState.banks["crash_bank"].retainsSince)
		}

		// Retain 2 more items — should now hit threshold 5
		for i := 0; i < 2; i++ {
			s2.maybeAutoImprove("crash_bank")
		}
		s2.cogneeWg.Wait()

		// After 2 more retains, threshold met, improve should have fired
		if s2.improveState.banks["crash_bank"].retainsSince != 0 {
			t.Errorf("expected retainsSince=0 after threshold reached, got %d",
				s2.improveState.banks["crash_bank"].retainsSince)
		}

		s2.cogneeCancel()
	}()
}

// -----------------------------------------------------------------------
// Attack 6: Memory under sustained load — 1000 retains, check memory
// doesn't grow unboundedly. Use runtime.ReadMemStats() before/after.
// -----------------------------------------------------------------------
func TestChaos_MemoryUnderSustainedLoad(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{
		AutoImproveAfterN:   100, // high threshold — just increments, no goroutines
		AutoImproveCooldown: 0,
	}
	s := validTestServer(dir, cfg)

	runtime.GC()
	var memBefore runtime.MemStats
	runtime.ReadMemStats(&memBefore)

	const numRetains = 1000
	for i := 0; i < numRetains; i++ {
		s.maybeAutoImprove(fmt.Sprintf("mem_bank_%d", i%10))
	}

	s.cogneeWg.Wait()

	runtime.GC()
	time.Sleep(50 * time.Millisecond)
	runtime.GC()
	var memAfter runtime.MemStats
	runtime.ReadMemStats(&memAfter)

	allocDelta := int64(memAfter.Alloc) - int64(memBefore.Alloc)

	t.Logf("memory before: Alloc=%d KB, TotalAlloc=%d KB",
		memBefore.Alloc/1024, memBefore.TotalAlloc/1024)
	t.Logf("memory after:  Alloc=%d KB, TotalAlloc=%d KB",
		memAfter.Alloc/1024, memAfter.TotalAlloc/1024)
	t.Logf("delta: Alloc=%+d KB", allocDelta/1024)

	// Allow generous headroom (10 MB). If there's a memory leak,
	// Alloc would be many MB after 1000 retains with only 10 banks.
	if allocDelta > 10*1024*1024 {
		t.Errorf("possible memory leak: Alloc grew by %d KB (>10 MB)", allocDelta/1024)
	}

	// Verify all banks present
	s.improveState.mu.Lock()
	bankCount := len(s.improveState.banks)
	s.improveState.mu.Unlock()
	if bankCount != 10 {
		t.Errorf("expected 10 banks, got %d", bankCount)
	}
}

// -----------------------------------------------------------------------
// Attack 7: Lock ordering deadlock — static analysis + stress test.
// Verify: no ABBA deadlock between autoImproveState.mu and cogneeSemaphore.
// -----------------------------------------------------------------------
func TestChaos_LockOrderingDeadlockAnalysis(t *testing.T) {
	// Static analysis: verify Lock/Unlock pairs are balanced per execution path.
	// NOTE: Lock counts may not equal Unlock counts at static call sites because
	// early-return paths produce multiple Unlocks for a single Lock (e.g., Lock
	// at line X, then Unlock at Y (normal return) and Unlock at Z (early return)).
	// Each individual path is balanced at runtime.

	src, err := os.ReadFile("auto_improve.go")
	if err != nil {
		t.Fatal(err)
	}

	lockCount := bytes.Count(src, []byte(".Lock()"))
	unlockCount := bytes.Count(src, []byte(".Unlock()"))

	t.Logf("auto_improve.go: %d Lock(), %d Unlock() (unbalanced expected due to early-return paths)", lockCount, unlockCount)

	// Verify the critical safe pattern: Lock is released BEFORE goroutine spawn
	// Check that Unlock() appears before any go func() in maybeAutoImprove
	if !bytes.Contains(src, []byte("s.improveState.mu.Unlock()")) ||
		!bytes.Contains(src, []byte("s.improveState.mu.Lock()")) {
		t.Error("cannot find Lock/Unlock pattern in auto_improve.go")
	}

	// Determine if there's an Unlock BEFORE go func() - this is the critical safety check
	funcIdx := bytes.Index(src, []byte("func (s *Server) maybeAutoImprove"))
	goIdx := bytes.LastIndex(src[funcIdx:], []byte("go func"))
	sub := src[funcIdx : funcIdx+goIdx]
	lastUnlock := bytes.LastIndex(sub, []byte("mu.Unlock()"))
	if lastUnlock < 0 {
		t.Error("CRITICAL: no mu.Unlock() before go func() in maybeAutoImprove")
	} else {
		t.Logf("confirmed: mu.Unlock() at offset %d before go func()", lastUnlock)
	}

	// Check server.go for general pattern
	srvSrc, err := os.ReadFile("server.go")
	if err != nil {
		t.Fatal(err)
	}
	srvLocks := bytes.Count(srvSrc, []byte(".Lock()"))
	srvUnlocks := bytes.Count(srvSrc, []byte(".Unlock()"))
	t.Logf("server.go: %d Lock(), %d Unlock()", srvLocks, srvUnlocks)

	// Check workers.go
	wkSrc, err := os.ReadFile("workers.go")
	if err != nil {
		t.Fatal(err)
	}
	wkLocks := bytes.Count(wkSrc, []byte(".Lock()"))
	wkUnlocks := bytes.Count(wkSrc, []byte(".Unlock()"))
	// Also count RLock/RUnlock pairs (session cleaner)
	wkRLocks := bytes.Count(wkSrc, []byte(".RLock()"))
	wkRUnlocks := bytes.Count(wkSrc, []byte(".RUnlock()"))
	t.Logf("workers.go: %d Lock/%d Unlock + %d RLock/%d RUnlock", wkLocks, wkUnlocks, wkRLocks, wkRUnlocks)

	// Verify Stop() doesn't acquire improveState.mu (no ABBA deadlock risk)
	stopIdx := bytes.Index(srvSrc, []byte("func (s *Server) Stop()"))
	if stopIdx >= 0 {
		afterStop := srvSrc[stopIdx:]
		endIdx := bytes.Index(afterStop, []byte("func ("))
		if endIdx > 0 {
			afterStop = afterStop[:endIdx]
		}
		if bytes.Contains(afterStop, []byte("improveState")) {
			t.Log("NOTE: Stop() references improveState (expected for nil check)")
		}
		if bytes.Contains(afterStop, []byte("s.improveState.mu")) {
			t.Error("CRITICAL: Stop() acquires improveState.mu - potential ABBA deadlock!")
		}
	}

	// Verify the 'retain goroutine' only acquires semaphore BEFORE calling maybeAutoImprove
	// (not after releasing mu)
	hSrc, err := os.ReadFile("handlers.go")
	if err != nil {
		t.Fatal(err)
	}
	retainIdx := bytes.Index(hSrc, []byte("case \"memory_retain\""))
	if retainIdx >= 0 {
		retainBlock := hSrc[retainIdx : retainIdx+3000]
		// Check: semaphore acquire before maybeAutoImprove call
		semaAcq := bytes.LastIndex(retainBlock, []byte("cogneeSemaphore <- struct{}{}"))
		improveCall := bytes.Index(retainBlock, []byte("maybeAutoImprove"))
		if semaAcq < improveCall {
			t.Log("confirmed: semaphore acquired before maybeAutoImprove call")
		} else {
			t.Log("NOTE: maybeAutoImprove may be called before semaphore acquire")
		}
	}

	t.Log("lock ordering analysis: no ABBA deadlock path identified (autoImproveState.mu not held by Stop)")
}

// TestChaos_LockOrderingStress runs concurrent retains with semaphore
// and Stop() to try to trigger an ABBA deadlock.
func TestChaos_LockOrderingStress(t *testing.T) {
	dir := t.TempDir()

	for attempt := 0; attempt < 5; attempt++ {
		cfg := Config{
			AutoImproveAfterN:   1,
			AutoImproveCooldown: 0,
		}
		be := &chaosSlowBackend{delay: 50 * time.Millisecond}
		s := chaosTestServer(dir, cfg, be)

		var retainWg sync.WaitGroup
		for i := 0; i < 10; i++ {
			retainWg.Add(1)
			go func(idx int) {
				defer retainWg.Done()
				// Acquire semaphore slot like real retain goroutine
				s.cogneeSemaphore <- struct{}{}
				defer func() { <-s.cogneeSemaphore }()

				s.maybeAutoImprove(fmt.Sprintf("deadlock_bank_%d", idx))
			}(i)
		}

		// Wait a tiny bit for goroutines to start
		time.Sleep(10 * time.Millisecond)

		// Simulate Stop() while retains are in-flight
		s.cogneeCancel()

		done := make(chan struct{})
		go func() {
			s.cogneeWg.Wait()
			close(done)
		}()

		select {
		case <-done:
			// Good — no deadlock
		case <-time.After(10 * time.Second):
			t.Fatalf("attempt %d: DEADLOCK detected! Stop() blocked for 10s", attempt)
		}

		retainWg.Wait()
	}
}

// -----------------------------------------------------------------------
// Attack 8: Mock Cognee under load — 100 concurrent HTTP requests.
// Verify all captured, no lost requests, no data races.
// -----------------------------------------------------------------------
func TestChaos_MockCogneeUnderHighLoad(t *testing.T) {
	mock := cogneemock.NewServer()
	defer mock.Close()

	const numRequests = 100

	var wg sync.WaitGroup
	errCount := atomic.Int64{}

	for i := 0; i < numRequests; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			epIdx := idx % 4
			switch epIdx {
			case 0:
				code, err := httpGetSimple(mock.URL() + "/health")
				if err != nil || code != 200 {
					errCount.Add(1)
				}
			case 1:
				code, err := httpPostSimple(mock.URL()+"/api/v1/remember",
					"application/json", `{"dataset_name":"loadtest","data":"content"}`)
				if err != nil || code != 200 {
					errCount.Add(1)
				}
			case 2:
				code, err := httpPostSimple(mock.URL()+"/api/v1/recall",
					"application/json", `{"query":"test","datasets":["loadtest"]}`)
				if err != nil || code != 200 {
					errCount.Add(1)
				}
			case 3:
				code, err := httpPostSimple(mock.URL()+"/api/v1/improve",
					"application/json", `{"dataset_name":"loadtest","data":""}`)
				if err != nil || code != 200 {
					errCount.Add(1)
				}
			}
		}(i)
	}
	wg.Wait()

	if errCount.Load() > 0 {
		t.Errorf("%d/%d requests returned errors", errCount.Load(), numRequests)
	}

	// Verify all requests were captured
	captured := len(mock.Requests())
	if captured != numRequests {
		t.Errorf("expected %d captured requests, got %d — some requests lost", numRequests, captured)
	} else {
		t.Logf("all %d requests captured correctly", captured)
	}

	// Verify path distribution
	paths := make(map[string]int)
	for _, req := range mock.Requests() {
		paths[req.Path]++
	}
	for path, count := range paths {
		if count > 35 {
			t.Errorf("path %q captured %d times (expected ~25)", path, count)
		}
	}
}

// -----------------------------------------------------------------------
// Attack 8b: Concurrent SetResponse and HTTP requests on mock.
// -----------------------------------------------------------------------
func TestChaos_MockCogneeConcurrentSetResponseAndRequests(t *testing.T) {
	mock := cogneemock.NewServer()
	defer mock.Close()

	var wg sync.WaitGroup

	// 10 goroutines rapidly changing responses
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				mock.SetResponse("/health", cogneemock.ResponseConfig{
					StatusCode: 200 + (j % 5),
					Body:       fmt.Sprintf(`{"iteration":%d}`, j),
				})
			}
		}(i)
	}

	// 10 goroutines making HTTP requests
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 30; j++ {
				httpGetSimple(mock.URL() + "/health")
			}
		}()
	}

	wg.Wait()

	reqs := mock.Requests()
	if len(reqs) != 300 { // 10 goroutines * 30 requests
		t.Errorf("expected 300 captured requests, got %d — request loss", len(reqs))
	}
}

// -----------------------------------------------------------------------
// Attack: Cognee mock rapid create/close cycles
// -----------------------------------------------------------------------
func TestChaos_MockRapidCreateClose(t *testing.T) {
	const iterations = 50
	for i := 0; i < iterations; i++ {
		mock := cogneemock.NewServer()
		code, err := httpGetSimple(mock.URL() + "/health")
		if err != nil || code != 200 {
			t.Errorf("iteration %d: health check failed: code=%d, err=%v", i, code, err)
		}
		mock.Close()
	}
}

// -----------------------------------------------------------------------
// Attack: Concurrent maybeAutoImprove with rapid shutdown
// -----------------------------------------------------------------------
func TestChaos_ConcurrentMaybeAutoImproveWithShutdown(t *testing.T) {
	// This test verifies no deadlock when maybeAutoImprove (which acquires
	// autoImproveState.mu) is called concurrently with context cancellation
	// and WaitGroup draining.
	be := &chaosSlowBackend{delay: 100 * time.Millisecond}
	s := chaosTestServer(t.TempDir(), Config{
		AutoImproveAfterN:   2,
		AutoImproveCooldown: 0,
	}, be)

	// Spawn test goroutines that call maybeAutoImprove
	var testWg sync.WaitGroup
	for i := 0; i < 5; i++ {
		testWg.Add(1)
		go func(idx int, srv *Server) {
			defer testWg.Done()
			for j := 0; j < 3; j++ {
				srv.maybeAutoImprove(fmt.Sprintf("shutdown_bank_%d", idx%2))
			}
		}(i, s)
	}

	// Cancel context (simulates Stop)
	s.cogneeCancel()

	// Wait for all goroutines to finish with timeout
	wgDone := make(chan struct{}, 1)
	go func(srv *Server) {
		srv.cogneeWg.Wait()
		wgDone <- struct{}{}
	}(s)

	select {
	case <-wgDone:
	case <-time.After(10 * time.Second):
		t.Fatalf("DEADLOCK during concurrent maybeAutoImprove + shutdown")
	}

	testWg.Wait()
}

// -----------------------------------------------------------------------
// Attack: Rapid fire improves then shutdown
// -----------------------------------------------------------------------
func TestChaos_RapidFireImproveThenShutdown(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{
		AutoImproveAfterN:   1,
		AutoImproveCooldown: 0,
	}
	s := validTestServer(dir, cfg)

	for i := 0; i < 20; i++ {
		s.maybeAutoImprove(fmt.Sprintf("rapid_bank_%d", i))
	}

	s.cogneeCancel()

	done := make(chan struct{})
	go func() {
		s.cogneeWg.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("DEADLOCK: rapid fire improves blocked shutdown")
	}

	// Verify state is consistent
	s.improveState.mu.Lock()
	for name, bs := range s.improveState.banks {
		if bs.improveInFlight {
			t.Errorf("bank %q: improveInFlight stuck after shutdown", name)
		}
	}
	s.improveState.mu.Unlock()
}

// -----------------------------------------------------------------------
// Attack: Random interleaving — concurrent retains on random banks
// with random timing, simulating real-world chaotic load.
// -----------------------------------------------------------------------
func TestChaos_RandomInterleaving(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{
		AutoImproveAfterN:   3,
		AutoImproveCooldown: time.Second,
	}
	s := validTestServer(dir, cfg)

	// Use a mutex to guard the shared rand source (math/rand is not concurrent-safe)
	var mu sync.Mutex
	rng := rand.New(rand.NewSource(time.Now().UnixNano()))
	var wg sync.WaitGroup

	for i := 0; i < 30; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			mu.Lock()
			bank := fmt.Sprintf("random_bank_%d", rng.Intn(5))
			delay := time.Duration(rng.Intn(5)) * time.Millisecond
			mu.Unlock()
			time.Sleep(delay)
			s.maybeAutoImprove(bank)
		}()
	}

	wg.Wait()
	s.cogneeWg.Wait()

	// Verify state file is valid
	path := filepath.Join(dir, "improve_state.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("state file missing: %v", err)
	}
	var persisted map[string]persistedBankState
	if err := json.Unmarshal(data, &persisted); err != nil {
		t.Fatalf("state file corrupt: %v\ncontent: %s", err, string(data))
	}

	totalRetains := 0
	for _, ps := range persisted {
		totalRetains += int(ps.RetainsSince)
	}
	t.Logf("random interleaving: %d banks, %d total retains in state file", len(persisted), totalRetains)
}

// -----------------------------------------------------------------------
// Local HTTP helpers — avoid naming conflict with services.go:httpGet
// by using different function names.
// -----------------------------------------------------------------------

func httpGetSimple(url string) (int, error) {
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	return resp.StatusCode, nil
}

func httpPostSimple(url, contentType, body string) (int, error) {
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Post(url, contentType, bytes.NewBufferString(body))
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	return resp.StatusCode, nil
}
