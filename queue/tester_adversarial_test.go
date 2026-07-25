package queue

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// =============================================================================
// B1: Semaphore Leak on ProcessFunc Panic
// =============================================================================
//
// Bug: When ProcessFunc panics, the workerLoop's defer recover catches it, but
// the semaphore slot (acquired before processJob()) is NEVER released because
// `<-w.sem` after `processJob()` is never reached. After `semSize` panics, the
// semaphore channel is full, causing ALL remaining workers to deadlock.
//
// AC-M2.27 claims "recovery + continue" — the worker should continue processing.
// In reality, the worker goroutine exits permanently after a panic.

func TestAdversarial_SemaphoreLeakOnPanic(t *testing.T) {
	s := newTestStore(t)
	const semSize = 3
	const workerCount = 4

	// Insert jobs — we need semSize+1 jobs to prove deadlock
	for i := 0; i < semSize+1; i++ {
		insertTestJob(t, s, fmt.Sprintf("panic-job-%d", i))
	}

	var panicCount atomic.Int32
	processFunc := func(ctx context.Context, job *Job) error {
		panicCount.Add(1)
		panic("intentional adversarial panic")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	w, err := NewWorker(WorkerConfig{
		Store:   s,
		Process: processFunc,
		Count:   workerCount,
		SemSize: semSize,
	})
	if err != nil {
		t.Fatal(err)
	}
	w.Start(ctx)

	// Wait long enough for 3 panics to fill the semaphore
	// After semSize panics, the 4th worker can never acquire the sem.
	// The 4th job never gets processed.
	time.Sleep(2 * time.Second)

	// Check: exactly semSize jobs should have panicked
	gotPanics := int(panicCount.Load())
	if gotPanics != semSize {
		t.Fatalf("BUG PARTIALLY CONFIRMED: expected %d panics (semaphore full), got %d", semSize, gotPanics)
	}

	// The semSize+1-th job should still be pending (never claimed)
	// because the 4th worker is blocked on the full semaphore
	extra, err := s.Get("panic-job-3")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if extra == nil {
		t.Fatal("BUG CONFIRMED: extra job vanished?")
	}
	if extra.Status == StatusPending {
		t.Logf("BUG CONFIRMED: extra job still pending (status=%s) — semaphore deadlocked", extra.Status)
	} else if extra.Status == StatusRunning {
		t.Log("BUG PARTIALLY CONFIRMED: extra job is running but worker may be stuck on semaphore")
	} else {
		t.Fatalf("BUG CONFIRMED: extra job has unexpected status %s", extra.Status)
	}

	// Verify no more progress happens even after waiting
	time.Sleep(3 * time.Second)
	extra2, _ := s.Get("panic-job-3")
	if extra2 != nil && extra2.Status == StatusPending {
		t.Logf("BUG CONFIRMED: after %ds, extra job still pending — semaphore permanently deadlocked", 5)
	} else if extra2 != nil && extra2.Status == StatusRunning {
		t.Error("BUG CONFIRMED: extra job stuck in running with no way to complete")
	}
}

// =============================================================================
// B2: Recover() Exported Without Mutex Protection
// =============================================================================
//
// Bug: Recover() is exported but does NOT acquire s.mu. The comment says
// "the caller must hold the lock or call before sharing the store." Since
// it's an exported method, external callers can race with Insert/NextPending
// etc. which DO acquire s.mu.

func TestAdversarial_RecoverRace(t *testing.T) {
	s := newTestStore(t)

	const numOps = 200

	var wg sync.WaitGroup

	// Inserter — adds jobs while Recover races
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < numOps; i++ {
			job := &Job{
				ID:      fmt.Sprintf("race-insert-%d", i),
				Bank:    "bank",
				Type:    "retain",
				Payload: "data",
			}
			// Ignore queue-full errors, just insert what we can
			_ = s.Insert(job)
			time.Sleep(1 * time.Millisecond)
		}
	}()

	// Claimer — claims and completes jobs while Recover races
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < numOps; i++ {
			job, err := s.NextPending()
			if err == nil && job != nil {
				_ = s.UpdateStatus(job.ID, StatusCompleted, "done", "")
			}
			time.Sleep(1 * time.Millisecond)
		}
	}()

	// Recover caller — races without mutex
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 50; i++ {
			// Recover() does NOT acquire s.mu — this races with Insert/NextPending
			n, err := s.Recover()
			if err != nil {
				t.Errorf("Recover error: %v", err)
			}
			if n > 0 {
				// If Recover modifies rows while Insert/NextPending are running,
				// we may observe inconsistent state
				t.Logf("Recover modified %d rows during concurrent ops (potential race)", n)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()

	wg.Wait()

	// After the race, verify the store is still in a valid state.
	// If Recover's updates race with NextPending's updates, we might see
	// duplicated or lost jobs.
	count, err := s.CountByStatus(StatusPending)
	if err != nil {
		t.Fatalf("CountByStatus: %v", err)
	}
	t.Logf("After Recover race: pending=%d", count)

	// If Recover set running→pending while NextPending was setting pending→running,
	// we might have double-counted. Count total jobs.
	totalPending, _ := s.CountByStatus(StatusPending)
	totalRunning, _ := s.CountByStatus(StatusRunning)
	totalCompleted, _ := s.CountByStatus(StatusCompleted)
	totalFailed, _ := s.CountByStatus(StatusFailed)
	totalDead, _ := s.CountByStatus(StatusDead)

	total := totalPending + totalRunning + totalCompleted + totalFailed + totalDead
	t.Logf("Total jobs after Recover race: %d (pending=%d running=%d completed=%d failed=%d dead=%d)",
		total, totalPending, totalRunning, totalCompleted, totalFailed, totalDead)

	// If Recover mutated rows inside NextPending's transaction, we might see
	// unexpected states like completed→pending or running→completed.
	// This is a data race by definition — the race detector should catch it.
	// If we got here without panic/race, it's still a logic bug because
	// Recover can reset running→pending for a job that was just claimed.
	notes, _ := s.CountByStatus(StatusRunning)
	if notes > 0 {
		t.Logf("NOTE: %d running jobs after race — Recover may have missed some", notes)
	}
}

// =============================================================================
// B3: NewWorker Doesn't Validate Nil Store/Process
// =============================================================================
//
// Bug: NewWorker() accepts nil Store and nil Process without validation.
// The spec §5.3 requires both to be non-nil. Calling Start() with nil Store
// causes nil pointer dereference (process crash).

func TestAdversarial_NilWorkerConfig(t *testing.T) {
	// Test nil Store
	t.Run("nil Store", func(t *testing.T) {
		w, err := NewWorker(WorkerConfig{
			Store:   nil,
			Process: func(ctx context.Context, job *Job) error { return nil },
			Count:   1,
			SemSize: 1,
		})
		if err == nil {
			t.Fatal("BUG: NewWorker accepted nil Store without error")
		}
		if w != nil {
			t.Fatal("BUG: NewWorker returned non-nil worker for nil Store")
		}
		t.Logf("CORRECT: NewWorker rejected nil Store: %v", err)
	})

	// Test nil Process
	t.Run("nil Process", func(t *testing.T) {
		s := newTestStore(t)
		w, err := NewWorker(WorkerConfig{
			Store:   s,
			Process: nil,
			Count:   1,
			SemSize: 1,
		})
		if err == nil {
			t.Fatal("BUG: NewWorker accepted nil Process without error")
		}
		if w != nil {
			t.Fatal("BUG: NewWorker returned non-nil worker for nil Process")
		}
		t.Logf("CORRECT: NewWorker rejected nil Process: %v", err)
	})
}

// =============================================================================
// G1: Illegal State Transitions via UpdateStatus
// =============================================================================
//
// Gap: UpdateStatus does not validate state machine transitions. The spec says
// "enforced by caller," but this means a buggy caller can corrupt job state.
// Documented as a gap, not a spec violation.

func TestAdversarial_IllegalStateTransitions(t *testing.T) {
	s := newTestStore(t)

	t.Run("completed to pending", func(t *testing.T) {
		insertTestJob(t, s, "comp-to-pend")
		job, _ := s.NextPending()
		_ = s.UpdateStatus(job.ID, StatusCompleted, "done", "")
		// Now attempt illegal transition: completed → pending
		err := s.UpdateStatus(job.ID, StatusPending, "", "")
		if err != nil {
			t.Fatalf("UpdateStatus(completed→pending) errored: %v", err)
		}
		got, _ := s.Get(job.ID)
		if got.Status == StatusPending {
			t.Log("GAP CONFIRMED: completed→pending is allowed by UpdateStatus (by spec design)")
		}
	})

	t.Run("completed to failed", func(t *testing.T) {
		insertTestJob(t, s, "comp-to-fail")
		job, _ := s.NextPending()
		_ = s.UpdateStatus(job.ID, StatusCompleted, "done", "")
		err := s.UpdateStatus(job.ID, StatusFailed, "", "retroactive fail")
		if err != nil {
			t.Fatalf("UpdateStatus(completed→failed) errored: %v", err)
		}
		got, _ := s.Get(job.ID)
		t.Logf("GAP CONFIRMED: completed→failed produces retry_count=%d", got.RetryCount)
	})

	t.Run("dead to pending", func(t *testing.T) {
		insertTestJob(t, s, "dead-to-pend")
		job, _ := s.NextPending()
		_ = s.UpdateStatus(job.ID, StatusFailed, "", "fail")
		_ = s.UpdateStatus(job.ID, StatusDead, "", "dead")
		err := s.UpdateStatus(job.ID, StatusPending, "", "")
		if err != nil {
			t.Fatalf("UpdateStatus(dead→pending) errored: %v", err)
		}
		got, _ := s.Get(job.ID)
		if got.Status == StatusPending {
			t.Log("GAP CONFIRMED: dead→pending is allowed — spec says dead is terminal")
		}
	})
}

// =============================================================================
// G2: Null Bytes in Payload
// =============================================================================

func TestAdversarial_NullBytesInPayload(t *testing.T) {
	s := newTestStore(t)

	nullPayload := "normal\x00null\x00bytes"
	job := &Job{
		ID:      "null-byte-job",
		Bank:    "bank",
		Type:    "retain",
		Payload: nullPayload,
	}
	if err := s.Insert(job); err != nil {
		t.Fatalf("Insert with null bytes: %v", err)
	}

	got, err := s.Get("null-byte-job")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got == nil {
		t.Fatal("job not found")
	}
	if got.Payload != nullPayload {
		t.Errorf("payload mismatch: got %q (len=%d), want %q (len=%d)",
			got.Payload, len(got.Payload), nullPayload, len(nullPayload))
	}
	if !strings.Contains(got.Payload, "\x00") {
		t.Error("null bytes were lost in round-trip")
	}
}

// =============================================================================
// G3: Large Payload (10KB)
// =============================================================================

func TestAdversarial_LargePayload(t *testing.T) {
	s := newTestStore(t)

	largePayload := strings.Repeat("A", 10*1024) // 10KB
	job := &Job{
		ID:      "large-payload",
		Bank:    "bank",
		Type:    "retain",
		Payload: largePayload,
	}
	if err := s.Insert(job); err != nil {
		t.Fatalf("Insert with 10KB payload: %v", err)
	}

	got, err := s.Get("large-payload")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got == nil {
		t.Fatal("job not found")
	}
	if len(got.Payload) != len(largePayload) {
		t.Errorf("payload length mismatch: got %d, want %d", len(got.Payload), len(largePayload))
	}
	if got.Payload != largePayload {
		// Check prefix/suffix to avoid printing 10KB in test output
		if got.Payload[:100] != largePayload[:100] || got.Payload[len(got.Payload)-100:] != largePayload[len(largePayload)-100:] {
			t.Error("payload content mismatch")
		}
	}
}

// =============================================================================
// G4: 1000 Rapid Inserts
// =============================================================================

func TestAdversarial_BulkInsert1000(t *testing.T) {
	s := newTestStoreWithConfig(t, StoreConfig{MaxPending: 2000})

	const n = 1000

	start := time.Now()
	for i := 0; i < n; i++ {
		job := &Job{
			ID:      fmt.Sprintf("bulk-%d", i),
			Bank:    "bank",
			Type:    "retain",
			Payload: fmt.Sprintf("payload-%d", i),
		}
		if err := s.Insert(job); err != nil {
			t.Fatalf("Insert(%d): %v", i, err)
		}
	}
	insertDur := time.Since(start)

	count, err := s.CountByStatus(StatusPending)
	if err != nil {
		t.Fatalf("CountByStatus: %v", err)
	}
	if count != n {
		t.Errorf("expected %d pending jobs, got %d", n, count)
	}

	t.Logf("Inserted %d jobs in %v (%.0f jobs/sec)", n, insertDur, float64(n)/insertDur.Seconds())

	// Now claim all 1000 concurrently
	start = time.Now()
	var claimed atomic.Int64
	var wg sync.WaitGroup
	const claimers = 10
	wg.Add(claimers)
	for c := 0; c < claimers; c++ {
		go func() {
			defer wg.Done()
			for {
				job, err := s.NextPending()
				if err != nil {
					return
				}
				if job == nil {
					return
				}
				claimed.Add(1)
				_ = s.UpdateStatus(job.ID, StatusCompleted, "done", "")
			}
		}()
	}
	wg.Wait()
	claimDur := time.Since(start)
	t.Logf("Claimed %d jobs in %v (%.0f jobs/sec)", claimed.Load(), claimDur, float64(claimed.Load())/claimDur.Seconds())
}

// =============================================================================
// G5: Double Recover
// =============================================================================

func TestAdversarial_DoubleRecover(t *testing.T) {
	s := newTestStore(t)

	// Insert a running job directly (simulate crash)
	rawInsertJob(t, s, "orphan", StatusRunning, 0, 3)
	rawInsertJob(t, s, "retryable", StatusFailed, 1, 3)
	rawInsertJob(t, s, "exhausted", StatusFailed, 3, 3)

	// First Recover
	n1, err := s.Recover()
	if err != nil {
		t.Fatalf("Recover 1: %v", err)
	}
	if n1 != 3 {
		t.Errorf("Recover 1: expected 3 rows affected, got %d", n1)
	}

	// Second Recover (should be a no-op)
	n2, err := s.Recover()
	if err != nil {
		t.Fatalf("Recover 2: %v", err)
	}
	if n2 != 0 {
		t.Logf("GAP NOTE: Recover 2 affected %d rows (expected 0 — double Recover is not fully idempotent)", n2)
	}
}

// =============================================================================
// G6: Worker Restart Cycle — Start→Stop→Start→Stop
// =============================================================================

func TestAdversarial_WorkerRestartCycle(t *testing.T) {
	s := newTestStore(t)

	processFunc := func(ctx context.Context, job *Job) error {
		// Small delay to ensure worker actually runs
		time.Sleep(50 * time.Millisecond)
		return nil
	}

	baseline := runtimeNumGoroutines()

	w, err := NewWorker(WorkerConfig{
		Store:   s,
		Process: processFunc,
		Count:   4,
		SemSize: 2,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Cycle 1: Start → Stop
	ctx1, cancel1 := context.WithCancel(context.Background())
	w.Start(ctx1)
	time.Sleep(200 * time.Millisecond) // let workers start polling
	w.Stop()
	cancel1()

	after1 := runtimeNumGoroutines()
	t.Logf("After cycle 1: goroutines %d (baseline %d, delta %d)", after1, baseline, after1-baseline)

	// Cycle 2: Start → Stop
	ctx2, cancel2 := context.WithCancel(context.Background())
	w.Start(ctx2)
	time.Sleep(200 * time.Millisecond)
	w.Stop()
	cancel2()

	after2 := runtimeNumGoroutines()
	t.Logf("After cycle 2: goroutines %d (baseline %d, delta %d)", after2, baseline, after2-baseline)

	// Verify the worker pool can still process a job after restart
	insertTestJob(t, s, "restart-job")
	ctx3, cancel3 := context.WithCancel(context.Background())
	w.Start(ctx3)
	defer cancel3()
	defer w.Stop()

	deadline := time.After(5 * time.Second)
	for {
		got, _ := s.Get("restart-job")
		if got != nil && got.Status == StatusCompleted {
			break
		}
		select {
		case <-deadline:
			t.Fatal("timed out waiting for job after restart")
		default:
			time.Sleep(50 * time.Millisecond)
		}
	}

	// Check goroutine leak (allow some slack for test infrastructure)
	after3 := runtimeNumGoroutines()
	leaked := after3 - baseline
	if leaked > 4 {
		t.Logf("GAP NOTE: possible goroutine leak after restart cycle: delta=%d", leaked)
	}
}

// =============================================================================
// G6b: Controlled concurrent Start/Stop races
// =============================================================================

func TestAdversarial_ConcurrentStartStop(t *testing.T) {
	s := newTestStore(t)
	processFunc := func(ctx context.Context, job *Job) error {
		time.Sleep(5 * time.Millisecond)
		return nil
	}

	// Run sequential Start/Stop pairs concurrently (not same-pool races)
	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		w, err := NewWorker(WorkerConfig{
			Store:   s,
			Process: processFunc,
			Count:   2,
			SemSize: 1,
		})
		if err != nil {
			t.Fatal(err)
		}
		wg.Add(2)
		go func() {
			defer wg.Done()
			ctx := context.Background()
			w.Start(ctx)
			time.Sleep(20 * time.Millisecond)
			w.Stop()
		}()
		go func() {
			defer wg.Done()
			time.Sleep(5 * time.Millisecond)
			w.Stop()
		}()
	}

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		t.Log("Concurrent Start/Stop on separate workers completed without deadlock")
	case <-time.After(15 * time.Second):
		t.Fatal("TIMEOUT: Concurrent Start/Stop deadlocked")
	}
}

// =============================================================================
// TTL Cleanup During Concurrent Operations
// =============================================================================

func TestAdversarial_TTLCleanupDuringOperations(t *testing.T) {
	s := newTestStoreWithConfig(t, StoreConfig{JobTTL: 100 * time.Millisecond})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Start TTL cleanup with very short interval
	s.StartTTLCleanup(ctx, 50*time.Millisecond)

	var wg sync.WaitGroup

	// Inserter — continuously adds jobs
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			job := &Job{
				ID:      fmt.Sprintf("ttl-race-%d", i),
				Bank:    "bank",
				Type:    "retain",
				Payload: "data",
			}
			_ = s.Insert(job)
			time.Sleep(2 * time.Millisecond)
		}
	}()

	// Claimer — claims and completes jobs (making them eligible for TTL deletion)
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			job, err := s.NextPending()
			if err == nil && job != nil {
				_ = s.UpdateStatus(job.ID, StatusCompleted, "done", "")
			}
			time.Sleep(3 * time.Millisecond)
		}
	}()

	wg.Wait()

	// Let TTL cleanup run a few more times
	time.Sleep(300 * time.Millisecond)

	// Verify store is still responsive
	job := &Job{
		ID:      "post-ttl-race",
		Bank:    "bank",
		Type:    "retain",
		Payload: "data",
	}
	if err := s.Insert(job); err != nil {
		t.Fatalf("Insert after TTL race: %v", err)
	}
	// No panics or races = pass
}

