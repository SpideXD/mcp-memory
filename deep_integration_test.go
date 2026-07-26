package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"mcp-memory/internal/testutil/cogneemock"
	"mcp-memory/queue"
)

// ─── Backend faults propagating into job state ──────────────────────────────
//
// These drive the full path — MCP tool call, queue insert, worker, backend,
// terminal status — with the backend misbehaving in each way it realistically
// can. Previously the only integration coverage called processQueueJob
// directly, so none of the failure translation was exercised over the wire.

// TestDeep_BackendFailureMarksJobFailed proves a 5xx backend does not leave
// jobs stuck in "running" forever.
func TestDeep_BackendFailureMarksJobFailed(t *testing.T) {
	h := newHTTPHarness(t)
	h.mock.SetResponse("/api/v1/remember", cogneemock.ResponseConfig{
		StatusCode: 500, Body: `{"error":"pipeline exploded"}`,
	})
	c := h.mustConnect("bank")

	jobID := jobIDFrom(t, c.callTool("memory_retain",
		map[string]interface{}{"content": "doomed 2026"}).toolText(t))

	job := h.waitForJobStatus(t, jobID, queue.StatusDead)
	if job.Error == "" {
		t.Error("dead job must record why it failed")
	}
	if !strings.Contains(job.Error, "500") {
		t.Errorf("error should name the backend status, got: %q", job.Error)
	}
	if job.RetryCount == 0 {
		t.Error("a retried job should record its retry count")
	}
}

// TestDeep_BackendTimeoutMarksJobFailed uses latency injection to blow the
// per-operation budget, which no previous test could express.
func TestDeep_BackendTimeoutMarksJobFailed(t *testing.T) {
	h := newHTTPHarness(t, func(c *Config) {
		c.CogneeRetainTimeout = 300 * time.Millisecond
		c.BackendRetainTimeout = 300 * time.Millisecond
	})
	h.mock.SetLatency("/api/v1/remember", 10*time.Second)
	c := h.mustConnect("bank")

	jobID := jobIDFrom(t, c.callTool("memory_retain",
		map[string]interface{}{"content": "slow 2026"}).toolText(t))

	job := h.waitForJobStatus(t, jobID, queue.StatusDead)
	if job.Error == "" {
		t.Error("timed-out job must record an error")
	}
}

// TestDeep_BackendHangDoesNotWedgeTheWorkerPool is the liveness property that
// matters most: one hung job must not stop every other job forever.
func TestDeep_BackendHangDoesNotWedgeTheWorkerPool(t *testing.T) {
	h := newHTTPHarness(t, func(c *Config) {
		c.QueueWorkerCount = 2
		c.QueueMaxConcurrent = 2
		c.CogneeRetainTimeout = 500 * time.Millisecond
		c.BackendRetainTimeout = 500 * time.Millisecond
	})
	h.mock.SetBehavior("/api/v1/remember", cogneemock.BehaviorHang)
	c := h.mustConnect("bank")

	var ids []string
	for i := 0; i < 4; i++ {
		ids = append(ids, jobIDFrom(t, c.callTool("memory_retain",
			map[string]interface{}{"content": fmt.Sprintf("hang %d 2026", i)}).toolText(t)))
	}

	// Every job must reach a terminal state despite the backend never answering.
	for _, id := range ids {
		h.waitForJobStatus(t, id, queue.StatusDead)
	}
}

// TestDeep_MalformedBackendResponseStillCompletes documents the contract: the
// server treats a 200 as success and passes the payload through. Cognee's
// response shape is the backend's problem, not a reason to fail the job.
func TestDeep_MalformedBackendResponseStillCompletes(t *testing.T) {
	h := newHTTPHarness(t)
	h.mock.SetBehavior("/api/v1/remember", cogneemock.BehaviorMalformedJSON)
	c := h.mustConnect("bank")

	jobID := jobIDFrom(t, c.callTool("memory_retain",
		map[string]interface{}{"content": "garbled 2026"}).toolText(t))
	h.waitForJobStatus(t, jobID, queue.StatusCompleted)
}

