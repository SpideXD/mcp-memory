package queue

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Test Infrastructure
// ---------------------------------------------------------------------------

func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := NewStore(StoreConfig{DBPath: ":memory:"})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func newTestStoreWithConfig(t *testing.T, cfg StoreConfig) *Store {
	t.Helper()
	cfg.DBPath = ":memory:"
	s, err := NewStore(cfg)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func insertTestJob(t *testing.T, s *Store, id string) *Job {
	t.Helper()
	job := &Job{
		ID:      id,
		Bank:    "test-bank",
		Type:    "retain",
		Payload: fmt.Sprintf("payload-%s", id),
	}
	if err := s.Insert(job); err != nil {
		t.Fatalf("Insert(%s): %v", id, err)
	}
	return job
}

func insertTestJobWithDelay(t *testing.T, s *Store, id string, delay time.Duration) *Job {
	t.Helper()
	time.Sleep(delay)
	return insertTestJob(t, s, id)
}

// rawInsertJob inserts a job directly via SQL, bypassing Insert() validation.
// Useful for setting up specific states for recovery tests.
func rawInsertJob(t *testing.T, s *Store, id string, status Status, retryCount, maxRetries int) {
	t.Helper()
	now := time.Now().Unix()
	_, err := s.db.Exec(
		`INSERT INTO jobs (id, bank, type, payload, status, retry_count, max_retries, result, error, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, "test-bank", "retain", "payload-"+id, string(status),
		retryCount, maxRetries, "", "", now, now,
	)
	if err != nil {
		t.Fatalf("rawInsertJob(%s): %v", id, err)
	}
}

// ---------------------------------------------------------------------------
// State Machine Tests
// ---------------------------------------------------------------------------

func TestStore_InsertAndGet(t *testing.T) {
	s := newTestStore(t)

	job := &Job{
		ID:      "job-1",
		Bank:    "my-bank",
		Type:    "retain",
		Payload: "hello world",
	}
	if err := s.Insert(job); err != nil {
		t.Fatalf("Insert: %v", err)
	}

	got, err := s.Get("job-1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got == nil {
		t.Fatal("Get returned nil")
	}
	if got.ID != "job-1" {
		t.Errorf("ID = %q, want %q", got.ID, "job-1")
	}
	if got.Bank != "my-bank" {
		t.Errorf("Bank = %q, want %q", got.Bank, "my-bank")
	}
	if got.Type != "retain" {
		t.Errorf("Type = %q, want %q", got.Type, "retain")
	}
	if got.Payload != "hello world" {
		t.Errorf("Payload = %q, want %q", got.Payload, "hello world")
	}
	if got.Status != StatusPending {
		t.Errorf("Status = %q, want %q", got.Status, StatusPending)
	}
	if got.RetryCount != 0 {
		t.Errorf("RetryCount = %d, want 0", got.RetryCount)
	}
	if got.MaxRetries != DefaultMaxRetries {
		t.Errorf("MaxRetries = %d, want %d", got.MaxRetries, DefaultMaxRetries)
	}
	if got.CreatedAt == 0 {
		t.Error("CreatedAt should be non-zero")
	}
	if got.UpdatedAt == 0 {
		t.Error("UpdatedAt should be non-zero")
	}
}

func TestStore_NextPending_ReturnsOldestFirst(t *testing.T) {
	s := newTestStore(t)

	// Insert 3 jobs with deliberate delays to ensure distinct created_at
	insertTestJob(t, s, "job-1")
	time.Sleep(1100 * time.Millisecond) // ensure distinct unix timestamps
	insertTestJob(t, s, "job-2")
	time.Sleep(1100 * time.Millisecond)
	insertTestJob(t, s, "job-3")

	// Dequeue should be FIFO
	for i, wantID := range []string{"job-1", "job-2", "job-3"} {
		job, err := s.NextPending()
		if err != nil {
			t.Fatalf("NextPending #%d: %v", i, err)
		}
		if job == nil {
			t.Fatalf("NextPending #%d: got nil", i)
		}
		if job.ID != wantID {
			t.Errorf("NextPending #%d: got ID=%q, want %q", i, job.ID, wantID)
		}
		if job.Status != StatusRunning {
			t.Errorf("NextPending #%d: status = %q, want %q", i, job.Status, StatusRunning)
		}
	}
}

func TestStore_NextPending_NoJobs(t *testing.T) {
	s := newTestStore(t)

	job, err := s.NextPending()
	if err != nil {
		t.Fatalf("NextPending: %v", err)
	}
	if job != nil {
		t.Errorf("expected nil job, got %+v", job)
	}
}

func TestStore_NextPending_OnlyRunningJobs(t *testing.T) {
	s := newTestStore(t)

	// Insert and claim a job
	insertTestJob(t, s, "job-1")
	job, err := s.NextPending()
	if err != nil {
		t.Fatalf("NextPending: %v", err)
	}
	if job == nil {
		t.Fatal("expected job, got nil")
	}
	if job.Status != StatusRunning {
		t.Fatalf("status = %q, want running", job.Status)
	}

	// Next call should return nil (only running jobs left)
	job2, err := s.NextPending()
	if err != nil {
		t.Fatalf("NextPending: %v", err)
	}
	if job2 != nil {
		t.Errorf("expected nil, got %+v", job2)
	}
}

func TestStore_UpdateStatus_PendingToRunningToCompleted(t *testing.T) {
	s := newTestStore(t)

	insertTestJob(t, s, "job-1")

	// Claim
	job, err := s.NextPending()
	if err != nil {
		t.Fatalf("NextPending: %v", err)
	}
	if job.Status != StatusRunning {
		t.Fatalf("after claim: status = %q, want running", job.Status)
	}

	// Complete
	if err := s.UpdateStatus(job.ID, StatusCompleted, "result-data", ""); err != nil {
		t.Fatalf("UpdateStatus(completed): %v", err)
	}

	got, err := s.Get(job.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != StatusCompleted {
		t.Errorf("status = %q, want completed", got.Status)
	}
	if got.Result != "result-data" {
		t.Errorf("result = %q, want %q", got.Result, "result-data")
	}
}

func TestStore_UpdateStatus_PendingToRunningToFailedToRetry(t *testing.T) {
	s := newTestStore(t)

	job := &Job{
		ID:         "retry-job",
		Bank:       "bank",
		Type:       "retain",
		Payload:    "data",
		MaxRetries: 3,
	}
	if err := s.Insert(job); err != nil {
		t.Fatalf("Insert: %v", err)
	}

	// Claim
	claimed, err := s.NextPending()
	if err != nil {
		t.Fatalf("NextPending: %v", err)
	}

	// Fail
	if err := s.UpdateStatus(claimed.ID, StatusFailed, "", "process error"); err != nil {
		t.Fatalf("UpdateStatus(failed): %v", err)
	}

	// Verify retry_count incremented
	got, err := s.Get(claimed.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != StatusFailed {
		t.Errorf("status = %q, want failed", got.Status)
	}
	if got.RetryCount != 1 {
		t.Errorf("retry_count = %d, want 1", got.RetryCount)
	}
	if got.Error != "process error" {
		t.Errorf("error = %q, want %q", got.Error, "process error")
	}

	// CanRetry should be true (1 < 3)
	if !got.CanRetry() {
		t.Error("CanRetry should be true (1 < 3)")
	}

	// Simulate worker retry: set back to pending
	if err := s.UpdateStatus(claimed.ID, StatusPending, "", ""); err != nil {
		t.Fatalf("UpdateStatus(pending retry): %v", err)
	}

	got2, err := s.Get(claimed.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got2.Status != StatusPending {
		t.Errorf("after retry: status = %q, want pending", got2.Status)
	}

	// Claim again
	retried, err := s.NextPending()
	if err != nil {
		t.Fatalf("NextPending: %v", err)
	}
	if retried == nil {
		t.Fatal("expected retried job, got nil")
	}
	if retried.ID != "retry-job" {
		t.Errorf("retried job ID = %q, want %q", retried.ID, "retry-job")
	}
}

func TestStore_UpdateStatus_PendingToRunningToFailedToDead(t *testing.T) {
	s := newTestStore(t)

	job := &Job{
		ID:         "dead-job",
		Bank:       "bank",
		Type:       "retain",
		Payload:    "data",
		MaxRetries: 1,
	}
	if err := s.Insert(job); err != nil {
		t.Fatalf("Insert: %v", err)
	}

	// Claim
	claimed, err := s.NextPending()
	if err != nil {
		t.Fatalf("NextPending: %v", err)
	}

	// Fail — retry_count becomes 1
	if err := s.UpdateStatus(claimed.ID, StatusFailed, "", "fatal error"); err != nil {
		t.Fatalf("UpdateStatus(failed): %v", err)
	}

	got, err := s.Get(claimed.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.RetryCount != 1 {
		t.Errorf("retry_count = %d, want 1", got.RetryCount)
	}

	// CanRetry should be false (1 >= 1)
	if got.CanRetry() {
		t.Error("CanRetry should be false (1 >= 1)")
	}

	// Mark dead
	if err := s.UpdateStatus(claimed.ID, StatusDead, "", "fatal error"); err != nil {
		t.Fatalf("UpdateStatus(dead): %v", err)
	}

	got2, err := s.Get(claimed.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got2.Status != StatusDead {
		t.Errorf("status = %q, want dead", got2.Status)
	}
}

func TestStore_GetJob_NotFound(t *testing.T) {
	s := newTestStore(t)

	got, err := s.Get("nonexistent")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got != nil {
		t.Errorf("expected nil, got %+v", got)
	}
}

// ---------------------------------------------------------------------------
// Recovery Tests
// ---------------------------------------------------------------------------

func TestStore_Recover_RunningToPending(t *testing.T) {
	s := newTestStore(t)

	// Insert a job directly with "running" status (simulates crashed worker)
	rawInsertJob(t, s, "orphan-1", StatusRunning, 0, 3)

	// Recover
	n, err := s.Recover()
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if n != 1 {
		t.Errorf("recovered %d jobs, want 1", n)
	}

	got, err := s.Get("orphan-1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != StatusPending {
		t.Errorf("status = %q, want pending", got.Status)
	}
}

func TestStore_Recover_FailedWithRetriesToPending(t *testing.T) {
	s := newTestStore(t)

	// Failed job with retry_count=1, max_retries=3 → should become pending
	rawInsertJob(t, s, "retry-fail", StatusFailed, 1, 3)

	n, err := s.Recover()
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if n != 1 {
		t.Errorf("recovered %d jobs, want 1", n)
	}

	got, err := s.Get("retry-fail")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != StatusPending {
		t.Errorf("status = %q, want pending", got.Status)
	}
}

func TestStore_Recover_FailedExhaustedToDead(t *testing.T) {
	s := newTestStore(t)

	// Failed job with retry_count=3, max_retries=3 → should become dead
	rawInsertJob(t, s, "exhausted", StatusFailed, 3, 3)

	n, err := s.Recover()
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if n != 1 {
		t.Errorf("recovered %d jobs, want 1", n)
	}

	got, err := s.Get("exhausted")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != StatusDead {
		t.Errorf("status = %q, want dead", got.Status)
	}
}

// ---------------------------------------------------------------------------
// Concurrency Tests
// ---------------------------------------------------------------------------

func TestStore_NextPending_ConcurrentNoDuplicates(t *testing.T) {
	s := newTestStore(t)

	// Insert exactly 1 pending job
	insertTestJob(t, s, "sole-job")

	const numGoroutines = 10
	var wg sync.WaitGroup
	results := make([]*Job, numGoroutines)
	errs := make([]error, numGoroutines)

	wg.Add(numGoroutines)
	for i := 0; i < numGoroutines; i++ {
		go func(idx int) {
			defer wg.Done()
			results[idx], errs[idx] = s.NextPending()
		}(i)
	}
	wg.Wait()

	// Exactly one goroutine should have gotten the job
	claimed := 0
	for i := 0; i < numGoroutines; i++ {
		if errs[i] != nil {
			t.Errorf("goroutine %d: error: %v", i, errs[i])
		}
		if results[i] != nil {
			claimed++
			if results[i].ID != "sole-job" {
				t.Errorf("goroutine %d: got ID=%q, want %q", i, results[i].ID, "sole-job")
			}
		}
	}
	if claimed != 1 {
		t.Errorf("claimed = %d, want 1 (no duplicates allowed)", claimed)
	}
}

func TestStore_ConcurrentInsertAndNextPending(t *testing.T) {
	s := newTestStore(t)

	const numJobs = 50
	var wg sync.WaitGroup
	var insertCount atomic.Int64
	var claimCount atomic.Int64

	// Inserter goroutine
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < numJobs; i++ {
			job := &Job{
				ID:      fmt.Sprintf("conc-job-%d", i),
				Bank:    "bank",
				Type:    "retain",
				Payload: "data",
			}
			if err := s.Insert(job); err != nil && !errors.Is(err, ErrQueueFull) {
				t.Errorf("Insert(%d): %v", i, err)
			} else if err == nil {
				insertCount.Add(1)
			}
		}
	}()

	// Claimer goroutines
	const numClaimers = 5
	wg.Add(numClaimers)
	for c := 0; c < numClaimers; c++ {
		go func() {
			defer wg.Done()
			for {
				job, err := s.NextPending()
				if err != nil {
					t.Errorf("NextPending: %v", err)
					return
				}
				if job != nil {
					claimCount.Add(1)
				}
				// Check if we've claimed enough (give inserter time)
				if claimCount.Load() >= int64(numJobs) {
					return
				}
				time.Sleep(10 * time.Millisecond)
			}
		}()
	}

	wg.Wait()

	// All inserted jobs should eventually be claimed
	total := insertCount.Load()
	if total != numJobs {
		t.Errorf("inserted %d, want %d", total, numJobs)
	}
	// claimed count should not exceed inserted (no phantom claims)
	if claimCount.Load() > total {
		t.Errorf("claimed %d > inserted %d", claimCount.Load(), total)
	}
}

func TestStore_ConcurrentUpdateStatus(t *testing.T) {
	s := newTestStore(t)

	// Insert 10 jobs and claim them all
	const numJobs = 10
	ids := make([]string, numJobs)
	for i := 0; i < numJobs; i++ {
		id := fmt.Sprintf("upd-job-%d", i)
		ids[i] = id
		insertTestJob(t, s, id)
	}

	claimed := make([]string, 0, numJobs)
	for i := 0; i < numJobs; i++ {
		job, err := s.NextPending()
		if err != nil {
			t.Fatalf("NextPending: %v", err)
		}
		if job == nil {
			t.Fatalf("expected job #%d, got nil", i)
		}
		claimed = append(claimed, job.ID)
	}

	// Concurrently complete all jobs
	var wg sync.WaitGroup
	wg.Add(numJobs)
	for _, id := range claimed {
		go func(jobID string) {
			defer wg.Done()
			if err := s.UpdateStatus(jobID, StatusCompleted, "done", ""); err != nil {
				t.Errorf("UpdateStatus(%s): %v", jobID, err)
			}
		}(id)
	}
	wg.Wait()

	// Verify all completed
	for _, id := range claimed {
		got, err := s.Get(id)
		if err != nil {
			t.Fatalf("Get(%s): %v", id, err)
		}
		if got.Status != StatusCompleted {
			t.Errorf("job %s: status = %q, want completed", id, got.Status)
		}
	}
}

// ---------------------------------------------------------------------------
// Cleanup Tests (TTL)
// ---------------------------------------------------------------------------

func TestStore_Cleanup_DeletesOldCompleted(t *testing.T) {
	jobTTL := 1 * time.Second
	s := newTestStoreWithConfig(t, StoreConfig{JobTTL: jobTTL})

	// Insert and complete a job
	insertTestJob(t, s, "old-completed")
	claimed, err := s.NextPending()
	if err != nil {
		t.Fatalf("NextPending: %v", err)
	}
	if err := s.UpdateStatus(claimed.ID, StatusCompleted, "done", ""); err != nil {
		t.Fatalf("UpdateStatus: %v", err)
	}

	// Backdate the updated_at to be older than TTL
	oldTime := time.Now().Add(-2 * jobTTL).Unix()
	_, err = s.db.Exec("UPDATE jobs SET updated_at = ? WHERE id = ?", oldTime, "old-completed")
	if err != nil {
		t.Fatalf("backdate: %v", err)
	}

	// Run cleanup
	s.cleanupOnce()

	// Job should be deleted
	got, err := s.Get("old-completed")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got != nil {
		t.Error("old completed job should have been deleted by TTL cleanup")
	}
}

func TestStore_Cleanup_KeepsRecentCompleted(t *testing.T) {
	s := newTestStoreWithConfig(t, StoreConfig{JobTTL: 1 * time.Hour})

	// Insert and complete a job (just now, well within TTL)
	insertTestJob(t, s, "recent-completed")
	claimed, err := s.NextPending()
	if err != nil {
		t.Fatalf("NextPending: %v", err)
	}
	if err := s.UpdateStatus(claimed.ID, StatusCompleted, "done", ""); err != nil {
		t.Fatalf("UpdateStatus: %v", err)
	}

	// Run cleanup
	s.cleanupOnce()

	// Job should still exist
	got, err := s.Get("recent-completed")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got == nil {
		t.Error("recent completed job should NOT have been deleted")
	}
	if got != nil && got.Status != StatusCompleted {
		t.Errorf("status = %q, want completed", got.Status)
	}
}

func TestStore_Cleanup_DeletesOldDead(t *testing.T) {
	jobTTL := 1 * time.Second
	s := newTestStoreWithConfig(t, StoreConfig{JobTTL: jobTTL})

	// Insert a dead job directly
	rawInsertJob(t, s, "old-dead", StatusDead, 3, 3)

	// Backdate the updated_at to be older than TTL
	oldTime := time.Now().Add(-2 * jobTTL).Unix()
	_, err := s.db.Exec("UPDATE jobs SET updated_at = ? WHERE id = ?", oldTime, "old-dead")
	if err != nil {
		t.Fatalf("backdate: %v", err)
	}

	// Run cleanup
	s.cleanupOnce()

	// Job should be deleted
	got, err := s.Get("old-dead")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got != nil {
		t.Error("old dead job should have been deleted by TTL cleanup")
	}
}

// ---------------------------------------------------------------------------
// Stats Tests
// ---------------------------------------------------------------------------

func TestStore_Stats_Accurate(t *testing.T) {
	s := newTestStore(t)

	// Insert 3 pending jobs
	for i := 0; i < 3; i++ {
		insertTestJob(t, s, fmt.Sprintf("pending-%d", i))
	}

	// Claim one and complete it
	job1, _ := s.NextPending()
	s.UpdateStatus(job1.ID, StatusCompleted, "done", "")

	// Claim one and fail it
	job2, _ := s.NextPending()
	s.UpdateStatus(job2.ID, StatusFailed, "", "err")

	// One should still be pending
	stats, err := s.Stats()
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if stats.Pending != 1 {
		t.Errorf("Pending = %d, want 1", stats.Pending)
	}
	if stats.Running != 0 {
		t.Errorf("Running = %d, want 0", stats.Running)
	}
	if stats.Completed != 1 {
		t.Errorf("Completed = %d, want 1", stats.Completed)
	}
	if stats.Failed != 1 {
		t.Errorf("Failed = %d, want 1", stats.Failed)
	}
	if stats.Dead != 0 {
		t.Errorf("Dead = %d, want 0", stats.Dead)
	}
}

func TestStore_Stats_OldestPendingAge(t *testing.T) {
	s := newTestStore(t)

	// Insert first job
	insertTestJob(t, s, "old-pending")
	time.Sleep(1100 * time.Millisecond) // ensure distinct timestamp

	// Insert second job
	insertTestJob(t, s, "new-pending")

	stats, err := s.Stats()
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}

	if stats.OldestPending == 0 {
		t.Fatal("OldestPending should be non-zero")
	}

	// The oldest pending should be from the first job
	now := time.Now().Unix()
	age := now - stats.OldestPending
	if age < 0 || age > 5 {
		t.Errorf("OldestPending age = %d seconds, want ~1 second", age)
	}
}

// ---------------------------------------------------------------------------
// Backpressure Tests
// ---------------------------------------------------------------------------

func TestStore_Insert_QueueFull(t *testing.T) {
	s := newTestStoreWithConfig(t, StoreConfig{MaxPending: 2})

	// Insert 2 jobs — should succeed
	for i := 0; i < 2; i++ {
		job := &Job{
			ID:      fmt.Sprintf("full-%d", i),
			Bank:    "bank",
			Type:    "retain",
			Payload: "data",
		}
		if err := s.Insert(job); err != nil {
			t.Fatalf("Insert(%d): %v", i, err)
		}
	}

	// Third insert should fail with ErrQueueFull
	job := &Job{
		ID:      "full-overflow",
		Bank:    "bank",
		Type:    "retain",
		Payload: "data",
	}
	err := s.Insert(job)
	if !errors.Is(err, ErrQueueFull) {
		t.Errorf("Insert: got %v, want ErrQueueFull", err)
	}
}

// ---------------------------------------------------------------------------
// Worker Pool Tests
// ---------------------------------------------------------------------------

func TestWorkerPool_ProcessesJob(t *testing.T) {
	s := newTestStore(t)
	insertTestJob(t, s, "process-me")

	var processed atomic.Bool
	processFunc := func(ctx context.Context, job *Job) error {
		if job.ID == "process-me" {
			processed.Store(true)
		}
		return nil
	}

	w := NewWorker(WorkerConfig{
		Store:   s,
		Process: processFunc,
		Count:   1,
		SemSize: 1,
	})
	w.Start(context.Background())
	defer w.Stop()

	// Wait for processing
	deadline := time.After(5 * time.Second)
	for !processed.Load() {
		select {
		case <-deadline:
			t.Fatal("timed out waiting for job to be processed")
		default:
			time.Sleep(50 * time.Millisecond)
		}
	}

	// Verify job completed
	got, err := s.Get("process-me")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != StatusCompleted {
		t.Errorf("status = %q, want completed", got.Status)
	}
}

func TestWorkerPool_RetryOnFailure(t *testing.T) {
	s := newTestStore(t)

	job := &Job{
		ID:         "retry-worker",
		Bank:       "bank",
		Type:       "retain",
		Payload:    "data",
		MaxRetries: 3,
	}
	if err := s.Insert(job); err != nil {
		t.Fatalf("Insert: %v", err)
	}

	var attempts atomic.Int32
	processFunc := func(ctx context.Context, job *Job) error {
		n := attempts.Add(1)
		if n == 1 {
			return fmt.Errorf("first attempt fails")
		}
		return nil // second attempt succeeds
	}

	w := NewWorker(WorkerConfig{
		Store:   s,
		Process: processFunc,
		Count:   1,
		SemSize: 1,
	})
	w.Start(context.Background())
	defer w.Stop()

	// Wait for the job to be completed (after retry)
	deadline := time.After(10 * time.Second)
	for {
		got, err := s.Get("retry-worker")
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if got != nil && got.Status == StatusCompleted {
			break
		}
		select {
		case <-deadline:
			got, _ := s.Get("retry-worker")
			t.Fatalf("timed out: job status = %q, attempts = %d", got.Status, attempts.Load())
		default:
			time.Sleep(100 * time.Millisecond)
		}
	}

	if attempts.Load() < 2 {
		t.Errorf("expected at least 2 attempts, got %d", attempts.Load())
	}
}

func TestWorkerPool_DeadAfterMaxRetries(t *testing.T) {
	s := newTestStore(t)

	job := &Job{
		ID:         "always-fail",
		Bank:       "bank",
		Type:       "retain",
		Payload:    "data",
		MaxRetries: 2,
	}
	if err := s.Insert(job); err != nil {
		t.Fatalf("Insert: %v", err)
	}

	processFunc := func(ctx context.Context, job *Job) error {
		return fmt.Errorf("permanent failure")
	}

	w := NewWorker(WorkerConfig{
		Store:   s,
		Process: processFunc,
		Count:   1,
		SemSize: 1,
	})
	w.Start(context.Background())
	defer w.Stop()

	// Wait for the job to become dead
	deadline := time.After(15 * time.Second)
	for {
		got, err := s.Get("always-fail")
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if got != nil && got.Status == StatusDead {
			break
		}
		select {
		case <-deadline:
			got, _ := s.Get("always-fail")
			if got != nil {
				t.Fatalf("timed out: job status = %q, retry_count = %d", got.Status, got.RetryCount)
			}
			t.Fatal("timed out: job is nil")
		default:
			time.Sleep(100 * time.Millisecond)
		}
	}

	got, _ := s.Get("always-fail")
	if got.RetryCount != 2 {
		t.Errorf("retry_count = %d, want 2", got.RetryCount)
	}
}

func TestWorkerPool_StopDrainsWorkers(t *testing.T) {
	s := newTestStore(t)

	// Insert a job so workers have work
	insertTestJob(t, s, "drain-job")

	startGoroutines := runtimeNumGoroutines()

	processFunc := func(ctx context.Context, job *Job) error {
		time.Sleep(100 * time.Millisecond) // simulate work
		return nil
	}

	w := NewWorker(WorkerConfig{
		Store:   s,
		Process: processFunc,
		Count:   4,
		SemSize: 2,
	})
	w.Start(context.Background())

	// Give workers time to start
	time.Sleep(200 * time.Millisecond)

	w.Stop()

	// Give goroutines time to fully exit
	time.Sleep(200 * time.Millisecond)

	endGoroutines := runtimeNumGoroutines()
	// Allow some slack for test framework goroutines
	leaked := endGoroutines - startGoroutines
	if leaked > 2 {
		t.Errorf("goroutine leak: started with %d, ended with %d (leaked %d)", startGoroutines, endGoroutines, leaked)
	}
}

func TestWorkerPool_SemaphoreLimitsConcurrency(t *testing.T) {
	s := newTestStore(t)

	// Insert 3 jobs
	for i := 0; i < 3; i++ {
		insertTestJob(t, s, fmt.Sprintf("sem-job-%d", i))
	}

	var maxConcurrent atomic.Int32
	var currentConcurrent atomic.Int32

	processFunc := func(ctx context.Context, job *Job) error {
		n := currentConcurrent.Add(1)
		// Track max concurrency
		for {
			old := maxConcurrent.Load()
			if n <= old || maxConcurrent.CompareAndSwap(old, n) {
				break
			}
		}
		time.Sleep(200 * time.Millisecond) // simulate work
		currentConcurrent.Add(-1)
		return nil
	}

	// SemSize=1 means only 1 concurrent process call
	w := NewWorker(WorkerConfig{
		Store:   s,
		Process: processFunc,
		Count:   3, // 3 workers but semaphore limits to 1
		SemSize: 1,
	})
	w.Start(context.Background())
	defer w.Stop()

	// Wait for all jobs to complete
	deadline := time.After(15 * time.Second)
	for {
		allDone := true
		for i := 0; i < 3; i++ {
			got, _ := s.Get(fmt.Sprintf("sem-job-%d", i))
			if got == nil || got.Status != StatusCompleted {
				allDone = false
				break
			}
		}
		if allDone {
			break
		}
		select {
		case <-deadline:
			t.Fatal("timed out waiting for semaphore jobs to complete")
		default:
			time.Sleep(100 * time.Millisecond)
		}
	}

	// Max concurrency should be exactly 1
	if maxConcurrent.Load() != 1 {
		t.Errorf("max concurrent = %d, want 1 (semaphore should limit)", maxConcurrent.Load())
	}
}

// ---------------------------------------------------------------------------
// Additional Edge Case Tests
// ---------------------------------------------------------------------------

func TestStore_UpdateStatus_NotFound(t *testing.T) {
	s := newTestStore(t)

	err := s.UpdateStatus("nonexistent", StatusCompleted, "", "")
	if !errors.Is(err, ErrJobNotFound) {
		t.Errorf("UpdateStatus: got %v, want ErrJobNotFound", err)
	}
}

func TestStore_CountByStatus(t *testing.T) {
	s := newTestStore(t)

	// Insert 5 pending jobs
	for i := 0; i < 5; i++ {
		insertTestJob(t, s, fmt.Sprintf("count-%d", i))
	}

	count, err := s.CountByStatus(StatusPending)
	if err != nil {
		t.Fatalf("CountByStatus: %v", err)
	}
	if count != 5 {
		t.Errorf("count = %d, want 5", count)
	}

	// Other statuses should be 0
	for _, st := range []Status{StatusRunning, StatusCompleted, StatusFailed, StatusDead} {
		c, err := s.CountByStatus(st)
		if err != nil {
			t.Fatalf("CountByStatus(%s): %v", st, err)
		}
		if c != 0 {
			t.Errorf("CountByStatus(%s) = %d, want 0", st, c)
		}
	}
}

func TestStore_Defaults_Applied(t *testing.T) {
	s := newTestStoreWithConfig(t, StoreConfig{
		MaxPending: 0, // should default to DefaultMaxPending
		JobTTL:     0, // should default to DefaultJobTTL
	})

	// Insert DefaultMaxPending jobs should succeed (if we had time), but let's just
	// verify we can insert at least one and that defaults were applied
	insertTestJob(t, s, "default-test")

	got, err := s.Get("default-test")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got == nil {
		t.Fatal("job not found")
	}

	// The store should have accepted the job (defaults work)
	if got.Status != StatusPending {
		t.Errorf("status = %q, want pending", got.Status)
	}
}

func TestStore_ClosedOperations(t *testing.T) {
	s := newTestStore(t)
	insertTestJob(t, s, "before-close")

	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Operations after close should return errors (not panic)
	job := &Job{ID: "after-close", Bank: "b", Type: "retain", Payload: "p"}
	if err := s.Insert(job); err == nil {
		t.Error("Insert after close should return error")
	}

	if _, err := s.NextPending(); err == nil {
		t.Error("NextPending after close should return error")
	}

	if err := s.UpdateStatus("x", StatusCompleted, "", ""); err == nil {
		t.Error("UpdateStatus after close should return error")
	}

	if _, err := s.Get("x"); err == nil {
		t.Error("Get after close should return error")
	}

	if _, err := s.CountByStatus(StatusPending); err == nil {
		t.Error("CountByStatus after close should return error")
	}

	if _, err := s.Stats(); err == nil {
		t.Error("Stats after close should return error")
	}
}

func TestStore_CloseIdempotent(t *testing.T) {
	s := newTestStore(t)

	for i := 0; i < 3; i++ {
		if err := s.Close(); err != nil {
			t.Fatalf("Close #%d: %v", i, err)
		}
	}
	// No panic = pass
}

func TestStore_PragmaVerification(t *testing.T) {
	s := newTestStore(t)

	// For :memory: databases, journal_mode returns "memory" not "wal".
	// We accept either "wal" or "memory" for journal_mode.
	var journalMode string
	if err := s.db.QueryRow("PRAGMA journal_mode").Scan(&journalMode); err != nil {
		t.Fatalf("PRAGMA journal_mode: %v", err)
	}
	if journalMode != "wal" && journalMode != "memory" {
		t.Errorf("PRAGMA journal_mode = %q, want 'wal' or 'memory'", journalMode)
	}

	var busyTimeout string
	if err := s.db.QueryRow("PRAGMA busy_timeout").Scan(&busyTimeout); err != nil {
		t.Fatalf("PRAGMA busy_timeout: %v", err)
	}
	if busyTimeout != "5000" {
		t.Errorf("PRAGMA busy_timeout = %q, want %q", busyTimeout, "5000")
	}

	var foreignKeys string
	if err := s.db.QueryRow("PRAGMA foreign_keys").Scan(&foreignKeys); err != nil {
		t.Fatalf("PRAGMA foreign_keys: %v", err)
	}
	if foreignKeys != "1" {
		t.Errorf("PRAGMA foreign_keys = %q, want %q", foreignKeys, "1")
	}
}

func TestStore_TTLCleanup_ContextCancellation(t *testing.T) {
	s := newTestStoreWithConfig(t, StoreConfig{JobTTL: 1 * time.Hour})

	ctx, cancel := context.WithCancel(context.Background())
	s.StartTTLCleanup(ctx, 50*time.Millisecond)

	// Cancel immediately
	cancel()

	// The goroutine should exit — we can't directly observe this without races,
	// but we can verify no panic occurs and the store remains functional.
	time.Sleep(200 * time.Millisecond)

	// Store should still work
	insertTestJob(t, s, "post-cancel")
	got, err := s.Get("post-cancel")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got == nil {
		t.Error("store should still work after TTL cleanup context cancellation")
	}
}

func TestStore_TTLCleanup_DisabledWhenTTLNegative(t *testing.T) {
	s := newTestStoreWithConfig(t, StoreConfig{JobTTL: -1})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// This should be a no-op (no goroutine spawned)
	s.StartTTLCleanup(ctx, 50*time.Millisecond)

	// Verify the store still works (no goroutine interference)
	insertTestJob(t, s, "no-ttl")
	got, err := s.Get("no-ttl")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got == nil {
		t.Error("store should work with disabled TTL")
	}
}

func TestJob_Validate_EdgeCases(t *testing.T) {
	tests := []struct {
		name    string
		job     Job
		wantErr string
	}{
		{
			name:    "empty ID",
			job:     Job{ID: "", Bank: "b", Type: "retain", Payload: "p"},
			wantErr: "ID",
		},
		{
			name:    "whitespace ID",
			job:     Job{ID: "   ", Bank: "b", Type: "retain", Payload: "p"},
			wantErr: "ID",
		},
		{
			name:    "empty bank",
			job:     Job{ID: "x", Bank: "", Type: "retain", Payload: "p"},
			wantErr: "bank",
		},
		{
			name:    "invalid type",
			job:     Job{ID: "x", Bank: "b", Type: "invalid", Payload: "p"},
			wantErr: "type",
		},
		{
			name:    "empty payload",
			job:     Job{ID: "x", Bank: "b", Type: "retain", Payload: ""},
			wantErr: "payload",
		},
		{
			name:    "max_retries > 10",
			job:     Job{ID: "x", Bank: "b", Type: "retain", Payload: "p", MaxRetries: 11},
			wantErr: "max_retries",
		},
		{
			name:    "max_retries negative",
			job:     Job{ID: "x", Bank: "b", Type: "retain", Payload: "p", MaxRetries: -1},
			wantErr: "max_retries",
		},
		{
			name:    "valid retain",
			job:     Job{ID: "x", Bank: "b", Type: "retain", Payload: "p"},
			wantErr: "",
		},
		{
			name:    "valid reflect",
			job:     Job{ID: "x", Bank: "b", Type: "reflect", Payload: "p"},
			wantErr: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.job.Validate()
			if tt.wantErr == "" {
				if err != nil {
					t.Errorf("unexpected error: %v", err)
				}
			} else {
				if err == nil {
					t.Errorf("expected error containing %q, got nil", tt.wantErr)
				} else if !containsStr(err.Error(), tt.wantErr) {
					t.Errorf("error %q does not contain %q", err.Error(), tt.wantErr)
				}
			}
		})
	}
}

func TestJob_CanRetry(t *testing.T) {
	tests := []struct {
		name string
		job  Job
		want bool
	}{
		{
			name: "failed with retries left",
			job:  Job{Status: StatusFailed, RetryCount: 0, MaxRetries: 3},
			want: true,
		},
		{
			name: "failed at max retries",
			job:  Job{Status: StatusFailed, RetryCount: 3, MaxRetries: 3},
			want: false,
		},
		{
			name: "failed over max retries",
			job:  Job{Status: StatusFailed, RetryCount: 5, MaxRetries: 3},
			want: false,
		},
		{
			name: "pending status",
			job:  Job{Status: StatusPending, RetryCount: 0, MaxRetries: 3},
			want: false,
		},
		{
			name: "running status",
			job:  Job{Status: StatusRunning, RetryCount: 0, MaxRetries: 3},
			want: false,
		},
		{
			name: "completed status",
			job:  Job{Status: StatusCompleted, RetryCount: 0, MaxRetries: 3},
			want: false,
		},
		{
			name: "dead status",
			job:  Job{Status: StatusDead, RetryCount: 3, MaxRetries: 3},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.job.CanRetry()
			if got != tt.want {
				t.Errorf("CanRetry() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestJob_Clone(t *testing.T) {
	original := &Job{
		ID:         "clone-test",
		Bank:       "bank",
		Type:       "retain",
		Payload:    "data",
		Status:     StatusRunning,
		RetryCount: 1,
		MaxRetries: 3,
		Result:     "res",
		Error:      "err",
		CreatedAt:  1000,
		UpdatedAt:  2000,
	}

	cloned := original.Clone()
	if cloned == nil {
		t.Fatal("Clone returned nil")
	}

	// All fields should match
	if cloned.ID != original.ID {
		t.Errorf("ID mismatch")
	}
	if cloned.Bank != original.Bank {
		t.Errorf("Bank mismatch")
	}
	if cloned.Type != original.Type {
		t.Errorf("Type mismatch")
	}
	if cloned.Payload != original.Payload {
		t.Errorf("Payload mismatch")
	}
	if cloned.Status != original.Status {
		t.Errorf("Status mismatch")
	}
	if cloned.RetryCount != original.RetryCount {
		t.Errorf("RetryCount mismatch")
	}
	if cloned.MaxRetries != original.MaxRetries {
		t.Errorf("MaxRetries mismatch")
	}
	if cloned.Result != original.Result {
		t.Errorf("Result mismatch")
	}
	if cloned.Error != original.Error {
		t.Errorf("Error mismatch")
	}
	if cloned.CreatedAt != original.CreatedAt {
		t.Errorf("CreatedAt mismatch")
	}
	if cloned.UpdatedAt != original.UpdatedAt {
		t.Errorf("UpdatedAt mismatch")
	}

	// Mutation of clone should not affect original
	cloned.Payload = "mutated"
	if original.Payload == "mutated" {
		t.Error("Clone is not a deep copy — mutation affected original")
	}
}

func TestJob_CloneNil(t *testing.T) {
	var j *Job
	if j.Clone() != nil {
		t.Error("Clone of nil should return nil")
	}
}

// ---------------------------------------------------------------------------
// Worker Pool: Idempotent Start/Stop and Panic Recovery
// ---------------------------------------------------------------------------

func TestWorkerPool_StartIdempotent(t *testing.T) {
	s := newTestStore(t)
	insertTestJob(t, s, "idempotent-job")

	var callCount atomic.Int32
	processFunc := func(ctx context.Context, job *Job) error {
		callCount.Add(1)
		return nil
	}

	w := NewWorker(WorkerConfig{
		Store:   s,
		Process: processFunc,
		Count:   2,
		SemSize: 2,
	})

	// Start twice — second should be a no-op
	w.Start(context.Background())
	w.Start(context.Background())

	// Wait for job to be processed
	deadline := time.After(5 * time.Second)
	for {
		got, _ := s.Get("idempotent-job")
		if got != nil && got.Status == StatusCompleted {
			break
		}
		select {
		case <-deadline:
			t.Fatal("timed out")
		default:
			time.Sleep(50 * time.Millisecond)
		}
	}

	w.Stop()
	// No panic = pass (double-start didn't double-spawn)
}

func TestWorkerPool_StopIdempotent(t *testing.T) {
	s := newTestStore(t)

	processFunc := func(ctx context.Context, job *Job) error {
		return nil
	}

	w := NewWorker(WorkerConfig{
		Store:   s,
		Process: processFunc,
		Count:   1,
		SemSize: 1,
	})

	w.Start(context.Background())
	w.Stop()
	w.Stop() // second stop should not panic
}

func TestWorkerPool_PanicRecovery(t *testing.T) {
	s := newTestStore(t)

	// Insert 2 jobs and use 2 workers. Worker 0 will panic on "panic-job",
	// worker 1 should still pick up "normal-job" and process it.
	// Per spec: "panic in workerLoop does NOT crash other workers".
	insertTestJob(t, s, "panic-job")
	insertTestJob(t, s, "normal-job")

	var normalProcessed atomic.Bool

	processFunc := func(ctx context.Context, job *Job) error {
		if job.ID == "panic-job" {
			panic("intentional test panic")
		}
		if job.ID == "normal-job" {
			normalProcessed.Store(true)
		}
		return nil
	}

	w := NewWorker(WorkerConfig{
		Store:   s,
		Process: processFunc,
		Count:   2, // 2 workers so surviving one processes normal-job
		SemSize: 2,
	})
	w.Start(context.Background())
	defer w.Stop()

	// Wait for the normal job to be processed by the surviving worker
	deadline := time.After(10 * time.Second)
	for !normalProcessed.Load() {
		select {
		case <-deadline:
			t.Fatal("timed out waiting for normal job after panic")
		default:
			time.Sleep(100 * time.Millisecond)
		}
	}

	got, err := s.Get("normal-job")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != StatusCompleted {
		t.Errorf("normal-job status = %q, want completed", got.Status)
	}
}

func TestWorkerPool_EmptyQueue(t *testing.T) {
	s := newTestStore(t)

	var processCalled atomic.Bool
	processFunc := func(ctx context.Context, job *Job) error {
		processCalled.Store(true)
		return nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	w := NewWorker(WorkerConfig{
		Store:   s,
		Process: processFunc,
		Count:   2,
		SemSize: 2,
	})
	w.Start(ctx)

	// Give workers time to poll empty queue
	time.Sleep(500 * time.Millisecond)

	cancel()
	w.Stop()

	// ProcessFunc should not have been called (no jobs in queue)
	if processCalled.Load() {
		t.Error("ProcessFunc should not have been called on empty queue")
	}
}

// ---------------------------------------------------------------------------
// Race detector stress test
// ---------------------------------------------------------------------------

func TestStore_RaceDetectorStress(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping stress test in short mode")
	}

	s := newTestStore(t)

	const numJobs = 100
	// Insert all jobs
	for i := 0; i < numJobs; i++ {
		job := &Job{
			ID:      fmt.Sprintf("stress-%d", i),
			Bank:    "bank",
			Type:    "retain",
			Payload: "data",
		}
		if err := s.Insert(job); err != nil {
			t.Fatalf("Insert(%d): %v", i, err)
		}
	}

	// Concurrent readers and writers
	var wg sync.WaitGroup

	// Claimers
	wg.Add(5)
	for i := 0; i < 5; i++ {
		go func() {
			defer wg.Done()
			for j := 0; j < 30; j++ {
				job, _ := s.NextPending()
				if job != nil {
					s.UpdateStatus(job.ID, StatusCompleted, "done", "")
				}
			}
		}()
	}

	// Readers
	wg.Add(5)
	for i := 0; i < 5; i++ {
		go func(idx int) {
			defer wg.Done()
			for j := 0; j < 30; j++ {
				s.Get(fmt.Sprintf("stress-%d", j%numJobs))
				s.CountByStatus(StatusPending)
				s.Stats()
			}
		}(i)
	}

	wg.Wait()
	// If we get here without race detector complaints, we pass
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func containsStr(s, substr string) bool {
	return len(s) >= len(substr) && searchStr(s, substr)
}

func searchStr(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

func runtimeNumGoroutines() int {
	return runtime.NumGoroutine()
}