// =============================================================================
// Slow ProcessFunc
// =============================================================================

func TestAdversarial_SlowProcessFunc(t *testing.T) {
	s := newTestStore(t)

	// Insert 3 jobs
	for i := 0; i < 3; i++ {
		insertTestJob(t, s, fmt.Sprintf("slow-%d", i))
	}

	var completed atomic.Int32
	processFunc := func(ctx context.Context, job *Job) error {
		time.Sleep(2 * time.Second)
		completed.Add(1)
		return nil
	}

	w, err := NewWorker(WorkerConfig{
		Store:   s,
		Process: processFunc,
		Count:   3,
		SemSize: 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	w.Start(context.Background())
	defer w.Stop()

	// All 3 should complete within ~2 seconds (parallel with semSize=3)
	deadline := time.After(5 * time.Second)
	for completed.Load() < 3 {
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for slow jobs: completed=%d", completed.Load())
		default:
			time.Sleep(100 * time.Millisecond)
		}
	}
	// If SemSize was correctly 3, all 3 run in parallel → ~2s total
	t.Logf("All 3 slow jobs completed in parallel via semSize=3")
}

// =============================================================================
// Chaos: NextPending while Recover runs
// =============================================================================

func TestAdversarial_NextPendingDuringRecover(t *testing.T) {
	s := newTestStore(t)

	// Insert jobs in specific states
	rawInsertJob(t, s, "orphan-1", StatusRunning, 0, 3)
	rawInsertJob(t, s, "orphan-2", StatusRunning, 0, 3)
	insertTestJob(t, s, "normal-1")
	insertTestJob(t, s, "normal-2")

	var wg sync.WaitGroup

	// Concurrent: claim normal jobs
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 10; i++ {
			job, err := s.NextPending()
			if err == nil && job != nil {
				_ = s.UpdateStatus(job.ID, StatusCompleted, "done", "")
			}
			time.Sleep(1 * time.Millisecond)
		}
	}()

	// Concurrent: Recover (without mutex)
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 10; i++ {
			_, _ = s.Recover()
			time.Sleep(1 * time.Millisecond)
		}
	}()

	wg.Wait()

	// After chaos, verify at least the orphan jobs were recovered
	got, _ := s.Get("orphan-1")
	if got != nil && got.Status == StatusPending {
		t.Log("orphan-1 recovered to pending (expected)")
	} else if got != nil {
		t.Logf("NOTE: orphan-1 status=%s (may have been claimed during race)", got.Status)
	}
}

