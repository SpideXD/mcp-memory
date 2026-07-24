package main

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// bankState tracks the auto-improve state for a single memory bank.
// Safe for concurrent use — all access is guarded by autoImproveState.mu.
type bankState struct {
	retainsSince    int64     // number of retains since last improve
	lastImprove     time.Time // last improve start time (UTC)
	improveInFlight bool      // goroutine currently running improve for this bank
}

// persistedBankState is the JSON-serializable form of bankState.
// improveInFlight is never persisted (goroutines don't survive restart).
type persistedBankState struct {
	RetainsSince int64     `json:"retains_since"`
	LastImprove  time.Time `json:"last_improve"`
}

// autoImproveState manages per-bank auto-improve state with persistence.
// Safe for concurrent use — locks mu for all reads and writes.
type autoImproveState struct {
	mu      sync.Mutex
	banks   map[string]*bankState
	dataDir string
}

// loadAutoImproveState reads improve_state.json from dataDir and returns
// a populated autoImproveState. If the file is missing, returns empty state.
// If corrupt, logs warning and returns empty state. No crash.
func loadAutoImproveState(dataDir string) *autoImproveState {
	state := &autoImproveState{
		banks:   make(map[string]*bankState),
		dataDir: dataDir,
	}

	path := filepath.Join(dataDir, "improve_state.json")
	data, err := os.ReadFile(path)
	if err != nil {
		// Missing file = start with empty state, no error
		return state
	}

	var persisted map[string]persistedBankState
	if err := json.Unmarshal(data, &persisted); err != nil {
		// Corrupt file = log warning, start with empty state
		fmt.Fprintf(os.Stderr, "WARN: corrupt %s, starting with empty state: %v\n", path, err)
		return state
	}

	for bank, ps := range persisted {
		state.banks[bank] = &bankState{
			retainsSince: ps.RetainsSince,
			lastImprove:  ps.LastImprove,
			// improveInFlight always false on load
		}
	}

	return state
}

// saveStateLocked persists the current state to disk.
// Caller MUST hold s.mu.
// Uses atomic write (temp + os.Rename). On failure, logs warning and skips.
func (s *autoImproveState) saveStateLocked() {
	if s.dataDir == "" {
		return
	}

	// Build persisted map (skip improveInFlight)
	persisted := make(map[string]persistedBankState, len(s.banks))
	for bank, bs := range s.banks {
		persisted[bank] = persistedBankState{
			RetainsSince: bs.retainsSince,
			LastImprove:  bs.lastImprove,
		}
	}

	data, err := json.Marshal(persisted)
	if err != nil {
		fmt.Fprintf(os.Stderr, "WARN: failed to marshal improve state: %v\n", err)
		return
	}

	// Ensure directory exists
	if err := os.MkdirAll(s.dataDir, 0755); err != nil {
		fmt.Fprintf(os.Stderr, "WARN: cannot create data dir %s: %v\n", s.dataDir, err)
		return
	}

	path := filepath.Join(s.dataDir, "improve_state.json")
	tmpPath := path + ".tmp"

	if err := os.WriteFile(tmpPath, data, 0644); err != nil {
		fmt.Fprintf(os.Stderr, "WARN: failed to write improve state: %v\n", err)
		return
	}

	if err := os.Rename(tmpPath, path); err != nil {
		fmt.Fprintf(os.Stderr, "WARN: failed to rename improve state: %v\n", err)
		os.Remove(tmpPath)
		return
	}
}

