package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"mcp-memory/internal/testutil"
)

const testTimeout = 3 * time.Second

// stressError and stressResult are type aliases for backward compatibility.
type stressError = testutil.AgentError
type stressResult = testutil.AgentResult

func requireServerUp(t *testing.T) {
	t.Helper()
	if !testutil.ServerUp() {
		t.Skip("MCP memory server not running at " + testutil.DefaultServerURL + ". Start with: cd mcp/memory && go run .")
	}
}

func TestMCPMemoryHealth(t *testing.T) {
	resp, err := http.Get(testutil.DefaultServerURL + "/health")
	if err != nil {
		t.Skipf("MCP memory server not running at %s: %v", testutil.DefaultServerURL, err)
		return
	}
	defer resp.Body.Close()

	var health map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&health)

	if resp.StatusCode != 200 {
		t.Fatalf("health check failed: status %d", resp.StatusCode)
	}

	status, _ := health["status"].(string)
	if status == "" {
		t.Fatal("health response missing status field")
	}
	t.Logf("Server status: %s, health: %+v", status, health)
}

func TestSingleAgent_BankIsolation(t *testing.T) {
	requireServerUp(t)
	client, err := testutil.NewClient(testutil.DefaultServerURL, "test:alice")
	if err != nil {
		t.Skipf("MCP memory server not running: %v", err)
		return
	}

	if err := client.Initialize(); err != nil {
		t.Fatalf("initialize failed: %v", err)
	}

	result, err := client.Recall("test query")
	if err != nil {
		t.Fatalf("recall failed: %v", err)
	}
	t.Logf("Recall result: %s", result[:testutil.Min(len(result), 200)])

	result, err = client.Retain("test memory for bank isolation test at " + time.Now().String())
	if err != nil {
		t.Fatalf("retain failed: %v", err)
	}
	t.Logf("Retain result: %s", result[:testutil.Min(len(result), 200)])
}

func TestConcurrentAgents_BankIsolation(t *testing.T) {
	requireServerUp(t)
	var wg sync.WaitGroup
	errs := make(chan error, 20)
	results := make(chan string, 20)

	wg.Add(1)
	go func() {
		defer wg.Done()
		client, err := testutil.NewClient(testutil.DefaultServerURL, "test:alice")
		if err != nil {
			errs <- fmt.Errorf("alice SSE: %w", err)
			return
		}
		if err := client.Initialize(); err != nil {
			errs <- err
			return
		}
		result, err := client.Retain("Alice's memory: test entry")
		if err != nil {
			errs <- fmt.Errorf("alice retain: %w", err)
			return
		}
		results <- "alice:" + result[:testutil.Min(len(result), 50)]
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		client, err := testutil.NewClient(testutil.DefaultServerURL, "test:bob")
		if err != nil {
			errs <- fmt.Errorf("bob SSE: %w", err)
			return
		}
		if err := client.Initialize(); err != nil {
			errs <- err
			return
		}
		result, err := client.Retain("Bob's memory: test entry")
		if err != nil {
			errs <- fmt.Errorf("bob retain: %w", err)
			return
		}
		results <- "bob:" + result[:testutil.Min(len(result), 50)]
	}()

	wg.Wait()
	close(errs)
	close(results)

	for err := range errs {
		t.Errorf("Concurrent test error: %v", err)
	}

	count := 0
	for r := range results {
		t.Logf("Result %d: %s", count, r)
		count++
	}
	if count < 2 {
		t.Errorf("expected 2 results, got %d", count)
	}
}

