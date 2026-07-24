package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"mcp-memory/logger"
	"mcp-memory/metrics"
)

func TestLoadAutoImproveState_MissingFile(t *testing.T) {
	dir := t.TempDir()
	state := loadAutoImproveState(dir)

	if len(state.banks) != 0 {
		t.Fatalf("expected empty banks, got %d", len(state.banks))
	}
}

func TestLoadAutoImproveState_CorruptFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "improve_state.json")
	os.WriteFile(path, []byte(`{"garbage`), 0644)

	state := loadAutoImproveState(dir)

	if len(state.banks) != 0 {
		t.Fatalf("expected empty banks after corrupt file, got %d", len(state.banks))
	}
}

func TestLoadAutoImproveState_ValidFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "improve_state.json")

	data := map[string]persistedBankState{
		"bank_a": {RetainsSince: 5, LastImprove: time.Now().UTC()},
		"bank_b": {RetainsSince: 2},
	}
	jsonData, _ := json.Marshal(data)
	os.WriteFile(path, jsonData, 0644)

	state := loadAutoImproveState(dir)

	if len(state.banks) != 2 {
		t.Fatalf("expected 2 banks, got %d", len(state.banks))
	}
	if state.banks["bank_a"].retainsSince != 5 {
		t.Fatalf("bank_a retainsSince=5, got %d", state.banks["bank_a"].retainsSince)
	}
	if state.banks["bank_b"].retainsSince != 2 {
		t.Fatalf("bank_b retainsSince=2, got %d", state.banks["bank_b"].retainsSince)
	}
	// improveInFlight should always be false on load
	if state.banks["bank_a"].improveInFlight {
		t.Fatal("improveInFlight should be false on load")
	}
}

func TestSaveStateLocked_AtomicWrite(t *testing.T) {
	dir := t.TempDir()
	state := &autoImproveState{
		banks:   make(map[string]*bankState),
		dataDir: dir,
	}

	state.mu.Lock()
	state.banks["test"] = &bankState{retainsSince: 3, lastImprove: time.Now().UTC()}
	state.saveStateLocked()
	state.mu.Unlock()

	// Verify file exists
	path := filepath.Join(dir, "improve_state.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("failed to read persisted state: %v", err)
	}

	var persisted map[string]persistedBankState
	if err := json.Unmarshal(data, &persisted); err != nil {
		t.Fatalf("failed to unmarshal persisted state: %v", err)
	}

	if persisted["test"].RetainsSince != 3 {
		t.Fatalf("expected retainsSince=3, got %d", persisted["test"].RetainsSince)
	}

	// Verify improveInFlight is NOT persisted
	// (it's not in persistedBankState, so it can't be)
}

func TestSaveStateLocked_ImproInFlightNotPersisted(t *testing.T) {
	dir := t.TempDir()
	state := &autoImproveState{
		banks:   make(map[string]*bankState),
		dataDir: dir,
	}

	state.mu.Lock()
	state.banks["bank"] = &bankState{
		retainsSince:    5,
		lastImprove:     time.Now().UTC(),
		improveInFlight: true, // should not be persisted
	}
	state.saveStateLocked()
	state.mu.Unlock()

	// Reload and verify
	loaded := loadAutoImproveState(dir)
	if loaded.banks["bank"].improveInFlight {
		t.Fatal("improveInFlight should not be persisted")
	}
}

func TestSaveStateLocked_CreatesDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "data")
	state := &autoImproveState{
		banks:   make(map[string]*bankState),
		dataDir: dir,
	}

	state.mu.Lock()
	state.banks["bank"] = &bankState{retainsSince: 1}
	state.saveStateLocked()
	state.mu.Unlock()

	path := filepath.Join(dir, "improve_state.json")
	if _, err := os.Stat(path); os.IsNotExist(err) {
		t.Fatal("state file was not created")
	}
}

// mockBackend is a minimal backend for testing auto-improve.
type mockBackend struct {
	reflectFn func(ctx context.Context, bank string, query string) (string, error)
}

func (m *mockBackend) Retain(ctx context.Context, bank string, content string) (string, error) {
	return "", nil
}
func (m *mockBackend) Recall(ctx context.Context, bank string, query string) (string, error) {
	return "", nil
}
func (m *mockBackend) Reflect(ctx context.Context, bank string, query string) (string, error) {
	if m.reflectFn != nil {
		return m.reflectFn(ctx, bank, query)
	}
	return "", nil
}
func (m *mockBackend) Health(ctx context.Context) error { return nil }
func (m *mockBackend) Name() string                      { return "mock" }
func (m *mockBackend) IsSync() bool                      { return false }
func (m *mockBackend) Forget(ctx context.Context, bank string, contentID string) (string, error) {
	return "", nil
}