// TestDeep_TransientFailureRecoversViaRetry uses a response sequence to prove
// the retry path actually salvages a flaky backend rather than giving up.
func TestDeep_TransientFailureRecoversViaRetry(t *testing.T) {
	h := newHTTPHarness(t)
	h.mock.SetSequence("/api/v1/remember", []cogneemock.ResponseConfig{
		{StatusCode: 503, Body: `{"e":"warming up"}`},
		{StatusCode: 503, Body: `{"e":"warming up"}`},
		{StatusCode: 200, Body: `{"status":"completed","items_processed":1}`},
	})
	c := h.mustConnect("bank")

	jobID := jobIDFrom(t, c.callTool("memory_retain",
		map[string]interface{}{"content": "flaky 2026"}).toolText(t))

	job := h.waitForJobStatus(t, jobID, queue.StatusCompleted)
	if job.RetryCount == 0 {
		t.Log("note: recovery happened inside doRequest's retries, not the queue's")
	}
	if got := h.mock.CallCount("/api/v1/remember"); got < 3 {
		t.Errorf("backend saw %d calls, expected at least 3 before success", got)
	}
}

// ─── Dead-letter path ────────────────────────────────────────────────────────

// TestDeep_DeadLetterFiresWebhook covers the OnDead callback wired in server.go
// — the operator's only signal that work was permanently lost.
func TestDeep_DeadLetterFiresWebhook(t *testing.T) {
	var mu sync.Mutex
	var received []map[string]interface{}

	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload map[string]interface{}
		json.NewDecoder(r.Body).Decode(&payload)
		mu.Lock()
		received = append(received, payload)
		mu.Unlock()
		w.WriteHeader(200)
	}))
	defer hook.Close()

	h := newHTTPHarness(t, func(c *Config) {
		c.ErrorWebhookURL = hook.URL
		c.QueueWorkerCount = 1
	})
	h.mock.SetResponse("/api/v1/remember", cogneemock.ResponseConfig{
		StatusCode: 500, Body: `{"error":"permanent"}`,
	})

	// Insert directly with no retries so the job dies on first failure.
	job := &queue.Job{
		ID: newJobID(), Bank: "bank", Type: "retain",
		Payload: "dead on arrival 2026", MaxRetries: 1,
	}
	if err := h.store.Insert(job); err != nil {
		t.Fatalf("insert: %v", err)
	}

	h.waitForJobStatus(t, job.ID, queue.StatusDead)

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(received)
		mu.Unlock()
		if n > 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(received) == 0 {
		t.Fatal("a dead job fired no webhook — permanent data loss would be silent")
	}
	body := fmt.Sprintf("%v", received[0])
	for _, want := range []string{job.ID, "bank"} {
		if !strings.Contains(body, want) {
			t.Errorf("webhook payload missing %q: %v", want, received[0])
		}
	}
}

// TestDeep_WebhookFailureDoesNotBlockTheWorker ensures a dead webhook endpoint
// cannot stall job processing.
func TestDeep_WebhookFailureDoesNotBlockTheWorker(t *testing.T) {
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	hook.Close() // unreachable

	h := newHTTPHarness(t, func(c *Config) {
		c.ErrorWebhookURL = hook.URL
		c.QueueWorkerCount = 1
	})
	h.mock.SetResponse("/api/v1/remember", cogneemock.ResponseConfig{StatusCode: 500, Body: `{}`})

	for i := 0; i < 3; i++ {
		job := &queue.Job{
			ID: newJobID(), Bank: "bank", Type: "retain",
			Payload: fmt.Sprintf("job %d 2026", i), MaxRetries: 1,
		}
		if err := h.store.Insert(job); err != nil {
			t.Fatalf("insert: %v", err)
		}
		h.waitForJobStatus(t, job.ID, queue.StatusDead)
	}
}

// ─── Crash recovery ──────────────────────────────────────────────────────────

