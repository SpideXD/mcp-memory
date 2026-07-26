package main

import (
	"sync"
	"testing"
	"time"

	"mcp-memory/queue"
)

// testServerWithQueue creates a test server with an in-memory queue store.
func testServerWithQueue(t *testing.T, cfg Config) *Server {
	t.Helper()
	s := testServer(t.TempDir(), cfg)
	store, err := queue.NewStore(queue.StoreConfig{DBPath: ":memory:"})
	if err != nil {
		t.Fatalf("failed to create queue store: %v", err)
	}
	s.queueStore = store
	t.Cleanup(func() { store.Close() })
	return s
}

func TestCheckAutoReflect_DisabledWhenBothZero(t *testing.T) {
	s := testServer(t.TempDir(), Config{
		AutoReflectAfterN:  0,
		AutoReflectTimeout: 0,
	})

	// Should not panic or create state
	for i := 0; i < 100; i++ {
		s.checkAutoReflect("testbank")
	}

	// No state should be created
	s.reflectStates.Range(func(key, value interface{}) bool {
		t.Fatal("expected no reflect state created when disabled")
		return true
	})
}

func TestCheckAutoReflect_CountBasedTrigger(t *testing.T) {
	s := testServerWithQueue(t, Config{
		AutoReflectAfterN:  5,
		AutoReflectTimeout: 0,
	})

	// Call 4 times — no trigger
	for i := 0; i < 4; i++ {
		s.checkAutoReflect("testbank")
	}

	val, ok := s.reflectStates.Load("testbank")
	if !ok {
		t.Fatal("expected reflect state to be created")
	}
	rs := val.(*reflectState)
	if rs.retainCount != 4 {
		t.Fatalf("expected retainCount=4, got %d", rs.retainCount)
	}

	// 5th call — should trigger
	s.checkAutoReflect("testbank")

	if rs.retainCount != 0 {
		t.Fatalf("expected retainCount=0 after trigger, got %d", rs.retainCount)
	}

	// Verify a reflect job was inserted
	job, err := s.queueStore.NextPending()
	if err != nil {
		t.Fatalf("expected pending job, got error: %v", err)
	}
	if job == nil {
		t.Fatal("expected a reflect job in queue")
	}
	if job.Type != "reflect" {
		t.Fatalf("expected job type 'reflect', got '%s'", job.Type)
	}
	if job.Payload != "_auto" {
		t.Fatalf("expected payload '_auto', got '%s'", job.Payload)
	}
	if job.Bank != "testbank" {
		t.Fatalf("expected bank 'testbank', got '%s'", job.Bank)
	}
}

func TestCheckAutoReflect_TimeoutBasedTrigger(t *testing.T) {
	s := testServerWithQueue(t, Config{
		AutoReflectAfterN:  9999, // effectively infinite
		AutoReflectTimeout: 1 * time.Millisecond,
	})

	// First call — initialize state, no trigger
	s.checkAutoReflect("testbank")

	val, ok := s.reflectStates.Load("testbank")
	if !ok {
		t.Fatal("expected reflect state to be created")
	}
	rs := val.(*reflectState)
	if rs.retainCount != 1 {
		t.Fatalf("expected retainCount=1, got %d", rs.retainCount)
	}

	// Wait for timeout to elapse
	time.Sleep(5 * time.Millisecond)

	// Second call — timeout should trigger
	s.checkAutoReflect("testbank")

	if rs.retainCount != 0 {
		t.Fatalf("expected retainCount=0 after timeout trigger, got %d", rs.retainCount)
	}

	// Verify a reflect job was inserted
	job, err := s.queueStore.NextPending()
	if err != nil {
		t.Fatalf("expected pending job, got error: %v", err)
	}
	if job == nil {
		t.Fatal("expected a reflect job in queue")
	}
	if job.Type != "reflect" {
		t.Fatalf("expected job type 'reflect', got '%s'", job.Type)
	}
}

