package main

import (
	"log"
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
			log.Printf("checkAutoReflect panicked for bank %s: %v", bank, r)
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
	val, _ := s.reflectStates.LoadOrStore(bank, &reflectState{lastReflect: time.Now()})
	rs := val.(*reflectState)

	// Lock for the check-and-update phase
	rs.mu.Lock()

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
		log.Printf("WARN: auto_reflect: queue store not available for bank %s", bank)
		return
	}

	// Insert reflect job
	job := &queue.Job{
		ID:         newJobID(),
		Bank:       bank,
		Type:       "reflect",
		Payload:    "_auto",
		MaxRetries: 0, // uses default (3) in Store.Insert
	}
	if err := s.queueStore.Insert(job); err != nil {
		log.Printf("auto_reflect: failed to insert reflect job for bank %s: %v", bank, err)
		return
	}

	// Update pending gauge
	if s.metrics != nil && s.metrics.cogneePending != nil {
		stats, err := s.queueStore.Stats()
		if err == nil {
			s.metrics.cogneePending.Set(int64(stats.Pending))
		}
	}

	log.Printf("auto_reflect: job inserted for bank %s (trigger=%s)", bank, triggerReason(countTrigger, timeoutTrigger))
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