// maybeAutoImprove checks all 4 conditions and spawns an auto-improve goroutine
// if all are met. Called at the end of each Cognee retain goroutine.
//
// Four conditions must ALL be true:
//  1. retainsSince >= AUTO_IMPROVE_AFTER_N
//  2. len(cogneeSemaphore) <= 1 (idle: at most the caller's own slot)
//  3. !improveInFlight
//  4. time.Since(lastImprove) >= AUTO_IMPROVE_COOLDOWN
func (s *Server) maybeAutoImprove(bank string) {
	if s.config.AutoImproveAfterN <= 0 {
		return // disabled — no goroutines, no file I/O
	}
	if s.improveState == nil {
		return
	}

	// Defensive bank name validation (HIGH-5)
	if bank == "" || !bankNamePattern.MatchString(bank) {
		return
	}

	s.improveState.mu.Lock()

	// Get or create bank state
	bs, ok := s.improveState.banks[bank]
	if !ok {
		bs = &bankState{}
		s.improveState.banks[bank] = bs
	}

	// Increment retain counter (saturate at MaxInt64 to prevent overflow wrap)
	if bs.retainsSince < math.MaxInt64 {
		bs.retainsSince++
	}

	// Persist after counter increment
	s.improveState.saveStateLocked()

	// Check all 4 conditions
	thresholdMet := bs.retainsSince >= int64(s.config.AutoImproveAfterN)
	idleCheck := len(s.cogneeSemaphore) <= 1
	noInFlight := !bs.improveInFlight
	cooldownMet := bs.lastImprove.IsZero() || time.Since(bs.lastImprove) >= s.config.AutoImproveCooldown

	if !thresholdMet || !idleCheck || !noInFlight || !cooldownMet {
		s.improveState.mu.Unlock()
		return
	}

	// All conditions met — fire auto-improve
	bs.improveInFlight = true
	bs.lastImprove = time.Now().UTC()
	bs.retainsSince = 0

	// Persist after state mutation
	s.improveState.saveStateLocked()
	s.improveState.mu.Unlock()

	// Spawn auto-improve goroutine
	s.cogneeWg.Add(1)
	go func() {
		defer s.cogneeWg.Done()

		// Defer ordering per AC-M2.31: recover() must be the FIRST deferred
		// statement to execute. In Go, defers execute LIFO, so recover() is
		// registered as the FIRST defer (outermost after cogneeWg.Done) so it
		// executes LAST — but it is the FIRST to be registered, meaning all
		// subsequent defers run INSIDE its protection.
		// Register FIRST so it catches panics from all other defers and body.
		defer func() {
			if r := recover(); r != nil {
				if s.log != nil {
					s.log.Error("auto-improve goroutine panicked", "bank", bank, "panic", fmt.Sprintf("%v", r))
				}
				s.panics.Add(1)
			}
		}()

		// Nil-safety: capture log/metrics locally so nil struct fields
		// don't cause SIGSEGV during defer registration or body execution.
		log := s.log
		metrics := s.metrics

		// Nil-safe context: context.WithTimeout(nil, ...) panics before
		// recover() can catch it (defer args are evaluated at registration).
		if s.cogneeCtx == nil {
			s.cogneeCtx = context.Background()
		}

		// Detached context with timeout — cancelled on Stop()
		detachedCtx, cancel := context.WithTimeout(s.cogneeCtx, s.config.BackendReflectTimeout)
		defer cancel()

		// Log goroutine lifecycle (nil-safe: guard at registration time)
		if log != nil {
			defer log.Info("goroutine_stopped", "name", "auto_improve", "bank", bank)
		}

		// Reset improveInFlight on exit (success, error, or panic)
		defer func() {
			s.improveState.mu.Lock()
			if bs, ok := s.improveState.banks[bank]; ok {
				bs.improveInFlight = false
			}
			s.improveState.saveStateLocked()
			s.improveState.mu.Unlock()
		}()

		if log != nil {
			log.Info("goroutine_started", "name", "auto_improve", "bank", bank)
		}

		_, err := s.backend.Reflect(detachedCtx, bank, "") // empty query = full improve
		if err != nil {
			if log != nil {
				log.Error("auto-improve failed", "bank", bank, "error", err.Error())
			}
			if metrics != nil {
				metrics.errorCalls.Inc()
			}
		} else {
			if log != nil {
				log.Info("auto-improve completed", "bank", bank)
			}
		}
	}()
}