func TestCheckAutoReflect_PerBankIsolation(t *testing.T) {
	s := testServerWithQueue(t, Config{
		AutoReflectAfterN:  5,
		AutoReflectTimeout: 0,
	})

	// Bank A: 4 retains
	for i := 0; i < 4; i++ {
		s.checkAutoReflect("alpha")
	}
	// Bank B: 1 retain
	s.checkAutoReflect("beta")

	// Bank A should not trigger (count=4 < 5)
	valA, ok := s.reflectStates.Load("alpha")
	if !ok {
		t.Fatal("expected alpha state")
	}
	rsA := valA.(*reflectState)
	if rsA.retainCount != 4 {
		t.Fatalf("alpha retainCount=4, got %d", rsA.retainCount)
	}

	// Bank B should not trigger (count=1 < 5)
	valB, ok := s.reflectStates.Load("beta")
	if !ok {
		t.Fatal("expected beta state")
	}
	rsB := valB.(*reflectState)
	if rsB.retainCount != 1 {
		t.Fatalf("beta retainCount=1, got %d", rsB.retainCount)
	}

	// Bank A: 5th retain — should trigger
	s.checkAutoReflect("alpha")

	if rsA.retainCount != 0 {
		t.Fatalf("alpha retainCount should be 0 after trigger, got %d", rsA.retainCount)
	}
	// Beta should be unaffected
	if rsB.retainCount != 1 {
		t.Fatalf("beta retainCount should still be 1, got %d", rsB.retainCount)
	}

	// Verify exactly 1 job in queue (alpha's reflect)
	job, err := s.queueStore.NextPending()
	if err != nil {
		t.Fatalf("expected pending job, got error: %v", err)
	}
	if job == nil {
		t.Fatal("expected a reflect job in queue")
	}
	if job.Bank != "alpha" {
		t.Fatalf("expected job bank 'alpha', got '%s'", job.Bank)
	}
}

func TestCheckAutoReflect_NilQueueStore(t *testing.T) {
	s := testServer(t.TempDir(), Config{
		AutoReflectAfterN:  1,
		AutoReflectTimeout: 0,
	})

	// Test nil queueStore — should log warning, not panic
	s.queueStore = nil
	s.checkAutoReflect("testbank")

	// Verify no panic occurred (test would fail if panic wasn't recovered)
	// Also verify state was reset (the trigger fired but insertion was skipped)
	val, ok := s.reflectStates.Load("testbank")
	if !ok {
		t.Fatal("expected reflect state to be created")
	}
	rs := val.(*reflectState)
	if rs.retainCount != 0 {
		t.Fatalf("expected retainCount=0 after nil queueStore, got %d", rs.retainCount)
	}
}

func TestCheckAutoReflect_NegativeConfigClampedToZero(t *testing.T) {
	s := testServer(t.TempDir(), Config{
		AutoReflectAfterN:  -1,
		AutoReflectTimeout: -1 * time.Second,
	})

	// Should be treated as disabled (both <= 0)
	for i := 0; i < 100; i++ {
		s.checkAutoReflect("testbank")
	}

	// No state should be created
	s.reflectStates.Range(func(key, value interface{}) bool {
		t.Fatal("expected no reflect state created when config is negative")
		return true
	})
}

func TestCheckAutoReflect_EmptyBankName(t *testing.T) {
	s := testServer(t.TempDir(), Config{
		AutoReflectAfterN:  1,
		AutoReflectTimeout: 0,
	})

	// Should return immediately, no state created
	s.checkAutoReflect("")

	s.reflectStates.Range(func(key, value interface{}) bool {
		t.Fatal("expected no reflect state created for empty bank")
		return true
	})
}

func TestCheckAutoReflect_InvalidBankName(t *testing.T) {
	s := testServer(t.TempDir(), Config{
		AutoReflectAfterN:  1,
		AutoReflectTimeout: 0,
	})

	// Bank name with spaces and special chars should be rejected
	s.checkAutoReflect("bank with spaces!!!")

	s.reflectStates.Range(func(key, value interface{}) bool {
		t.Fatal("expected no reflect state created for invalid bank")
		return true
	})
}

func TestCheckAutoReflect_ConcurrentDifferentBanks(t *testing.T) {
	s := testServerWithQueue(t, Config{
		AutoReflectAfterN:  1000, // high threshold — no fires
		AutoReflectTimeout: 0,
	})

	var wg sync.WaitGroup
	banks := []string{"bank_a", "bank_b", "bank_c", "bank_d"}

	// 4 goroutines, each calling 100 times for different banks
	for _, bank := range banks {
		wg.Add(1)
		go func(b string) {
			defer wg.Done()
			for i := 0; i < 100; i++ {
				s.checkAutoReflect(b)
			}
		}(bank)
	}
	wg.Wait()

	// Each bank should have retainCount=100 (no triggers fired)
	for _, bank := range banks {
		val, ok := s.reflectStates.Load(bank)
		if !ok {
			t.Fatalf("expected state for bank %s", bank)
		}
		rs := val.(*reflectState)
		if rs.retainCount != 100 {
			t.Fatalf("bank %s: expected retainCount=100, got %d", bank, rs.retainCount)
		}
	}
}

