package main

import (
	"fmt"
	"sync"
	"time"
)

// JobStatus represents the current state of a Cognee retain job.
type JobStatus string

const (
	JobPending   JobStatus = "pending"
	JobCompleted JobStatus = "completed"
	JobFailed    JobStatus = "failed"
)

// JobResult holds the outcome of an async Cognee retain operation.
type JobResult struct {
	JobID   string    `json:"job_id"`
	Bank    string    `json:"bank"`
	Status  JobStatus `json:"status"`
	Error   string    `json:"error,omitempty"`
	Result  string    `json:"result,omitempty"`
	Created time.Time `json:"created"`
	Updated time.Time `json:"updated"`
}

// JobStats holds aggregate job tracking statistics.
type JobStats struct {
	Total     int `json:"total"`
	Completed int `json:"completed"`
	Failed    int `json:"failed"`
	Pending   int `json:"pending"`
}

// jobTracker is an in-memory job result store with TTL-based cleanup.
// Safe for concurrent use. Only allocated when BACKEND is not "hindsight".
type jobTracker struct {
	mu   sync.RWMutex
	jobs map[string]*JobResult
	ttl  time.Duration
}

// newJobTracker creates a job tracker with the given TTL for entry expiration.
func newJobTracker(ttl time.Duration) *jobTracker {
	return &jobTracker{
		jobs: make(map[string]*JobResult),
		ttl:  ttl,
	}
}

func (jt *jobTracker) store(jobID, bank string) {
	jt.mu.Lock()
	defer jt.mu.Unlock()
	jt.jobs[jobID] = &JobResult{
		JobID:   jobID,
		Bank:    bank,
		Status:  JobPending,
		Created: time.Now(),
		Updated: time.Now(),
	}
}

func (jt *jobTracker) complete(jobID, result string) {
	jt.mu.Lock()
	defer jt.mu.Unlock()
	if job, ok := jt.jobs[jobID]; ok {
		job.Status = JobCompleted
		job.Result = result
		job.Updated = time.Now()
	}
}

func (jt *jobTracker) fail(jobID, errMsg string) {
	jt.mu.Lock()
	defer jt.mu.Unlock()
	if job, ok := jt.jobs[jobID]; ok {
		job.Status = JobFailed
		job.Error = errMsg
		job.Updated = time.Now()
	}
}

func (jt *jobTracker) get(jobID string) *JobResult {
	jt.mu.RLock()
	defer jt.mu.RUnlock()
	job, ok := jt.jobs[jobID]
	if !ok {
		return nil
	}
	// Return a defensive copy
	cp := *job
	return &cp
}

func (jt *jobTracker) stats() JobStats {
	jt.mu.RLock()
	defer jt.mu.RUnlock()
	var s JobStats
	s.Total = len(jt.jobs)
	for _, j := range jt.jobs {
		switch j.Status {
		case JobCompleted:
			s.Completed++
		case JobFailed:
			s.Failed++
		case JobPending:
			s.Pending++
		}
	}
	return s
}

// cleanup removes entries older than TTL. Called by TTL cleaner goroutine.
func (jt *jobTracker) cleanup() {
	jt.mu.Lock()
	defer jt.mu.Unlock()
	cutoff := time.Now().Add(-jt.ttl)
	for id, job := range jt.jobs {
		if job.Updated.Before(cutoff) {
			delete(jt.jobs, id)
		}
	}
}

// jobTrackerCleanup runs the TTL cleaner goroutine.
func (s *Server) jobTrackerCleanup() {
	defer func() {
		if r := recover(); r != nil {
			s.panics.Add(1)
			s.log.Error("job tracker cleanup goroutine panicked", "panic", fmt.Sprintf("%v", r))
		}
	}()
	s.log.Info("goroutine_started", "name", "job_tracker_cleanup")
	s.log.Info("job tracker cleanup goroutine started")
	defer s.log.Info("goroutine_stopped", "name", "job_tracker_cleanup")
	defer s.log.Info("job tracker cleanup goroutine stopped")

	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			if s.jobTracker != nil {
				s.jobTracker.cleanup()
				s.log.Debug("job tracker cleanup", "jobs", len(s.jobTracker.jobs))
			}
		case <-s.shutdown:
			return
		}
	}
}
