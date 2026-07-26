package main

import (
	"fmt"
	"time"
)

// sessionCleaner periodically closes and removes idle MCP sessions.
// Extracted from workers.go during M1 (Hindsight removal).
func (s *Server) sessionCleaner() {
	defer func() {
		if r := recover(); r != nil {
			s.panics.Add(1)
			s.log.Error("session cleaner goroutine panicked", "panic", fmt.Sprintf("%v", r))
			s.alerts.Send(AlertCritical, fmt.Sprintf("Session cleaner panicked: %v", r), nil)
		}
	}()
	s.log.Info("goroutine_started", "name", "session_cleaner")
	defer s.log.Info("goroutine_stopped", "name", "session_cleaner")
	ticker := time.NewTicker(s.config.SessionCleanInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
		case <-s.shutdown:
			return
		}

		// Phase 1: Collect stale session IDs under read lock (minimal contention)
		type staleSession struct {
			id   string
			sess *MCPSession
		}
		var stale []staleSession
		s.sessionsMu.RLock()
		now := time.Now()
		for id, sess := range s.sessions {
			if now.Sub(sess.LastActive) > s.config.SessionIdleTimeout {
				stale = append(stale, staleSession{id: id, sess: sess})
			}
		}
		s.sessionsMu.RUnlock()

		// Phase 2: Close stale sessions without holding the lock
		for _, st := range stale {
			st.sess.Close()
			s.log.Info("session cleaned", "id", st.id[:8], "idle", now.Sub(st.sess.LastActive).Round(time.Second).String())
		}

		// Phase 3: Remove cleaned sessions under write lock (brief)
		if len(stale) > 0 {
			s.sessionsMu.Lock()
			for _, st := range stale {
				delete(s.sessions, st.id)
			}
			s.sessionsMu.Unlock()
		}

		// Phase 4: Update metrics (brief read lock)
		s.sessionsMu.RLock()
		sessionCount := len(s.sessions)
		s.sessionsMu.RUnlock()

		s.metrics.sessionGauge.Set(int64(sessionCount))
		// M3: Read queue depth from SQLite queue store
		s.metrics.queueGauge.Set(pendingCount(s.queueStore))
		if sessionCount > s.config.MaxSessions*9/10 {
			s.log.Warn("approaching session limit", "sessions", sessionCount, "max", s.config.MaxSessions)
			s.alerts.Send(AlertWarn, fmt.Sprintf("Sessions at %d/%d", sessionCount, s.config.MaxSessions), nil)
		}
	}
}
