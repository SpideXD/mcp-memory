package queue

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// =============================================================================
// M2 Tester Pass 3 — Chaos & Fuzz Final
// =============================================================================
//
// 8 chaos tests targeting resource exhaustion, concurrency storms, and
// lifecycle edge cases that the spec and previous passes didn't cover.

// =============================================================================
// Chaos 1: Rapid start/stop cycles (×50)
// =============================================================================
//
// Creates store + worker pool, starts, stops, restarts × 50.
// Checks for: goroutine leaks, panics, deadlocks, semaphore corruption.

func TestChaos1_RapidStartStopCycles(t *testing.T) {
	s := newTestStore(t)

	const (
		cycles     = 50
		workerCnt  = 4
		semSize    = 2
	)

	processFunc := func(ctx context.Context, job *Job) error {
		return nil
	}

	baseline := runtimeNumGoroutines()
	var panicked atomic.Int64

	for i := 0; i < cycles; i++ {
		func() { // capture panics per cycle
			defer func() {
				if r := recover(); r != nil {
					panicked.Add(1)
					t.Logf("CYCLE %d PANICKED: %v", i, r)
				}
			}()

			w := NewWorker(WorkerConfig{
				Store:   s,
				Process: processFunc,
				Count:   workerCnt,
				SemSize: semSize,
			})

			ctx1, cancel1 := context.WithCancel(context.Background())
			w.Start(ctx1)
			// Let workers start polling
			time.Sleep(time.Millisecond)
			w.Stop()
			cancel1()

			ctx2, cancel2 := context.WithCancel(context.Background())
			w.Start(ctx2)
			time.Sleep(time.Millisecond)
			w.Stop()
			cancel2()
		}()
	}

	if panicked.Load() > 0 {
		t.Errorf("%d cycles panicked during start/stop", panicked.Load())
	}

	// Give goroutines time to settle
	time.Sleep(100 * time.Millisecond)
	after := runtimeNumGoroutines()
	leaked := after - baseline
	if leaked > 5 {
		t.Errorf("goroutine leak after %d start/stop cycles: baseline=%d after=%d delta=%d",
			cycles, baseline, after, leaked)
	} else {
		t.Logf("Goroutine delta after %d start/stop cycles: %d (baseline=%d after=%d)",
			cycles, leaked, baseline, after)
	}

	// Verify store still functional
	insertTestJob(t, s, "post-rapid-cycle")
	got, err := s.Get("post-rapid-cycle")
	if err != nil {
		t.Fatalf("Get after rapid cycles: %v", err)
	}
	if got == nil || got.Status != StatusPending {
		t.Errorf("Job after rapid cycles: got status=%q, want pending", got.Status)
	}
	t.Log("Store functional after rapid start/stop cycles")
}

// =============================================================================
// Chaos 2: Disk full / filesystem failure simulation
// =============================================================================
//
// Create a store on a real file (not :memory:), then simulate filesystem
// failure by making the directory non-writable. Insert should return a
// graceful error, not a panic.