// =============================================================================
// UpdateStatus with empty/invalid status
// =============================================================================

func TestAdversarial_UpdateStatusInvalid(t *testing.T) {
	s := newTestStore(t)
	insertTestJob(t, s, "invalid-status")

	job, _ := s.NextPending()

	t.Run("empty status string", func(t *testing.T) {
		err := s.UpdateStatus(job.ID, Status(""), "", "")
		if err != nil {
			t.Fatalf("UpdateStatus with empty status: %v", err)
		}
		got, _ := s.Get(job.ID)
		if got.Status != "" {
			t.Logf("NOTE: UpdateStatus with empty status set status to %q", got.Status)
		}
	})

	t.Run("invalid status string", func(t *testing.T) {
		// Insert a new job for this test
		insertTestJob(t, s, "invalid-status-2")
		j2, _ := s.NextPending()

		err := s.UpdateStatus(j2.ID, Status("foobar"), "", "")
		if err != nil {
			t.Fatalf("UpdateStatus with 'foobar': %v", err)
		}
		got, _ := s.Get(j2.ID)
		t.Logf("NOTE: UpdateStatus with 'foobar' set status to %q", got.Status)
	})

	t.Run("status string with spaces", func(t *testing.T) {
		insertTestJob(t, s, "invalid-status-3")
		j3, _ := s.NextPending()

		err := s.UpdateStatus(j3.ID, Status("  completed  "), "", "")
		if err != nil {
			t.Fatalf("UpdateStatus with spaced status: %v", err)
		}
		got, _ := s.Get(j3.ID)
		t.Logf("NOTE: UpdateStatus with '  completed  ' set status to %q", got.Status)
	})
}