// TestDeep_StartupRecoveryResetsOrphanedRunningJobs simulates the crash the
// queue was designed for: jobs left in "running" by a killed process must be
// requeued, not abandoned.
func TestDeep_StartupRecoveryResetsOrphanedRunningJobs(t *testing.T) {
	h := newHTTPHarness(t, func(c *Config) { c.QueueWorkerCount = 0 })

	orphan := &queue.Job{
		ID: newJobID(), Bank: "bank", Type: "retain",
		Payload: "interrupted 2026", MaxRetries: 3,
	}
	if err := h.store.Insert(orphan); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if err := h.store.UpdateStatus(orphan.ID, queue.StatusRunning, "", ""); err != nil {
		t.Fatalf("simulate in-flight: %v", err)
	}

	recovered, err := h.store.Recover()
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	if recovered < 1 {
		t.Fatalf("recovered %d jobs, want at least 1", recovered)
	}

	job, err := h.store.Get(orphan.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if job.Status != queue.StatusPending {
		t.Fatalf("orphaned running job is %q after recovery, want pending", job.Status)
	}
}

// TestDeep_RecoveryIsIdempotent guards against a second recovery pass
// corrupting state — restart loops call it repeatedly.
func TestDeep_RecoveryIsIdempotent(t *testing.T) {
	h := newHTTPHarness(t, func(c *Config) { c.QueueWorkerCount = 0 })

	job := &queue.Job{ID: newJobID(), Bank: "bank", Type: "retain", Payload: "x 2026", MaxRetries: 3}
	if err := h.store.Insert(job); err != nil {
		t.Fatalf("insert: %v", err)
	}
	h.store.UpdateStatus(job.ID, queue.StatusRunning, "", "")

	for i := 0; i < 3; i++ {
		if _, err := h.store.Recover(); err != nil {
			t.Fatalf("recover pass %d: %v", i, err)
		}
	}
	got, _ := h.store.Get(job.ID)
	if got.Status != queue.StatusPending {
		t.Fatalf("status = %q after 3 recovery passes, want pending", got.Status)
	}
}

// ─── Auto-reflect through the real path ─────────────────────────────────────

// TestDeep_AutoReflectFiresAfterNRetains drives auto-reflect through real tool
// calls rather than by invoking checkAutoReflect directly.
func TestDeep_AutoReflectFiresAfterNRetains(t *testing.T) {
	const threshold = 3
	h := newHTTPHarness(t, func(c *Config) {
		c.AutoReflectAfterN = threshold
		c.AutoReflectTimeout = 0 // count trigger only
	})
	c := h.mustConnect("bank")

	for i := 0; i < threshold; i++ {
		id := jobIDFrom(t, c.callTool("memory_retain",
			map[string]interface{}{"content": fmt.Sprintf("fact %d 2026", i)}).toolText(t))
		h.waitForJobStatus(t, id, queue.StatusCompleted)
	}

	// The auto-reflect job is enqueued and then processed, so it lands on the
	// backend's improve endpoint. Asserting there tests the observable effect
	// rather than an internal queue detail.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if h.mock.CallCount("/api/v1/improve") > 0 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("no auto-reflect reached the backend after %d retains", threshold)
}

// TestDeep_AutoReflectIsPerBank ensures one busy tenant cannot trigger reflects
// for a quiet one.
func TestDeep_AutoReflectIsPerBank(t *testing.T) {
	const threshold = 3
	h := newHTTPHarness(t, func(c *Config) {
		c.AutoReflectAfterN = threshold
		c.AutoReflectTimeout = 0
	})
	busy := h.mustConnect("tenant:busy")
	quiet := h.mustConnect("tenant:quiet")

	for i := 0; i < threshold; i++ {
		id := jobIDFrom(t, busy.callTool("memory_retain",
			map[string]interface{}{"content": fmt.Sprintf("busy %d 2026", i)}).toolText(t))
		h.waitForJobStatus(t, id, queue.StatusCompleted)
	}
	id := jobIDFrom(t, quiet.callTool("memory_retain",
		map[string]interface{}{"content": "quiet 1 2026"}).toolText(t))
	h.waitForJobStatus(t, id, queue.StatusCompleted)

	// Let any auto-reflect drain to the backend.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && h.mock.CallCount("/api/v1/improve") == 0 {
		time.Sleep(50 * time.Millisecond)
	}
	time.Sleep(300 * time.Millisecond)

	var sawBusy, sawQuiet bool
	for _, req := range h.mock.Requests() {
		if req.Path != "/api/v1/improve" {
			continue
		}
		if strings.Contains(req.Body, "tenant:busy") {
			sawBusy = true
		}
		if strings.Contains(req.Body, "tenant:quiet") {
			sawQuiet = true
		}
	}
	if !sawBusy {
		t.Fatal("busy bank crossed the threshold but no auto-reflect fired")
	}
	if sawQuiet {
		t.Fatal("quiet bank got an auto-reflect it did not earn")
	}
}

