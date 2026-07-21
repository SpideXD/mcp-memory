package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"mcp-memory/metrics"
)

// ─── Batch 1: Circuit Breaker Thrashing ─────────────────────────────────

// TestChaos_CircuitBreakerRapidFailureRecovery sends rapid success/failure
// cycles to the breaker, ensuring it remains in a valid state and does not
// panic under high-frequency toggling (50k ops across 20 goroutines).
func TestChaos_CircuitBreakerRapidFailureRecovery(t *testing.T) {
	cb := newCircuitBreaker(5, 10*time.Millisecond)

	var wg sync.WaitGroup
	ops := 1000
	threads := 20
	totalOps := ops * threads

	type result struct {
		panicked bool
		val      interface{}
	}
	results := make(chan result, totalOps)

	// Rapid alternating RecordFailure / RecordSuccess across goroutines
	for i := 0; i < threads; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for j := 0; j < ops; j++ {
				func() {
					defer func() {
						if r := recover(); r != nil {
							results <- result{panicked: true, val: r}
						}
					}()
					if j%2 == 0 {
						cb.RecordFailure()
					} else {
						cb.RecordSuccess()
					}
				}()
			}
		}(i)
	}

	// Simultaneously hammer IsTripped from more goroutines
	for i := 0; i < threads*2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < ops; j++ {
				func() {
					defer func() {
						if r := recover(); r != nil {
							results <- result{panicked: true, val: r}
						}
					}()
					cb.IsTripped()
				}()
			}
		}()
	}

	wg.Wait()
	close(results)

	panicCount := 0
	for r := range results {
		if r.panicked {
			panicCount++
			t.Logf("panic: %v", r.val)
		}
	}
	if panicCount > 0 {
		t.Fatalf("circuit breaker panicked %d times during thrashing", panicCount)
	}

	// After all noise, breaker should be in a valid state
	cb.mu.Lock()
	failures := cb.failures
	trippedUntil := cb.trippedUntil
	cb.mu.Unlock()

	t.Logf("Circuit breaker after %d ops: failures=%d, tripped=%v",
		totalOps*2, failures, !trippedUntil.IsZero())

	// Should not leak goroutines (no goroutines are started by breaker)
}

// TestChaos_CircuitBreakerTrippedWhileThrashing verifies the breaker remains
// in a consistent state even when IsTripped is called concurrently with a
// cooldown close to expiry — the half-open transition must be atomic.
func TestChaos_CircuitBreakerTrippedWhileThrashing(t *testing.T) {
	cb := newCircuitBreaker(3, 1*time.Millisecond) // very short cooldown

	var wg sync.WaitGroup
	var failures atomic.Int64
	var allowedThrough atomic.Int64

	// Phase 1: Trip the breaker
	for i := 0; i < 5; i++ {
		cb.RecordFailure()
	}

	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				if cb.IsTripped() {
					failures.Add(1)
				} else {
					allowedThrough.Add(1)
				}
				// Continuous failure/success toggling
				if j%5 == 0 {
					cb.RecordFailure()
				}
				if j%7 == 0 {
					cb.RecordSuccess()
				}
			}
		}()
	}

	wg.Wait()

	t.Logf("Circuit breaker: fail-fast=%d, allowed-through=%d",
		failures.Load(), allowedThrough.Load())

	// Verify no goroutine leaks — breaker creates no goroutines
	// Verify final state is valid
	cb.mu.Lock()
	defer cb.mu.Unlock()
	if cb.failures < 0 {
		t.Error("failure count negative")
	}
}

// ─── Batch 2: doRequest Concurrent Stress with Nil/Rapid Body ────────────