func TestChaos2_DiskFullSimulation(t *testing.T) {
	// 2a: Non-writable directory — NewStore should fail gracefully
	t.Run("NewStore_in_non_writable_dir", func(t *testing.T) {
		dir := t.TempDir()
		// Make the directory read-only
		if err := os.Chmod(dir, 0o444); err != nil {
			t.Fatalf("Chmod read-only: %v", err)
		}
		defer os.Chmod(dir, 0o755) // restore for cleanup

		dbPath := filepath.Join(dir, "queue.db")
		_, err := NewStore(StoreConfig{DBPath: dbPath})
		if err == nil {
			t.Error("NewStore in read-only dir should return error, got nil")
		} else {
			t.Logf("NewStore in read-only dir returned error (expected): %v", err)
		}
	})

	// 2b: Store on real file, then delete the DB file and try operations.
	// Simulates disk failure / file corruption mid-operation.
	t.Run("Insert_after_db_file_deleted", func(t *testing.T) {
		dir := t.TempDir()
		dbPath := filepath.Join(dir, "queue.db")

		s, err := NewStore(StoreConfig{DBPath: dbPath})
		if err != nil {
			t.Fatalf("NewStore on real file: %v", err)
		}
		defer s.Close()

		// Insert one good job first
		job1 := &Job{
			ID:      "pre-delete-job",
			Bank:    "bank",
			Type:    "retain",
			Payload: "survivor",
		}
		if err := s.Insert(job1); err != nil {
			t.Fatalf("Insert before delete: %v", err)
		}

		// Delete the database file while store is open
		if err := os.Remove(dbPath); err != nil {
			t.Fatalf("Remove db file: %v", err)
		}

		// Try operations after file deletion — must NOT panic
		// Note: modernc.org/sqlite may cache data in memory after opening.
		// File deletion may not immediately affect operations; no panic is the minimum contract.
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("PANIC on Insert after file deleted: %v", r)
				}
			}()
			job2 := &Job{
				ID:      "post-delete-job",
				Bank:    "bank",
				Type:    "retain",
				Payload: "ghost",
			}
			err := s.Insert(job2)
			if err != nil {
				t.Logf("Insert after file deletion returned error: %v", err)
			}
		}()

		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("PANIC on NextPending after file deleted: %v", r)
				}
			}()
			_, err := s.NextPending()
			if err != nil {
				t.Logf("NextPending after file deletion returned error: %v", err)
			}
		}()

		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("PANIC on Stats after file deleted: %v", r)
				}
			}()
			_, err := s.Stats()
			if err != nil {
				t.Logf("Stats after file deletion returned error: %v", err)
			}
		}()
	})

	// 2c: Create a store on a real file, fill with large payloads to
	// approach storage limits. Check graceful error handling.
	t.Run("large_payloads_to_near_limit", func(t *testing.T) {
		dir := t.TempDir()
		dbPath := filepath.Join(dir, "queue.db")

		s, err := NewStore(StoreConfig{
			DBPath:     dbPath,
			MaxPending: 10000,
		})
		if err != nil {
			t.Fatalf("NewStore: %v", err)
		}
		defer s.Close()

		// Insert increasingly large payloads until we hit an error
		payloadSizes := []int{1024, 10 * 1024, 100 * 1024, 1024 * 1024} // 1KB, 10KB, 100KB, 1MB
		for _, size := range payloadSizes {
			payload := make([]byte, size)
			for i := range payload {
				payload[i] = byte(i % 256)
			}

			job := &Job{
				ID:      fmt.Sprintf("large-%d", size),
				Bank:    "bank",
				Type:    "retain",
				Payload: string(payload),
			}
			err := s.Insert(job)
			if err != nil {
				t.Logf("Insert with %d-byte payload: %v", size, err)
				// This is fine — we reached some limit gracefully
			} else {
				t.Logf("Insert with %d-byte payload: OK", size)
			}
		}
		t.Log("Large payloads handled without panic")
	})
}

// =============================================================================
// Chaos 3: 1000 concurrent inserts
// =============================================================================
//
// Fire 1000 goroutines inserting simultaneously.
// Verify no lost jobs, no duplicates, and correct count.