func testServer(dir string, cfg Config) *Server {
	l, err := logger.NewBuf("test", "error", &bytes.Buffer{})
	if err != nil {
		panic(fmt.Sprintf("failed to create test logger: %v", err))
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Server{
		config:        cfg,
		improveState:  loadAutoImproveState(dir),
		cogneeSemaphore: make(chan struct{}, 10),
		log:           l,
		metrics:       &serverMetrics{errorCalls: metrics.NewCounter("test")},
		backend:       &mockBackend{},
		cogneeCtx:     ctx,
		cogneeCancel:  cancel,
	}
}

func TestMaybeAutoImprove_DisabledWhenZero(t *testing.T) {
	dir := t.TempDir()
	s := &Server{
		config: Config{
			AutoImproveAfterN:  0,
			AutoImproveCooldown: 120 * time.Second,
		},
		improveState: loadAutoImproveState(dir),
		cogneeSemaphore: make(chan struct{}, 10),
	}

	// Should not panic or modify state
	s.maybeAutoImprove("testbank")

	if len(s.improveState.banks) != 0 {
		t.Fatal("expected no bank state created when disabled")
	}
}

func TestMaybeAutoImprove_ThresholdNotMet(t *testing.T) {
	dir := t.TempDir()
	s := testServer(dir, Config{
		AutoImproveAfterN:  5,
		AutoImproveCooldown: 120 * time.Second,
	})

	// 4 retains (threshold=5)
	for i := 0; i < 4; i++ {
		s.maybeAutoImprove("testbank")
	}

	if s.improveState.banks["testbank"].retainsSince != 4 {
		t.Fatalf("expected retainsSince=4, got %d", s.improveState.banks["testbank"].retainsSince)
	}
	if s.improveState.banks["testbank"].improveInFlight {
		t.Fatal("should not have fired improve (threshold not met)")
	}
}

func TestMaybeAutoImprove_IdleCheckBlocks(t *testing.T) {
	dir := t.TempDir()
	s := testServer(dir, Config{
		AutoImproveAfterN:  1,
		AutoImproveCooldown: 0,
	})

	// Simulate 2 active retains (semaphore has 2 items)
	s.cogneeSemaphore <- struct{}{}
	s.cogneeSemaphore <- struct{}{}

	s.maybeAutoImprove("testbank")

	// Should not fire because len(semaphore) > 1
	if s.improveState.banks["testbank"].improveInFlight {
		t.Fatal("should not have fired improve (idle check blocked)")
	}
}

func TestMaybeAutoImprove_CooldownBlocks(t *testing.T) {
	dir := t.TempDir()
	s := testServer(dir, Config{
		AutoImproveAfterN:  1,
		AutoImproveCooldown: 60 * time.Second,
	})
	s.improveState.banks["bank"] = &bankState{
		lastImprove: time.Now().UTC(), // just improved
	}

	s.maybeAutoImprove("bank")

	if s.improveState.banks["bank"].improveInFlight {
		t.Fatal("should not have fired improve (cooldown not elapsed)")
	}
}

func TestMaybeAutoImprove_InFlightBlocks(t *testing.T) {
	dir := t.TempDir()
	s := testServer(dir, Config{
		AutoImproveAfterN:  1,
		AutoImproveCooldown: 0,
	})
	s.improveState.banks["bank"] = &bankState{
		improveInFlight: true,
		retainsSince:    10, // way above threshold
	}

	s.maybeAutoImprove("bank")

	// Should not fire because improveInFlight is true
	// (retainsSince will be incremented to 11, but no goroutine spawned)
	if s.improveState.banks["bank"].retainsSince != 11 {
		t.Fatalf("expected retainsSince=11, got %d", s.improveState.banks["bank"].retainsSince)
	}
}

func TestMaybeAutoImprove_PersistsOnIncrement(t *testing.T) {
	dir := t.TempDir()
	s := testServer(dir, Config{
		AutoImproveAfterN:  10,
		AutoImproveCooldown: 120 * time.Second,
	})

	s.maybeAutoImprove("testbank")

	// Verify state was persisted
	path := filepath.Join(dir, "improve_state.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("state not persisted: %v", err)
	}

	var persisted map[string]persistedBankState
	json.Unmarshal(data, &persisted)
	if persisted["testbank"].RetainsSince != 1 {
		t.Fatalf("expected persisted retainsSince=1, got %d", persisted["testbank"].RetainsSince)
	}
}

func TestMaybeAutoImprove_PerBankIsolation(t *testing.T) {
	dir := t.TempDir()
	s := testServer(dir, Config{
		AutoImproveAfterN:  5,
		AutoImproveCooldown: 0,
	})

	// Bank A: 4 retains
	for i := 0; i < 4; i++ {
		s.maybeAutoImprove("bank_a")
	}
	// Bank B: 5 retains
	for i := 0; i < 5; i++ {
		s.maybeAutoImprove("bank_b")
	}

	// Wait for goroutine to complete
	s.cogneeWg.Wait()

	if s.improveState.banks["bank_a"].retainsSince != 4 {
		t.Fatalf("bank_a retainsSince should be 4, got %d", s.improveState.banks["bank_a"].retainsSince)
	}
	if s.improveState.banks["bank_b"].retainsSince != 0 {
		t.Fatalf("bank_b retainsSince should be 0 (reset after fire), got %d", s.improveState.banks["bank_b"].retainsSince)
	}
}

func TestMaybeAutoImprove_ConcurrentSafety(t *testing.T) {
	dir := t.TempDir()
	s := testServer(dir, Config{
		AutoImproveAfterN:  10000, // high threshold — no fires
		AutoImproveCooldown: 0,
	})

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				s.maybeAutoImprove("bank")
			}
		}()
	}
	wg.Wait()

	if s.improveState.banks["bank"].retainsSince != 1000 {
		t.Fatalf("expected retainsSince=1000, got %d", s.improveState.banks["bank"].retainsSince)
	}
}

// testLogger returns a logger backed by a bytes.Buffer for tests.
func testLogger() *logger.Logger {
	l, err := logger.NewBuf("test", "error", &bytes.Buffer{})
	if err != nil {
		l, _ = logger.New("test", "error")
	}
	return l
}

// testCounter returns a no-op counter for tests.
func testCounter() *metrics.Counter {
	return metrics.NewCounter("test")
}