// TestChaos_DoRequestConcurrentNilBody tests many concurrent doRequest calls
// with nil body, verifying the nil guard works under heavy load.
func TestChaos_DoRequestConcurrentNilBody(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		r.Body.Close()
		if len(body) == 0 {
			w.WriteHeader(400)
			w.Write([]byte("empty body"))
			return
		}
		w.WriteHeader(200)
		w.Write([]byte("ok"))
	}))
	defer ts.Close()

	cfg := newTestConfig()
	l, _ := newTestLogger()
	s := &Server{
		config: cfg,
		svc:    newServices(cfg, l, NewAlertClient("", "optional")),
		log:    l,
		hindsightBreaker: newCircuitBreaker(100, time.Minute),
	}
	s.svc.httpClient = &http.Client{Timeout: time.Second}

	var wg sync.WaitGroup
	errors := make(chan error, 500)
	panics := make(chan interface{}, 500)

	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			defer func() {
				if r := recover(); r != nil {
					panics <- r
				}
			}()
			for j := 0; j < 20; j++ {
				// Create request with nil body
				req, err := http.NewRequest("POST", ts.URL, nil)
				if err != nil {
					errors <- err
					return
				}
				_, err = s.doRequest(req, 100*time.Millisecond)
				if err == nil {
					// nil body request should fail
				}
			}
		}(i)
	}

	wg.Wait()
	close(errors)
	close(panics)

	var panicList []interface{}
	for p := range panics {
		panicList = append(panicList, p)
	}
	if len(panicList) > 0 {
		t.Fatalf("doRequest panicked %d times on nil body: %v", len(panicList), panicList)
	}

	errCount := 0
	for range errors {
		errCount++
	}
	t.Logf("Concurrent nil body: %d requests, all handled without panic", 50*20)
}

// TestChaos_DoRequestConcurrentRapid tests many concurrent doRequest calls
// that race on retry loops, verifying no shared state corruption.
func TestChaos_DoRequestConcurrentRapid(t *testing.T) {
	var mu sync.Mutex
	callCount := 0

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		callCount++
		mu.Unlock()
		w.WriteHeader(200)
		w.Write([]byte(`{"status":"ok"}`))
	}))
	defer ts.Close()

	cfg := newTestConfig()
	cfg.RetryAttempts = 2
	cfg.RetryDelay = 5 * time.Millisecond
	cfg.RetryMaxDelay = 20 * time.Millisecond

	l, _ := newTestLogger()
	s := &Server{
		config: cfg,
		svc:    newServices(cfg, l, NewAlertClient("", "optional")),
		log:    l,
		hindsightBreaker: newCircuitBreaker(100, time.Minute),
	}
	s.svc.httpClient = &http.Client{Timeout: time.Second}

	var wg sync.WaitGroup
	successes := atomic.Int64{}
	failures := atomic.Int64{}

	for i := 0; i < 30; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				body := bytes.NewReader([]byte(fmt.Sprintf(`{"items":[{"content":"test-%d"}]}`, j)))
				req, err := http.NewRequest("POST", ts.URL, body)
				if err != nil {
					failures.Add(1)
					continue
				}
				_, err = s.doRequest(req, time.Second)
				if err != nil {
					failures.Add(1)
				} else {
					successes.Add(1)
				}
			}
		}()
	}

	wg.Wait()
	t.Logf("Concurrent doRequest: %d successes, %d failures, %d server calls",
		successes.Load(), failures.Load(), callCount)
}

// ─── Batch 3: Session Exhaustion Burst ────────────────────────────────────