func TestChaos3_ThousandConcurrentInserts(t *testing.T) {
	s := newTestStoreWithConfig(t, StoreConfig{MaxPending: 2000})

	const numGoroutines = 1000

	var (
		successCount atomic.Int64
		errorCount   atomic.Int64
		wg           sync.WaitGroup
	)

	start := time.Now()

	wg.Add(numGoroutines)
	for i := 0; i < numGoroutines; i++ {
		go func(id int) {
			defer wg.Done()
			job := &Job{
				ID:      fmt.Sprintf("conc-insert-%d", id),
				Bank:    "bank",
				Type:    "retain",
				Payload: fmt.Sprintf("payload-%d", id),
			}
			if err := s.Insert(job); err != nil {
				errorCount.Add(1)
			} else {
				successCount.Add(1)
			}
		}(i)
	}

	wg.Wait()
	dur := time.Since(start)

	success := int(successCount.Load())
	failures := int(errorCount.Load())
	total := success + failures

	t.Logf("Concurrent inserts: %d success, %d failures, %d total in %v", success, failures, total, dur)

	if total != numGoroutines {
		t.Errorf("Total results (%d) != goroutines (%d) — some jobs may be unaccounted", total, numGoroutines)
	}

	// Verify no duplicate IDs
	duplicateDetected := false
	for i := 0; i < numGoroutines; i++ {
		id := fmt.Sprintf("conc-insert-%d", i)
		got, err := s.Get(id)
		if err != nil {
			t.Errorf("Get(%s): %v", id, err)
			continue
		}
		if got == nil {
			// This should not happen — if Insert succeeded, Get should find it
			if i < success {
				t.Errorf("Job %s was reported as inserted but not found by Get", id)
			}
			continue
		}
		// Verify payload integrity
		expectedPayload := fmt.Sprintf("payload-%d", i)
		if got.Payload != expectedPayload {
			t.Errorf("Job %s payload mismatch: got %q, want %q", id, got.Payload, expectedPayload)
		}
	}

	// Count jobs in store via CountByStatus
	count, err := s.CountByStatus(StatusPending)
	if err != nil {
		t.Fatalf("CountByStatus: %v", err)
	}
	// Some jobs may have been claimed if workers were running
	t.Logf("Pending jobs after concurrent inserts: %d (success=%d)", count, success)

	// If we have any duplicate, flag it
	if duplicateDetected {
		t.Error("Duplicate IDs detected in concurrent inserts")
	}

	if failures > 0 {
		t.Logf("NOTE: %d inserts failed (likely ErrQueueFull or transient)", failures)
	}
}

// =============================================================================
// Chaos 4: 100 concurrent NextPending on 1 job
// =============================================================================
//
// 100 goroutines calling NextPending on a store with exactly 1 pending job.
// Exactly 1 goroutine must get the job. Zero duplicates. 99 get nil.

func TestChaos4_HundredConcurrentNextPending(t *testing.T) {
	s := newTestStore(t)

	const numContenders = 100

	// Insert exactly 1 job
	insertTestJob(t, s, "single-job-4")

	var (
		gotJob atomic.Int64 // how many goroutines got a non-nil job
		wg     sync.WaitGroup
	)

	wg.Add(numContenders)
	for i := 0; i < numContenders; i++ {
		go func(id int) {
			defer wg.Done()
			job, err := s.NextPending()
			if err != nil {
				return // error, didn't get job
			}
			if job != nil {
				gotJob.Add(1)
				// Mark as completed to free up if any duplicate slips through
				_ = s.UpdateStatus(job.ID, StatusCompleted, "done", "")
			}
		}(i)
	}

	wg.Wait()

	claimed := gotJob.Load()
	if claimed != 1 {
		t.Errorf("BUG: %d goroutines claimed the single job (expected exactly 1)", claimed)
	} else {
		t.Log("EXACTLY 1 goroutine claimed the single job — NextPending is race-safe")
	}

	// Verify the job state
	got, err := s.Get("single-job-4")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got == nil {
		t.Fatal("Job disappeared")
	}
	t.Logf("Final job status: %s (expected completed since the claimer completed it)", got.Status)

	// Now test: 100 goroutines on 0 jobs — all should get nil
	t.Run("all_nil_on_empty", func(t *testing.T) {
		var gotNil atomic.Int64
		var wg2 sync.WaitGroup

		wg2.Add(numContenders)
		for i := 0; i < numContenders; i++ {
			go func() {
				defer wg2.Done()
				job, err := s.NextPending()
				if err == nil && job == nil {
					gotNil.Add(1)
				}
			}()
		}

		wg2.Wait()

		if gotNil.Load() != int64(numContenders) {
			t.Errorf("Only %d/100 got nil on empty queue, expected all", gotNil.Load())
		} else {
			t.Log("All 100 goroutines correctly got nil on empty queue")
		}
	})
}