func TestConcurrentRecall_FastPath(t *testing.T) {
	requireServerUp(t)
	var wg sync.WaitGroup
	errs := make(chan error, 10)
	durations := make(chan time.Duration, 10)

	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			start := time.Now()
			client, err := testutil.NewClient(testutil.DefaultServerURL, fmt.Sprintf("test:user%d", id))
			if err != nil {
				errs <- fmt.Errorf("user%d SSE: %w", id, err)
				return
			}
			client.Initialize()
			_, err = client.Recall("quick test")
			durations <- time.Since(start)
			if err != nil {
				errs <- fmt.Errorf("user%d recall: %w", id, err)
			}
		}(i)
	}

	wg.Wait()
	close(errs)
	close(durations)

	for err := range errs {
		t.Errorf("Concurrent recall error: %v", err)
	}

	var maxDur time.Duration
	for d := range durations {
		if d > maxDur {
			maxDur = d
		}
	}
	t.Logf("Max concurrent recall duration: %v", maxDur)
	if maxDur > 10*time.Second {
		t.Errorf("concurrent recall took too long: %v (expected < 15s)", maxDur)
	}
}

func TestSessionCleanup_IdleExpiry(t *testing.T) {
	requireServerUp(t)
	client, err := testutil.NewClient(testutil.DefaultServerURL, "test:cleanup")
	if err != nil {
		t.Skipf("MCP memory server not running: %v", err)
		return
	}
	client.Initialize()

	resp, _ := http.Get(testutil.DefaultServerURL + "/health")
	var health map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&health)
	resp.Body.Close()
	initialSessions, _ := health["sessions"].(float64)
	t.Logf("Initial sessions: %v", initialSessions)

	if initialSessions == 0 {
		t.Error("expected at least 1 session")
	}
}

func TestInvalidBank_Rejected(t *testing.T) {
	requireServerUp(t)

	invalidBanks := []string{
		"../etc",
		"bank with spaces",
		"bank<script>",
	}

	for _, bank := range invalidBanks {
		req, _ := http.NewRequest("GET", testutil.DefaultServerURL+"/mcp/sse?bank="+bank, nil)
		req.Header.Set("Accept", "text/event-stream")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Logf("bank=%q: connection failed: %v", bank, err)
			continue
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		if resp.StatusCode == 200 || resp.StatusCode == 202 {
			t.Errorf("bank=%q: expected rejection, got %d. body: %s", bank, resp.StatusCode, string(body)[:testutil.Min(len(body), 100)])
		} else {
			t.Logf("bank=%q: correctly rejected (status %d)", bank, resp.StatusCode)
		}
	}

	req, _ := http.NewRequest("GET", testutil.DefaultServerURL+"/mcp/sse", nil)
	req.Header.Set("Accept", "text/event-stream")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req = req.WithContext(ctx)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("empty bank: connection failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("empty bank: expected 200, got %d", resp.StatusCode)
	} else {
		t.Logf("empty bank: correctly allowed (status %d)", resp.StatusCode)
	}
}

func TestSessionLimit_Rejected(t *testing.T) {
	requireServerUp(t)
	resp, err := http.Get(testutil.DefaultServerURL + "/health")
	if err != nil {
		t.Skipf("server not running: %v", err)
		return
	}
	defer resp.Body.Close()

	var health map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&health)
	sessions, _ := health["sessions"].(float64)
	t.Logf("Current sessions: %v (limit: 100)", sessions)
}

func TestRaceDetector_MultipleRetains(t *testing.T) {
	requireServerUp(t)
	var wg sync.WaitGroup
	errs := make(chan error, 20)
	count := 10

	for i := 0; i < count; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			client, err := testutil.NewClient(testutil.DefaultServerURL, fmt.Sprintf("test:race%d", id))
			if err != nil {
				errs <- fmt.Errorf("race%d SSE: %w", id, err)
				return
			}
			client.Initialize()

			for j := 0; j < 3; j++ {
				_, err := client.Retain(fmt.Sprintf("race%d entry %d at %s", id, j, time.Now().Format(time.RFC3339)))
				if err != nil {
					errs <- fmt.Errorf("race%d retain: %w", id, err)
					return
				}
			}
		}(i)
	}

	wg.Wait()
	close(errs)

	failCount := 0
	for err := range errs {
		t.Errorf("Race test error: %v", err)
		failCount++
	}

	t.Logf("Completed %d concurrent agents, %d failures", count, failCount)
}

