package main

import (
	"errors"
	"sync"
	"time"

	"mcp-memory/queue"
)

// reflectState tracks auto-reflect triggers for a single memory bank.
// Safe for concurrent use — all field access is guarded by mu.
type reflectState struct {
	mu          sync.Mutex
	retainCount int       // successful retains since last auto-reflect
	lastReflect time.Time // time of last auto-reflect trigger (UTC)
	lastAccess  time.Time // last time this state was accessed (for TTL eviction)
}

// checkAutoReflect checks whether an auto-reflect job should be inserted
// for the given bank based on count or timeout thresholds.
//
// This method is panic-safe: any panic is recovered and logged, and the
// retain job that triggered this call remains in its successful state.
func (s *Server) checkAutoReflect(bank string) {
	// Panic recovery — must NOT re-panic or propagate errors to the worker.
	// The retain job already succeeded and must stay completed.
	defer func() {
		if r := recover(); r != nil {
			s.panics.Add(1)
			s.log.Error("auto_reflect panicked", "bank", bank, "panic", r)
		}
	}()

	// Guard 1: both triggers disabled → fast return
	if s.config.AutoReflectAfterN <= 0 && s.config.AutoReflectTimeout <= 0 {
		return
	}

	// Guard 2: defensive bank validation
	if bank == "" || !bankNamePattern.MatchString(bank) {
		return
	}

	// Get or create per-bank state, initialize lastReflect if new
	val, _ := s.reflectStates.LoadOrStore(bank, &reflectState{lastReflect: time.Now(), lastAccess: time.Now()})
	rs := val.(*reflectState)

	// Lock for the check-and-update phase
	rs.mu.Lock()
	rs.lastAccess = time.Now()

	rs.retainCount++

	// Check count-based trigger
	countTrigger := s.config.AutoReflectAfterN > 0 && rs.retainCount >= s.config.AutoReflectAfterN

	// Check timeout-based trigger
	timeoutTrigger := false
	if s.config.AutoReflectTimeout > 0 && rs.retainCount > 0 {
		if time.Since(rs.lastReflect) > s.config.AutoReflectTimeout {
			timeoutTrigger = true
		}
	}

	// No trigger condition met → unlock and return
	if !countTrigger && !timeoutTrigger {
		rs.mu.Unlock()
		return
	}

	// Fire: reset state BEFORE Insert (so re-queue on Insert failure is safe)
	rs.retainCount = 0
	rs.lastReflect = time.Now().UTC()
	rs.mu.Unlock() // release lock before I/O

	// Guard 3: queue store may be nil (server starting up)
	if s.queueStore == nil {
		s.log.Warn("auto_reflect skipped", "reason", "queue store not available", "bank", bank)
		return
	}

	// Insert reflect job
	job := &queue.Job{
		ID:         newJobID(),
		Bank:       bank,
		Type:       "reflect",
		Payload:    "_auto",
		MaxRetries: 0,
	}
	if err := s.queueStore.Insert(job); err != nil {
		if errors.Is(err, queue.ErrQueueFull) {
			s.log.Warn("auto_reflect skipped", "reason", "queue full", "bank", bank)
		} else {
			s.log.Error("auto_reflect insert failed", "bank", bank, "error", err)
		}
		return
	}

	// Update pending gauge
	s.metrics.cogneePending.Set(pendingCount(s.queueStore))

	s.log.Info("job_queued", "job_id", job.ID, "bank", bank, "type", "reflect", "trigger", triggerReason(countTrigger, timeoutTrigger))
	s.log.Info("auto_reflect triggered", "bank", bank, "trigger", triggerReason(countTrigger, timeoutTrigger))
}

// triggerReason returns a human-readable trigger reason.
func triggerReason(countTrigger, timeoutTrigger bool) string {
	if countTrigger && timeoutTrigger {
		return "both"
	}
	if countTrigger {
		return "count"
	}
	return "timeout"
}

// cleanupReflectStates removes reflectState entries that haven't been accessed
// within the given TTL. Called periodically by a background goroutine to prevent
// unbounded memory growth from one-shot banks (M4 QA finding).
func (s *Server) cleanupReflectStates(ttl time.Duration) {
	cutoff := time.Now().Add(-ttl)
	s.reflectStates.Range(func(key, value interface{}) bool {
		rs := value.(*reflectState)
		rs.mu.Lock()
		stale := rs.lastAccess.Before(cutoff)
		rs.mu.Unlock()
		if stale {
			s.reflectStates.Delete(key)
		}
		return true
	})
}

// autoReflectCleanup periodically evicts stale reflectState entries.
// Uses the outer-loop/inner-func panic recovery pattern: a panic inside
// the inner func is caught, the goroutine survives and retries next tick.
func (s *Server) autoReflectCleanup(shutdown chan struct{}) {
	ticker := time.NewTicker(1 * time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			func() {
				defer func() {
					if r := recover(); r != nil {
						s.panics.Add(1)
						s.log.Error("autoReflectCleanup panicked", "panic", r)
					}
				}()
				s.cleanupReflectStates(24 * time.Hour)
			}()
		case <-shutdown:
			return
		}
	}
}