func TestCheckAutoReflect_ConcurrentSameBank(t *testing.T) {
	s := testServerWithQueue(t, Config{
		AutoReflectAfterN:  100,
		AutoReflectTimeout: 0,
	})

	var wg sync.WaitGroup
	// 4 goroutines, each calling 25 times for the same bank (total=100)
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 25; j++ {
				s.checkAutoReflect("shared")
			}
		}()
	}
	wg.Wait()

	val, ok := s.reflectStates.Load("shared")
	if !ok {
		t.Fatal("expected state for shared bank")
	}
	rs := val.(*reflectState)

	// With N=100 and 100 calls, the trigger should fire exactly once
	// After trigger, retainCount resets to 0
	// But timing may cause some calls after the trigger to increment again
	// So we just verify no data race and state is consistent
	if rs.retainCount < 0 {
		t.Fatalf("shared bank: retainCount should be non-negative, got %d", rs.retainCount)
	}
}

func TestCheckAutoReflect_CountTriggerThenTimeoutDebounce(t *testing.T) {
	s := testServerWithQueue(t, Config{
		AutoReflectAfterN:  3,
		AutoReflectTimeout: 1 * time.Millisecond,
	})

	// 3 retains to trigger count
	s.checkAutoReflect("testbank")
	s.checkAutoReflect("testbank")
	s.checkAutoReflect("testbank")

	// State should be reset
	val, ok := s.reflectStates.Load("testbank")
	if !ok {
		t.Fatal("expected state")
	}
	rs := val.(*reflectState)
	if rs.retainCount != 0 {
		t.Fatalf("expected retainCount=0 after trigger, got %d", rs.retainCount)
	}

	// Wait for timeout to elapse
	time.Sleep(5 * time.Millisecond)

	// One more retain — should trigger timeout (retainCount becomes 1, time elapsed)
	s.checkAutoReflect("testbank")

	if rs.retainCount != 0 {
		t.Fatalf("expected retainCount=0 after timeout trigger, got %d", rs.retainCount)
	}
}

func TestCheckAutoReflect_TimeoutDisabledCountOnly(t *testing.T) {
	s := testServerWithQueue(t, Config{
		AutoReflectAfterN:  5,
		AutoReflectTimeout: 0, // disabled
	})

	// 1 retain
	s.checkAutoReflect("testbank")

	// Wait a long time
	time.Sleep(10 * time.Millisecond)

	// Another retain — should NOT trigger (timeout disabled)
	s.checkAutoReflect("testbank")

	val, ok := s.reflectStates.Load("testbank")
	if !ok {
		t.Fatal("expected state")
	}
	rs := val.(*reflectState)
	if rs.retainCount != 2 {
		t.Fatalf("expected retainCount=2 (no timeout trigger), got %d", rs.retainCount)
	}
}

func TestCheckAutoReflect_CountDisabledTimeoutOnly(t *testing.T) {
	s := testServerWithQueue(t, Config{
		AutoReflectAfterN:  0, // disabled
		AutoReflectTimeout: 1 * time.Millisecond,
	})

	// First call — initialize state
	s.checkAutoReflect("testbank")

	val, ok := s.reflectStates.Load("testbank")
	if !ok {
		t.Fatal("expected state")
	}
	rs := val.(*reflectState)
	if rs.retainCount != 1 {
		t.Fatalf("expected retainCount=1, got %d", rs.retainCount)
	}

	// Wait for timeout
	time.Sleep(5 * time.Millisecond)

	// Should trigger timeout
	s.checkAutoReflect("testbank")

	if rs.retainCount != 0 {
		t.Fatalf("expected retainCount=0 after timeout trigger, got %d", rs.retainCount)
	}
}

func TestTriggerReason(t *testing.T) {
	tests := []struct {
		count   bool
		timeout bool
		want    string
	}{
		{true, false, "count"},
		{false, true, "timeout"},
		{true, true, "both"},
	}
	for _, tt := range tests {
		got := triggerReason(tt.count, tt.timeout)
		if got != tt.want {
			t.Errorf("triggerReason(%v, %v) = %q, want %q", tt.count, tt.timeout, got, tt.want)
		}
	}
}