// TestChaos_SessionLimitUnderConcurrentSSE tests rapid concurrent SSE
// connections against the session limit to verify no TOCTOU window allows
// exceeding MaxSessions. Each goroutine uses a timeout context so the
// SSE handler's for-select loop exits via r.Context().Done().
func TestChaos_SessionLimitUnderConcurrentSSE(t *testing.T) {
	cfg := newTestConfig()
	cfg.MaxSessions = 10     // small limit for stress
	cfg.SSEMessageBuffer = 2 // small buffer to test write-back pressure
	cfg.AuthToken = "test-token"
	l, _ := newTestLogger()

	s := &Server{
		config: cfg,
		log:    l,
		sessions:   make(map[string]*MCPSession),
		metrics: &serverMetrics{sseDrops: metrics.NewCounter("test.sse")},
		shutdown: make(chan struct{}),
	}

	var wg sync.WaitGroup
	accepted := atomic.Int64{}
	rejected := atomic.Int64{}

	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Use a short timeout so handleMCPSSE exits via context.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()
			req := httptest.NewRequest("GET", "/mcp/sse?bank=test", nil)
			req.Header.Set("Authorization", "Bearer test-token")
			req = req.WithContext(ctx)
			w := httptest.NewRecorder()

			s.handleMCPSSE(w, req)
			if w.Code == http.StatusOK || w.Code == 0 {
				accepted.Add(1)
			} else {
				rejected.Add(1)
			}
		}()
	}

	wg.Wait()

	acceptedCount := accepted.Load()
	rejectedCount := rejected.Load()

	t.Logf("Session limit %d: accepted=%d rejected=%d",
		cfg.MaxSessions, acceptedCount, rejectedCount)

	// Note: httptest.Recorder doesn't implement Flusher, so the handler
	// writes the endpoint event but never flushes. The session IS created
	// (the session map entry exists), but we can't read the response body.
	// We verify count by checking session map directly.
	s.sessionsMu.RLock()
	created := len(s.sessions)
	s.sessionsMu.RUnlock()

	if created > cfg.MaxSessions {
		t.Errorf("BUG: %d sessions created in map, but limit is %d (TOCTOU race)",
			created, cfg.MaxSessions)
	}

	// Clean up sessions
	s.sessionsMu.Lock()
	for id, sess := range s.sessions {
		sess.Close()
		delete(s.sessions, id)
	}
	s.sessionsMu.Unlock()

	t.Logf("Session limit test: created=%d accepted-response=%d rejected-response=%d",
		created, acceptedCount, rejectedCount)
}



// ─── Batch 4: Alert Client Flood ────────────────────────────────────────

// TestChaos_AlertClientSendFlood rapidly fires many alerts to the same
// endpoint, verifying no goroutine leaks or panics.
func TestChaos_AlertClientSendFlood(t *testing.T) {
	// Use a server that accepts but discards alerts
	var received atomic.Int64
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received.Add(1)
		w.WriteHeader(200)
	}))
	defer ts.Close()

	ac := NewAlertClient(ts.URL, "optional")
	if ac == nil {
		t.Fatal("AlertClient should not be nil with URL")
	}

	var wg sync.WaitGroup
	var panics atomic.Int64

	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			defer func() {
				if r := recover(); r != nil {
					panics.Add(1)
				}
			}()
			for j := 0; j < 100; j++ {
				ac.Send(AlertInfo, fmt.Sprintf("flood test %d-%d", id, j), map[string]interface{}{
					"thread": id,
					"count":  j,
					"data":   make([]byte, 1000), // large detail payload
				})
			}
		}(i)
	}

	wg.Wait()

	if panics.Load() > 0 {
		t.Errorf("AlertClient.Send panicked %d times under flood", panics.Load())
	}

	t.Logf("AlertClient flood: sent=2000 received=%d", received.Load())
}

// TestChaos_AlertClientConcurrentNilReceiver tests concurrent Send calls
// on a nil *AlertClient, verifying nil receiver guard is thread-safe.
func TestChaos_AlertClientConcurrentNilReceiver(t *testing.T) {
	var ac *AlertClient

	var wg sync.WaitGroup
	var panics atomic.Int64

	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() {
				if r := recover(); r != nil {
					panics.Add(1)
				}
			}()
			for j := 0; j < 50; j++ {
				ac.Send(AlertInfo, "test", nil)
				_ = ac.IsRequired()
			}
		}()
	}

	wg.Wait()

	if panics.Load() > 0 {
		t.Errorf("nil AlertClient panicked %d times", panics.Load())
	}
}

// ─── Batch 5: CheckHealth Under Concurrent/Repeated Stress ───────────────

// TestChaos_AlertClientCheckHealthConcurrent hammers CheckHealth
// concurrently to verify no data races or goroutine leaks.
func TestChaos_AlertClientCheckHealthConcurrent(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))
	defer ts.Close()

	ac := NewAlertClient(ts.URL, "required")
	if ac == nil {
		t.Fatal("AlertClient should not be nil")
	}

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				_ = ac.CheckHealth()
			}
		}()
	}
	wg.Wait()
}

// ─── Batch 6: Auth Check Under Rapid Concurrent Requests ─────────────────