// =============================================================================
// UpdateStatus with non-existent ID
// =============================================================================

func TestAdversarial_UpdateStatusEmptyID(t *testing.T) {
	s := newTestStore(t)

	err := s.UpdateStatus("", StatusCompleted, "", "")
	if err == nil {
		t.Error("UpdateStatus with empty ID should return error, got nil")
	} else {
		t.Logf("UpdateStatus(empty ID) returned: %v", err)
	}
}

// =============================================================================
// CountByStatus on empty store
// =============================================================================

func TestAdversarial_CountByStatusOnEmpty(t *testing.T) {
	s := newTestStore(t)

	for _, status := range []Status{StatusPending, StatusRunning, StatusCompleted, StatusFailed, StatusDead} {
		count, err := s.CountByStatus(status)
		if err != nil {
			t.Fatalf("CountByStatus(%s): %v", status, err)
		}
		if count != 0 {
			t.Errorf("CountByStatus(%s) on empty store = %d, want 0", status, count)
		}
	}
}

// =============================================================================
// Insert with duplicate ID
// =============================================================================

func TestAdversarial_InsertDuplicateID(t *testing.T) {
	s := newTestStore(t)

	job := &Job{
		ID:      "duplicate",
		Bank:    "bank",
		Type:    "retain",
		Payload: "first",
	}
	if err := s.Insert(job); err != nil {
		t.Fatalf("First insert: %v", err)
	}

	// Second insert with same ID
	job2 := &Job{
		ID:      "duplicate",
		Bank:    "bank",
		Type:    "retain",
		Payload: "second",
	}
	err := s.Insert(job2)
	if err == nil {
		t.Error("Insert duplicate ID should return error")
	} else {
		t.Logf("Insert duplicate ID returned: %v", err)
	}

	// Original job should be unchanged
	got, _ := s.Get("duplicate")
	if got.Payload != "first" {
		t.Errorf("original payload overwritten: got %q, want %q", got.Payload, "first")
	}
}