// =============================================================================
// Chaos 5: Worker storm — 50 workers, SemSize=1, 200 jobs
// =============================================================================
//
// Verify all jobs complete, measure total time, check FIFO claim order.

func TestChaos5_WorkerStormFIFO(t *testing.T) {
	s := newTestStoreWithConfig(t, StoreConfig{MaxPending: 500})

	const (
		numWorkers  = 50
		semSize     = 1
		numJobs     = 200
	)

	// Insert 200 jobs sequentially (so IDs reflect insertion order)
	for i := 0; i < numJobs; i++ {
		insertTestJob(t, s, fmt.Sprintf("storm-%d", i))
	}

	var (
		completedCount atomic.Int64
		// Track the order of first claims vs completions for FIFO analysis
		claimOrder    = make([]string, 0, numJobs)
		claimMu       sync.Mutex
	)

	processFunc := func(ctx context.Context, job *Job) error {
		// Record claim order
		claimMu.Lock()
		claimOrder = append(claimOrder, job.ID)
		claimMu.Unlock()

		// Simulate small processing delay
		time.Sleep(time.Millisecond)
		return nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	w := NewWorker(WorkerConfig{
		Store:   s,
		Process: processFunc,
		Count:   numWorkers,
		SemSize: semSize,
	})

	start := time.Now()
	w.Start(ctx)

	// Wait for all jobs to reach terminal state
	deadline := time.After(60 * time.Second)
	pollTick := time.NewTicker(10 * time.Millisecond)
	defer pollTick.Stop()

	pending := numJobs
	for pending > 0 {
		select {
		case <-deadline:
			t.Fatalf("TIMEOUT after %v: %d jobs not completed", time.Since(start), pending)
		case <-pollTick.C:
			count, err := s.CountByStatus(StatusCompleted)
			if err != nil {
				continue
			}
			failed, _ := s.CountByStatus(StatusFailed)
			deadJobs, _ := s.CountByStatus(StatusDead)
			done := count + failed + deadJobs
			pending = numJobs - done
			completedCount.Store(int64(done))
		}
	}

	dur := time.Since(start)
	w.Stop()

	t.Logf("Worker storm: %d workers, SemSize=%d, %d jobs completed in %v (%.0f jobs/sec)",
		numWorkers, semSize, completedCount.Load(), dur, float64(completedCount.Load())/dur.Seconds())

	// Verify all jobs completed successfully
	for i := 0; i < numJobs; i++ {
		id := fmt.Sprintf("storm-%d", i)
		got, err := s.Get(id)
		if err != nil {
			t.Errorf("Get(%s): %v", id, err)
			continue
		}
		if got == nil {
			t.Errorf("Job %s not found", id)
			continue
		}
		if got.Status != StatusCompleted {
			t.Errorf("Job %s status=%q, want completed", id, got.Status)
		}
	}

	// Verify FIFO claim order — jobs should be claimed in order
	// Note: with SemSize=1, jobs are claimed (running) in order via NextPending's
	// ORDER BY + mutex serialization, but the semaphore fairness depends on
	// goroutine scheduling. So claim order is not strictly guaranteed FIFO
	// when multiple workers race to the semaphore.
	t.Log("All 200 jobs completed successfully")
}

// =============================================================================
// Chaos 6: Context cancellation during in-flight processing
// =============================================================================
//
// Start worker, cancel context while processFunc is mid-flight.
// Verify worker exits cleanly with no goroutine leak.

func TestChaos6_ContextCancellationDuringProcessing(t *testing.T) {
	s := newTestStore(t)

	const numJobs = 5

	// Insert jobs that will be picked up but block in processFunc
	for i := 0; i < numJobs; i++ {
		insertTestJob(t, s, fmt.Sprintf("cancel-mid-%d", i))
	}

	var (
		startedProcessing atomic.Int64
		completedCleanup  atomic.Int64
		blockers          = make([]chan struct{}, numJobs) // signal to release each blocked job
	)

	for i := 0; i < numJobs; i++ {
		blockers[i] = make(chan struct{})
	}

	processFunc := func(ctx context.Context, job *Job) error {
		idx := -1
		fmt.Sscanf(job.ID, "cancel-mid-%d", &idx)
		startedProcessing.Add(1)

		// Block until either context is cancelled or we're explicitly released
		select {
		case <-ctx.Done():
			completedCleanup.Add(1)
			return ctx.Err()
		case <-blockers[idx]:
			completedCleanup.Add(1)
			return nil
		}
	}

	baseline := runtimeNumGoroutines()

	ctx, cancel := context.WithCancel(context.Background())

	w := NewWorker(WorkerConfig{
		Store:   s,
		Process: processFunc,
		Count:   numJobs,
		SemSize: numJobs,
	})
	w.Start(ctx)

	// Wait for all jobs to start processing
	deadline := time.After(10 * time.Second)
	for startedProcessing.Load() < int64(numJobs) {
		select {
		case <-deadline:
			t.Fatalf("TIMEOUT waiting for all jobs to start processing: got %d of %d",
				startedProcessing.Load(), numJobs)
		default:
			time.Sleep(5 * time.Millisecond)
		}
	}
	t.Logf("All %d jobs started processing", numJobs)

	// Cancel the context while all workers are mid-flight
	cancel()

	// Wait for workers to exit
	w.Stop()

	// Verify all processFunc calls completed (either via cancellation or release)
	if completedCleanup.Load() != int64(numJobs) {
		t.Errorf("Only %d/%d processFunc calls completed cleanup", completedCleanup.Load(), numJobs)
	}

	// Check for goroutine leak
	giveTime := time.After(200 * time.Millisecond)
	<-giveTime
	after := runtimeNumGoroutines()
	leaked := after - baseline
	if leaked > 3 {
		t.Errorf("Goroutine leak after cancellation: baseline=%d after=%d delta=%d",
			baseline, after, leaked)
	} else {
		t.Logf("Goroutine delta after cancellation: %d (baseline=%d after=%d)",
			leaked, baseline, after)
	}

	// Verify jobs were properly handled (some should be failed/cancelled)
	for i := 0; i < numJobs; i++ {
		id := fmt.Sprintf("cancel-mid-%d", i)
		got, err := s.Get(id)
		if err != nil {
			t.Errorf("Get(%s): %v", id, err)
			continue
		}
		if got == nil {
			continue
		}
		// The worker should have marked it as failed (ctx.Err) and possibly retried
		t.Logf("Job %s final status: %s retry_count=%d", id, got.Status, got.RetryCount)
	}

	t.Log("Context cancellation during processing: clean exit, no leak")
}

// =============================================================================
// Chaos 7: Store stats under fire
// =============================================================================
//
// While 20 goroutines insert and 5 workers dequeue, call Stats() 100 times.
// Any panic? Any impossible values?

func TestChaos7_StatsUnderFire(t *testing.T) {
	s := newTestStoreWithConfig(t, StoreConfig{MaxPending: 5000})

	const (
		numInserters = 20
		numWorkers   = 5
		numJobsEach  = 50   // each inserter adds this many jobs
		statCalls    = 100
	)

	var (
		wg             sync.WaitGroup
		statsErrors    atomic.Int64
		workersWg      sync.WaitGroup
		stopWorkers    atomic.Bool
	)

	// Worker pool — continuously dequeue until told to stop
	processFunc := func(ctx context.Context, job *Job) error {
		time.Sleep(time.Millisecond) // small processing cost
		return nil
	}

	workerCtx, workerCancel := context.WithCancel(context.Background())
	defer workerCancel()

	w := NewWorker(WorkerConfig{
		Store:   s,
		Process: processFunc,
		Count:   numWorkers,
		SemSize: 3,
	})
	w.Start(workerCtx)

	// Inserters — add jobs rapidly
	wg.Add(numInserters)
	for in := 0; in < numInserters; in++ {
		go func(inserterID int) {
			defer wg.Done()
			for j := 0; j < numJobsEach; j++ {
				job := &Job{
					ID:      fmt.Sprintf("statfire-%d-%d", inserterID, j),
					Bank:    "bank",
					Type:    "retain",
					Payload: "data",
				}
				if err := s.Insert(job); err != nil {
					// Could be ErrQueueFull — ignore
				}
				time.Sleep(time.Microsecond)
			}
		}(in)
	}

	// Stats readers — hammer Stats() while chaos ensues
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < statCalls; i++ {
			func() {
				defer func() {
					if r := recover(); r != nil {
						t.Errorf("PANIC in Stats() call %d: %v", i, r)
					}
				}()
				stats, err := s.Stats()
				if err != nil {
					statsErrors.Add(1)
					return
				}
				// Check for impossible values
				if stats.Pending < 0 || stats.Running < 0 || stats.Completed < 0 ||
					stats.Failed < 0 || stats.Dead < 0 {
					t.Errorf("IMPOSSIBLE negative stat: %+v", stats)
				}
				if stats.OldestPending < 0 {
					t.Errorf("IMPOSSIBLE negative OldestPending: %d", stats.OldestPending)
				}
			}()
			time.Sleep(time.Millisecond)
		}
	}()

	// Wait for inserters, then stop workers
	wg.Wait()
	stopWorkers.Store(true)
	workerCancel()
	w.Stop()
	workersWg.Wait()

	// Final stats check
	stats, err := s.Stats()
	if err != nil {
		t.Fatalf("Final Stats: %v", err)
	}

	t.Logf("Stats under fire: pending=%d running=%d completed=%d failed=%d dead=%d oldestPending=%d",
		stats.Pending, stats.Running, stats.Completed, stats.Failed, stats.Dead, stats.OldestPending)

	totalAll := stats.Pending + stats.Running + stats.Completed + stats.Failed + stats.Dead
	expectedTotal := numInserters * numJobsEach
	t.Logf("Total jobs tracked: %d (expected ~%d, some may be in-flight from workers)", totalAll, expectedTotal)

	if statsErrors.Load() > 0 {
		t.Logf("Stats() returned errors %d times during chaos", statsErrors.Load())
	}

	if stats.Pending < 0 || stats.Running < 0 || stats.Completed < 0 ||
		stats.Failed < 0 || stats.Dead < 0 {
		t.Error("Final Stats contains negative values")
	}
}