// TestDeep_ReflectStateEvictionRemovesIdleBanks covers the TTL cleanup added
// for the unbounded sync.Map — the fix existed but had no test.
func TestDeep_ReflectStateEvictionRemovesIdleBanks(t *testing.T) {
	h := newHTTPHarness(t, func(c *Config) { c.AutoReflectAfterN = 100 })

	for i := 0; i < 50; i++ {
		h.server.checkAutoReflect(fmt.Sprintf("bank%d", i))
	}
	if n := countReflectStates(h.server); n != 50 {
		t.Fatalf("expected 50 tracked banks, got %d", n)
	}

	// Nothing is older than the TTL yet.
	h.server.cleanupReflectStates(time.Hour)
	if n := countReflectStates(h.server); n != 50 {
		t.Fatalf("fresh entries were evicted: %d remain of 50", n)
	}

	// Everything is older than a zero TTL.
	h.server.cleanupReflectStates(0)
	if n := countReflectStates(h.server); n != 0 {
		t.Fatalf("stale entries survived eviction: %d remain", n)
	}
}

// TestDeep_ReflectStateEvictionIsRaceFree runs eviction against live traffic.
func TestDeep_ReflectStateEvictionIsRaceFree(t *testing.T) {
	h := newHTTPHarness(t, func(c *Config) { c.AutoReflectAfterN = 1000 })

	stop := make(chan struct{})
	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			h.server.checkAutoReflect(fmt.Sprintf("bank%d", i%20))
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			h.server.cleanupReflectStates(time.Nanosecond)
		}
	}()

	time.Sleep(300 * time.Millisecond)
	close(stop)
	wg.Wait()
}

// ─── Shutdown ────────────────────────────────────────────────────────────────

// TestDeep_WorkerStopDrainsWithoutLosingJobs verifies stopping the pool leaves
// no job stranded in "running".
func TestDeep_WorkerStopDrainsWithoutLosingJobs(t *testing.T) {
	h := newHTTPHarness(t, func(c *Config) {
		c.QueueWorkerCount = 2
		c.QueueMaxConcurrent = 2
	})
	h.mock.SetLatency("/api/v1/remember", 150*time.Millisecond)
	c := h.mustConnect("bank")

	var ids []string
	for i := 0; i < 8; i++ {
		ids = append(ids, jobIDFrom(t, c.callTool("memory_retain",
			map[string]interface{}{"content": fmt.Sprintf("drain %d 2026", i)}).toolText(t)))
	}

	time.Sleep(200 * time.Millisecond) // let some jobs start
	h.server.queueWorker.Stop()

	for _, id := range ids {
		job, err := h.store.Get(id)
		if err != nil {
			t.Fatalf("get %s: %v", id, err)
		}
		if job.Status == queue.StatusRunning {
			t.Errorf("job %s left running after Stop — a restart would orphan it "+
				"(recoverable, but Stop should not strand work)", id)
		}
	}
}

// TestDeep_StopIsIdempotent guards the double-close panic path.
func TestDeep_StopIsIdempotent(t *testing.T) {
	h := newHTTPHarness(t)
	h.server.queueWorker.Stop()
	h.server.queueWorker.Stop()
	h.server.queueWorker.Stop()
}

// ─── Session lifecycle ───────────────────────────────────────────────────────