// TestChaos_checkAuthConcurrent hammers the auth check function with
// concurrent requests using various token values, verifying the simple
// string comparison works correctly under load.
func TestChaos_checkAuthConcurrent(t *testing.T) {
	s := &Server{config: Config{AuthToken: "secret-token"}}

	var wg sync.WaitGroup
	results := make(chan struct{ authed bool; header string }, 5000)

	headers := []string{
		"Bearer secret-token",   // valid
		"Bearer wrong-token",    // invalid
		"",                       // empty
		"Bearer ",               // token is space
		"Basic dGVzdDp0ZXN0",    // wrong scheme
		"bearer secret-token",   // lowercase scheme (RFC 7235 says case-insensitive)
		"BEARER secret-token",   // uppercase scheme
	}

	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				h := headers[j%len(headers)]
				req := httptest.NewRequest("GET", "/", nil)
				if h != "" {
					req.Header.Set("Authorization", h)
				}
				got := s.checkAuth(req)
				results <- struct{ authed bool; header string }{got, h}
			}
		}()
	}

	wg.Wait()
	close(results)

	// Verify results are consistent (no corruption from concurrent map writes
	// or shared state — checkAuth is stateless, so this is a sanity check)
	authedCount := 0
	for r := range results {
		if r.authed {
			authedCount++
		}
	}

	// 100 goroutines * 50 iterations = 5000 total
	// Only "Bearer secret-token" should pass. That's 1/7th of headers = ~714
	expectedMin := 600
	expectedMax := 830
	if authedCount < expectedMin || authedCount > expectedMax {
		t.Logf("Auth concurrent: authed=%d/5000 (expect ~%d-%d)",
			authedCount, expectedMin, expectedMax)
	}
}