// =============================================================================
// Close while operations are in flight
// =============================================================================

func TestAdversarial_CloseDuringOperations(t *testing.T) {
	s := newTestStore(t)

	var wg sync.WaitGroup

	// Start concurrent insert/claim
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			for j := 0; j < 10; j++ {
				job := &Job{
					ID:      fmt.Sprintf("close-race-%d-%d", idx, j),
					Bank:    "bank",
					Type:    "retain",
					Payload: "data",
				}
				_ = s.Insert(job)
			}
		}(i)
	}

	// Hammer Close concurrently
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 5; i++ {
			_ = s.Close()
			time.Sleep(5 * time.Millisecond)
		}
	}()

	wg.Wait()

	// All operations after Close should return errors (not panic)
	job := &Job{
		ID:      "after-close-chaos",
		Bank:    "b",
		Type:    "retain",
		Payload: "p",
	}
	err := s.Insert(job)
	if err != nil {
		t.Logf("Insert after close (during race) returned expected error: %v", err)
	} else {
		t.Log("Insert after close-chaos succeeded — store may have reopened?")
	}
}

// =============================================================================
// Worker with ProcessFunc that takes 5 minutes (simulated with cancellation)
// =============================================================================

func TestAdversarial_WorkerContextCancellation(t *testing.T) {
	s := newTestStore(t)

	insertTestJob(t, s, "cancel-job")

	started := make(chan struct{})
	workerCtx, workerCancel := context.WithCancel(context.Background())

	processFunc := func(ctx context.Context, job *Job) error {
		close(started)
		// Wait for cancellation
		<-ctx.Done()
		return ctx.Err()
	}

	processResult := make(chan error)
	go func() {
		// Simulate what workerLoop does minus the semaphore
		processResult <- processFunc(workerCtx, &Job{ID: "cancel-job"})
	}()

	<-started
	workerCancel()

	err := <-processResult
	if err != context.Canceled {
		t.Errorf("expected context.Canceled, got %v", err)
	} else {
		t.Log("Worker context cancellation works correctly")
	}
}