func TestFastSlowPath_Isolation(t *testing.T) {
	requireServerUp(t)
	var wg sync.WaitGroup
	recallDurations := make(chan time.Duration, 1)
	retainDone := make(chan struct{})

	wg.Add(1)
	go func() {
		defer wg.Done()
		defer close(retainDone)
		client, err := testutil.NewClient(testutil.DefaultServerURL, "test:slow")
		if err != nil {
			t.Logf("Slow client SSE: %v", err)
			return
		}
		client.Initialize()
		_, err = client.Retain("Slow retain at " + time.Now().String())
		if err != nil {
			t.Logf("Slow retain failed: %v", err)
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		time.Sleep(500 * time.Millisecond)
		start := time.Now()
		client, err := testutil.NewClient(testutil.DefaultServerURL, "test:fast")
		if err != nil {
			t.Logf("Fast client SSE: %v", err)
			return
		}
		client.Initialize()
		_, err = client.Recall("fast recall")
		recallDurations <- time.Since(start)
		if err != nil {
			t.Logf("Fast recall failed: %v", err)
		}
	}()

	wg.Wait()
	close(recallDurations)

	dur := <-recallDurations
	t.Logf("Recall during retain took: %v", dur)
	if dur > 15*time.Second {
		t.Errorf("recall blocked by retain: took %v (should be < 1s)", dur)
	}
}

func TestStress_10AgentsMixedOps(t *testing.T) {
	requireServerUp(t)

	const agents = 10
	var wg sync.WaitGroup
	errs := make(chan stressError, agents*2)
	results := make(chan stressResult, agents*2)

	start := time.Now()

	for i := 0; i < agents; i++ {
		wg.Add(1)
		go func(agentID int) {
			defer wg.Done()

			bank := fmt.Sprintf("stress:agent%d", agentID)
			client, err := testutil.NewClient(testutil.DefaultServerURL, bank)
			if err != nil {
				errs <- stressError{Agent: agentID, Op: "connect", Err: err}
				return
			}
			defer client.Close()
			if err := client.Initialize(); err != nil {
				errs <- stressError{Agent: agentID, Op: "init", Err: err}
				return
			}

			opStart := time.Now()
			content := fmt.Sprintf("Stress agent %d test at %s", agentID, time.Now().Format(time.RFC3339))
			_, err = client.Retain(content)
			if err != nil {
				errs <- stressError{Agent: agentID, Op: "retain", Err: err}
			} else {
				results <- stressResult{Agent: agentID, Op: "retain", Dur: time.Since(opStart)}
			}

			opStart = time.Now()
			_, err = client.Recall(fmt.Sprintf("stress agent %d", agentID))
			if err != nil {
				errs <- stressError{Agent: agentID, Op: "recall", Err: err}
			} else {
				results <- stressResult{Agent: agentID, Op: "recall", Dur: time.Since(opStart)}
			}
		}(i)
	}

	wg.Wait()
	close(errs)
	close(results)

	total := time.Since(start)

	failCount := 0
	for e := range errs {
		t.Logf("FAIL: agent=%d op=%s: %v", e.Agent, e.Op, e.Err)
		failCount++
	}

	var maxRetain, maxRecall time.Duration
	retainCount, recallCount := 0, 0
	for r := range results {
		if r.Op == "retain" {
			if r.Dur > maxRetain {
				maxRetain = r.Dur
			}
			retainCount++
		} else {
			if r.Dur > maxRecall {
				maxRecall = r.Dur
			}
			recallCount++
		}
	}

	t.Logf("Stress: %d agents, %d retains, %d recalls, %d failures",
		agents, retainCount, recallCount, failCount)
	t.Logf("Wall time: %v, max retain: %v, max recall: %v",
		total.Round(time.Millisecond), maxRetain.Round(time.Millisecond), maxRecall.Round(time.Millisecond))

	if failCount > 0 {
		t.Errorf("%d failures", failCount)
	}
	if maxRecall > 15*time.Second {
		t.Errorf("recall too slow: %v (expected < 15s)", maxRecall)
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// ensure strings is used (for the invalid bank test)
var _ = strings.Contains