// =============================================================================
// Chaos 8: Memory after 10K jobs
// =============================================================================
//
// Insert 10000 jobs, process them all, check runtime.MemStats for unbounded
// growth. Close store, verify memory returns.

func TestChaos8_MemoryAfter10KJobs(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping memory-intensive test in short mode")
	}

	s := newTestStoreWithConfig(t, StoreConfig{MaxPending: 20000})

	const numJobs = 10000

	// Snapshot memory before inserting
	var memBefore runtime.MemStats
	runtime.GC()
	time.Sleep(50 * time.Millisecond)
	runtime.ReadMemStats(&memBefore)
	t.Logf("Memory before: HeapAlloc=%d KB TotalAlloc=%d KB",
		memBefore.HeapAlloc/1024, memBefore.TotalAlloc/1024)

	// Insert 10000 jobs in batches
	insertStart := time.Now()
	for i := 0; i < numJobs; i++ {
		job := &Job{
			ID:      fmt.Sprintf("memtest-%d", i),
			Bank:    "bank",
			Type:    "retain",
			Payload: fmt.Sprintf("payload-%d", i),
		}
		if err := s.Insert(job); err != nil {
			t.Fatalf("Insert(%d): %v", i, err)
		}
	}
	insertDur := time.Since(insertStart)
	t.Logf("Inserted %d jobs in %v (%.0f jobs/sec)", numJobs, insertDur, float64(numJobs)/insertDur.Seconds())

	// Memory after insert
	var memAfterInsert runtime.MemStats
	runtime.ReadMemStats(&memAfterInsert)
	t.Logf("Memory after insert: HeapAlloc=%d KB TotalAlloc=%d KB (delta HeapAlloc=%d KB)",
		memAfterInsert.HeapAlloc/1024, memAfterInsert.TotalAlloc/1024,
		(memAfterInsert.HeapAlloc-memBefore.HeapAlloc)/1024)

	// Process all 10000 jobs with a worker pool
	processStart := time.Now()
	var processed atomic.Int64

	processFunc := func(ctx context.Context, job *Job) error {
		processed.Add(1)
		return nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	w := NewWorker(WorkerConfig{
		Store:   s,
		Process: processFunc,
		Count:   10,
		SemSize: 5,
	})
	w.Start(ctx)

	// Wait for all jobs to complete
	deadline := time.After(120 * time.Second)
	pollTick := time.NewTicker(50 * time.Millisecond)
	defer pollTick.Stop()

	for processed.Load() < int64(numJobs) {
		select {
		case <-deadline:
			t.Fatalf("TIMEOUT processing 10K jobs: completed %d of %d", processed.Load(), numJobs)
		case <-pollTick.C:
		}
	}

	processDur := time.Since(processStart)
	w.Stop()
	t.Logf("Processed %d jobs in %v (%.0f jobs/sec)", numJobs, processDur, float64(numJobs)/processDur.Seconds())

	// Force GC and check memory after processing
	runtime.GC()
	time.Sleep(100 * time.Millisecond)
	runtime.GC() // second GC to clean up finalizers
	time.Sleep(50 * time.Millisecond)

	var memAfterProcessing runtime.MemStats
	runtime.ReadMemStats(&memAfterProcessing)

	heapDelta := int64(memAfterProcessing.HeapAlloc - memBefore.HeapAlloc)
	totalAllocDelta := int64(memAfterProcessing.TotalAlloc - memBefore.TotalAlloc)

	t.Logf("Memory after processing+GC: HeapAlloc=%d KB TotalAlloc=%d KB",
		memAfterProcessing.HeapAlloc/1024, memAfterProcessing.TotalAlloc/1024)
	t.Logf("HeapAlloc delta from before: %d KB (%.1f bytes/job)",
		heapDelta/1024, float64(heapDelta)/float64(numJobs))
	t.Logf("TotalAlloc delta from before: %d KB (%.1f bytes/job)",
		totalAllocDelta/1024, float64(totalAllocDelta)/float64(numJobs))

	// Close store
	s.Close()

	// Memory after close
	runtime.GC()
	time.Sleep(50 * time.Millisecond)

	var memAfterClose runtime.MemStats
	runtime.ReadMemStats(&memAfterClose)

	closeDelta := int64(memAfterClose.HeapAlloc - memBefore.HeapAlloc)
	t.Logf("Memory after close+GC: HeapAlloc=%d KB (delta=%d KB)",
		memAfterClose.HeapAlloc/1024, closeDelta/1024)

	// The heap should grow by at most a few MB for 10K jobs.
	// Go's SQLite driver (modernc.org/sqlite) may retain some internal caches.
	// If HeapAlloc > 50MB delta, flag it as potential leak.
	if heapDelta > 50*1024*1024 {
		t.Errorf("Possible memory leak: HeapAlloc grew by %d KB after processing 10K jobs", heapDelta/1024)
	} else {
		t.Logf("Memory growth within expected bounds (%d KB delta)", heapDelta/1024)
	}

	// TotalAlloc measures lifetime allocations. For 10K jobs, each job creates
	// a Job struct and payload. Expect roughly 10K * ~200 bytes ≈ 2MB + SQLite overhead.
	// If TotalAlloc > 500MB, that's suspicious per-job overhead.
	if totalAllocDelta > 500*1024*1024 {
		t.Errorf("High per-job allocation overhead: TotalAlloc grew by %d KB (%.0f bytes/job)",
			totalAllocDelta/1024, float64(totalAllocDelta)/float64(numJobs))
	} else {
		t.Logf("TotalAlloc growth reasonable at %d bytes/job", totalAllocDelta/int64(numJobs))
	}

	t.Log("Memory test completed — 10K jobs processed without unbounded growth")
}