// TestDeep_SessionCleanerEvictsIdleSessions exercises the cleaner loop, which
// had no coverage at all.
func TestDeep_SessionCleanerEvictsIdleSessions(t *testing.T) {
	h := newHTTPHarness(t, func(c *Config) {
		c.SessionCleanInterval = 20 * time.Millisecond
		c.SessionIdleTimeout = 50 * time.Millisecond
	})

	// Register a session that is already stale.
	stale := &MCPSession{
		SessionID:  "stale-session-id-0000",
		Bank:       "bank",
		SSEChannel: make(chan string, 1),
		CreatedAt:  time.Now().Add(-time.Hour),
		LastActive: time.Now().Add(-time.Hour),
	}
	h.server.sessionsMu.Lock()
	h.server.sessions[stale.SessionID] = stale
	h.server.sessionsMu.Unlock()

	go h.server.sessionCleaner()
	defer close(h.server.shutdown)

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		h.server.sessionsMu.RLock()
		_, present := h.server.sessions[stale.SessionID]
		h.server.sessionsMu.RUnlock()
		if !present {
			if !stale.IsClosed() {
				t.Error("evicted session was removed but not closed — SSE goroutine leaks")
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("idle session was never evicted")
}

// TestDeep_SessionCloseIsIdempotent covers the CompareAndSwap guard; a double
// close would panic on the channel.
func TestDeep_SessionCloseIsIdempotent(t *testing.T) {
	sess := &MCPSession{SSEChannel: make(chan string, 1)}
	sess.Close()
	sess.Close()
	sess.Close()
	if !sess.IsClosed() {
		t.Fatal("session should report closed")
	}
}

// ─── Metrics ─────────────────────────────────────────────────────────────────

// TestDeep_QueueGaugesTrackRealState checks the gauges operators rely on.
func TestDeep_QueueGaugesTrackRealState(t *testing.T) {
	h := newHTTPHarness(t, func(c *Config) { c.QueueWorkerCount = 0 })
	c := h.mustConnect("bank")

	if got := pendingCount(h.store); got != 0 {
		t.Fatalf("pendingCount = %d on a fresh queue", got)
	}
	for i := 0; i < 4; i++ {
		c.callTool("memory_retain", map[string]interface{}{"content": fmt.Sprintf("m%d 2026", i)})
	}
	if got := pendingCount(h.store); got != 4 {
		t.Errorf("pendingCount = %d, want 4", got)
	}
	if got := runningCount(h.store); got != 0 {
		t.Errorf("runningCount = %d with no workers, want 0", got)
	}
}

func TestDeep_QueueCountersHandleNilStore(t *testing.T) {
	if got := pendingCount(nil); got != 0 {
		t.Errorf("pendingCount(nil) = %d, want 0", got)
	}
	if got := runningCount(nil); got != 0 {
		t.Errorf("runningCount(nil) = %d, want 0", got)
	}
}

// ─── Concurrency stress over the real protocol ──────────────────────────────

// TestDeep_SustainedMixedLoad runs every tool concurrently against a backend
// that intermittently fails, and requires the server to stay coherent.
func TestDeep_SustainedMixedLoad(t *testing.T) {
	h := newHTTPHarness(t, func(c *Config) {
		c.QueueWorkerCount = 4
		c.QueueMaxConcurrent = 3
		c.AutoReflectAfterN = 5
	})
	// Every third recall fails.
	h.mock.SetSequence("/api/v1/recall", []cogneemock.ResponseConfig{
		{},
		{},
		{StatusCode: 500, Body: `{"e":"flaky"}`},
	})

	const agents = 8
	var wg sync.WaitGroup
	var errs atomic.Int64

	for i := 0; i < agents; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			c := h.mustConnect(fmt.Sprintf("tenant:load%d", i))
			for round := 0; round < 3; round++ {
				c.callTool("memory_retain",
					map[string]interface{}{"content": fmt.Sprintf("a%d r%d 2026", i, round)})
				if resp := c.callTool("memory_recall",
					map[string]interface{}{"query": "anything"}); resp.Error != nil {
					errs.Add(1)
				}
				c.callTool("memory_reflect", map[string]interface{}{"query": ""})
			}
		}(i)
	}
	wg.Wait()

	// The server must still answer and report a coherent queue.
	code, body := h.getJSON(t, "/debug/queue")
	if code != http.StatusOK {
		t.Fatalf("/debug/queue unhealthy after load: %d", code)
	}
	for _, f := range []string{"pending", "running", "completed_total"} {
		if _, ok := body[f]; !ok {
			t.Errorf("/debug/queue lost field %q under load", f)
		}
	}
	if h.server.panics.Load() != 0 {
		t.Errorf("server recorded %d panics under mixed load", h.server.panics.Load())
	}
}

// ─── helpers ─────────────────────────────────────────────────────────────────

func countReflectStates(s *Server) int {
	n := 0
	s.reflectStates.Range(func(_, _ interface{}) bool { n++; return true })
	return n
}