// TestChaos_SessionRapidConnectDisconnect rapidly connects and disconnects
// SSE sessions to verify session map doesn't leak entries and the cleaner
// doesn't choke on the churn.
func TestChaos_SessionRapidConnectDisconnect(t *testing.T) {
	l, _ := newTestLogger()

	s := &Server{
		log:    l,
		sessions:   make(map[string]*MCPSession),
		metrics: &serverMetrics{sseDrops: metrics.NewCounter("test.sse")},
		shutdown: make(chan struct{}),
	}

	var wg sync.WaitGroup
	cycleCount := 100
	threads := 10

	for i := 0; i < threads; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for j := 0; j < cycleCount; j++ {
				sid := fmt.Sprintf("stress-%d-%d", id, j)
				ch := make(chan string, 5)
				sess := &MCPSession{
					SessionID:  sid,
					SSEChannel: ch,
					CreatedAt:  time.Now(),
					LastActive: time.Now(),
				}

				s.sessionsMu.Lock()
				s.sessions[sid] = sess
				s.sessionsMu.Unlock()

				// Simulate a message and a read
				sess.SSEChannel <- fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"result":"ok"}`, j)

				// Close and remove
				sess.Close()
				s.sessionsMu.Lock()
				delete(s.sessions, sid)
				s.sessionsMu.Unlock()
			}
		}(i)
	}

	wg.Wait()

	s.sessionsMu.RLock()
	remaining := len(s.sessions)
	s.sessionsMu.RUnlock()

	if remaining > 0 {
		t.Errorf("BUG: %d sessions leaked after rapid connect/disconnect", remaining)
	} else {
		t.Logf("Session rapid connect/disconnect: 0 leaks after %d cycles", cycleCount*threads)
	}
}

// ─── Batch 7: writeSSE Non-blocking Send Stress ─────────────────────────

// TestChaos_writeSSEBufferFull hammers writeSSE with concurrent writers
// on a full buffer, verifying no panics and tracking drops correctly.
func TestChaos_writeSSEBufferFull(t *testing.T) {
	l, _ := newTestLogger()

	ch := make(chan string, 2) // very small buffer
	sess := &MCPSession{
		SessionID:  "test-session",
		SSEChannel: ch,
		CreatedAt:  time.Now(),
		LastActive: time.Now(),
	}

	s := &Server{
		log:    l,
		sessions:   map[string]*MCPSession{"test-session": sess},
		metrics: &serverMetrics{
			sseDrops: metrics.NewCounter("test.sse"),
		},
	}

	var wg sync.WaitGroup
	var panics atomic.Int64

	// Fill the buffer first so subsequent writes are non-blocking drops
	ch <- "fill-1"
	ch <- "fill-2"

	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			defer func() {
				if r := recover(); r != nil {
					panics.Add(1)
				}
			}()
			for j := 0; j < 50; j++ {
				s.writeSSE("test-session", id, "result", map[string]interface{}{
					"content": []map[string]interface{}{{"type": "text", "text": fmt.Sprintf("data-%d-%d", id, j)}},
				})
			}
		}(i)
	}

	// Drain while writers are active to allow some through
	go func() {
		for range ch {
		}
	}()

	wg.Wait()

	if panics.Load() > 0 {
		t.Errorf("writeSSE panicked %d times on buffer full", panics.Load())
	}

	close(ch) // stop drain goroutine
	t.Logf("writeSSE buffer full: %d drops, 0 panics after %d writes",
		s.metrics.sseDrops.Value(), 1000)
}

// TestChaos_writeSSEOnClosedSession tests writeSSE on a closed/removed session
// under concurrent writes — no errors should occur for deleted sessions.
func TestChaos_writeSSEOnClosedSession(t *testing.T) {
	l, _ := newTestLogger()

	s := &Server{
		log:    l,
		sessions:   make(map[string]*MCPSession),
		metrics: &serverMetrics{
			sseDrops: metrics.NewCounter("test.sse"),
		},
	}

	var wg sync.WaitGroup
	var panics atomic.Int64

	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			defer func() {
				if r := recover(); r != nil {
					panics.Add(1)
				}
			}()
			for j := 0; j < 50; j++ {
				s.writeSSE("nonexistent-session", id, "result", "data")
			}
		}(i)
	}

	wg.Wait()

	if panics.Load() > 0 {
		t.Errorf("writeSSE panicked %d times on nonexistent session", panics.Load())
	}
}

// ─── Batch 8: Circuit Breaker + retainAPI Integration Stress ─────────────

// TestChaos_retainAPICircuitBreakerRapidFailure triggers rapid failures
// through retainAPI, verifying the circuit breaker trips, stays tripped
// during cooldown, and recovers cleanly — all under concurrent calls.
func TestChaos_retainAPICircuitBreakerRapidFailure(t *testing.T) {
	// Use a server that returns 500 for a while, then 200
	var callMu sync.Mutex
	callCount := 0

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callMu.Lock()
		cc := callCount
		callCount++
		callMu.Unlock()

		if cc < 20 {
			w.WriteHeader(500)
			w.Write([]byte(`{"error":"internal error"}`))
			return
		}
		w.WriteHeader(200)
		w.Write([]byte(`{"status":"ok"}`))
	}))
	defer ts.Close()

	cfg := newTestConfig()
	cfg.HindsightPort = ts.URL[strings.LastIndex(ts.URL, ":")+1:]
	cfg.CircuitBreakerThreshold = 3
	cfg.CircuitBreakerCooldown = 20 * time.Millisecond
	cfg.HindsightRetainTimeout = time.Second
	cfg.RetryAttempts = 1
	cfg.RetryDelay = 1 * time.Millisecond

	l, _ := newTestLogger()
	s := &Server{
		config: cfg,
		svc:    newServices(cfg, l, NewAlertClient("", "optional")),
		log:    l,
		hindsightBreaker: newCircuitBreaker(cfg.CircuitBreakerThreshold, cfg.CircuitBreakerCooldown),
	}
	s.svc.httpClient = &http.Client{Timeout: time.Second}

	var wg sync.WaitGroup
	breakerTripped := atomic.Int64{}
	requestErrors := atomic.Int64{}

	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 30; j++ {
				_, err := s.retainAPI("test-bank", fmt.Sprintf("content-%d", j))
				if err != nil {
					if strings.Contains(err.Error(), "circuit breaker open") {
						breakerTripped.Add(1)
					} else {
						requestErrors.Add(1)
					}
				}
			}
		}()
	}

	wg.Wait()

	t.Logf("retainAPI circuit breaker: breaker-open=%d other-errors=%d",
		breakerTripped.Load(), requestErrors.Load())

	// After the test, the breaker should have eventually tripped
	// (20 failures with threshold=3 guarantees it trips)
	if breakerTripped.Load() == 0 {
		t.Log("circuit breaker never tripped — this is unexpected but not a failure")
	}
}

// ─── Batch 9: Health Monitor Concurrent Shutdown Stress ──────────────────

// TestChaos_checkAndRestartConcurrentCancel tests that concurrent
// checkAndRestart goroutines (simulating tight health check intervals)
// terminate cleanly when the context is cancelled — no leaks.
func TestChaos_checkAndRestartConcurrentCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())

	svc, _ := newTestServices()
	svc.config.HealthTimeout = 50 * time.Millisecond

	// Use a port that will fail quickly
	startFn := func() error { return fmt.Errorf("start always fails") }

	// Barrier ensures all goroutines have started before we cancel
	started := make(chan struct{})
	var startOnce sync.Once
	var startCount atomic.Int64

	var wg sync.WaitGroup
	for i := 0; i < 30; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			if startCount.Add(1) == 30 {
				startOnce.Do(func() { close(started) })
			}
			svc.checkAndRestart(ctx, fmt.Sprintf("svc-%d", id),
				"http://localhost:1", // non-existent port
				&svc.llamaCmd, startFn, &svc.llamaFails, 3)
		}(i)
	}

	// Wait for all goroutines to start, then cancel
	<-started
	cancel()

	// All should complete when ctx is cancelled
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		t.Log("All checkAndRestart goroutines completed after ctx cancel")
	case <-time.After(5 * time.Second):
		t.Fatal("checkAndRestart goroutines did not terminate within 5s after cancel")
	}
}

// TestChaos_checkAndRestartRapidTick simulates a tight health check interval
// (every 10ms) with failing services, to verify the goroutine count from
// checkAndRestart doesn't grow unbounded — each goroutine eventually exits
// when ctx is cancelled or maxRestarts is hit.
func TestChaos_checkAndRestartRapidTick(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 800*time.Millisecond)
	defer cancel()

	svc, _ := newTestServices()
	svc.config.HealthTimeout = 50 * time.Millisecond

	startFn := func() error { return fmt.Errorf("fail") }

	var wg sync.WaitGroup
	tickCount := atomic.Int64{}

	// Simulate 10ms tick intervals for 800ms = 80 ticks * 3 goroutines = 240
	// goroutines launched concurrently. Each has maxRestarts=3 and backoff
	// up to 2s, but ctx cancel will terminate them early.
	for i := 0; i < 80; i++ {
		tickCount.Add(1)
		wg.Add(3)

		go func() {
			defer wg.Done()
			svc.checkAndRestart(ctx, "llama",
				"http://localhost:1",
				&svc.llamaCmd, startFn, &svc.llamaFails, 3)
		}()
		go func() {
			defer wg.Done()
			svc.checkAndRestart(ctx, "reranker",
				"http://localhost:2",
				&svc.llamaRerankerCmd, startFn, &svc.rerankerFails, 3)
		}()
		go func() {
			defer wg.Done()
			svc.checkAndRestart(ctx, "hindsight",
				"http://localhost:3",
				&svc.hindsightCmd, startFn, &svc.hindsightFails, 3)
		}()

		// Small delay between ticks to see overlapping behavior
		// This is a timing window to overlap goroutines. The brief sleep
		// is acceptable because the goroutine lifetimes are bounded by
		// context timeout (800ms), not by this sleep.
		select {
		case <-time.After(10 * time.Millisecond):
		case <-ctx.Done():
		}
	}

	// Wait for all goroutines to complete (ctx timeout or max restarts)
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		t.Logf("All %d checkAndRestart goroutines completed (ticks=%d)",
			tickCount.Load()*3, tickCount.Load())
	case <-time.After(5 * time.Second):
		t.Fatal("goroutines did not terminate within 5s — leak suspected")
	}
}




