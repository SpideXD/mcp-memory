package main

import (
	"bufio"
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

// ─── Helpers ────────────────────────────────────────────────────────────────

func mustConnect(t *testing.T, bank string) *testutil.Client {
	t.Helper()
	c, err := testutil.NewClient(testutil.DefaultServerURL, bank)
	if err != nil {
		t.Fatalf("connect bank=%q: %v", bank, err)
	}
	if err := c.Initialize(); err != nil {
		t.Fatalf("initialize bank=%q: %v", bank, err)
	}
	return c
}

func rawMCPRequest(method string, params interface{}) string {
	body := map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  method,
	}
	if params != nil {
		body["params"] = params
	}
	data, _ := json.Marshal(body)
	return string(data)
}

// postMessage sends a raw JSON-RPC message and returns status + body.
func postMessage(sessionID, body string) (int, string) {
	url := fmt.Sprintf("%s/mcp/message?session_id=%s", testutil.DefaultServerURL, sessionID)
	resp, err := http.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		return 0, err.Error()
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// connectSSERaw opens an SSE connection and returns the response (unclosed).
func connectSSERaw(bank string) (*http.Response, error) {
	url := testutil.DefaultServerURL + "/mcp/sse"
	if bank != "" {
		url += "?bank=" + bank
	}
	req, _ := http.NewRequest("GET", url, nil)
	req.Header.Set("Accept", "text/event-stream")
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	return resp, nil
}

// ─── 1. MCP Protocol Compliance ────────────────────────────────────────────

func TestProtocol_Initialize(t *testing.T) {
	requireServerUp(t)
	c := mustConnect(t, "proto:init")
	defer c.Close()

	result, err := c.CallJSONRPC("initialize", nil)
	if err != nil {
		t.Fatalf("initialize failed: %v", err)
	}

	var resp map[string]interface{}
	json.Unmarshal([]byte(result), &resp)

	if resp["protocolVersion"] != "2024-11-05" {
		t.Errorf("expected protocolVersion 2024-11-05, got %v", resp["protocolVersion"])
	}
	caps, ok := resp["capabilities"].(map[string]interface{})
	if !ok {
		t.Fatal("missing capabilities")
	}
	if _, ok := caps["tools"]; !ok {
		t.Error("missing tools capability")
	}
	info, ok := resp["serverInfo"].(map[string]interface{})
	if !ok {
		t.Fatal("missing serverInfo")
	}
	if info["name"] != "mcp-memory" {
		t.Errorf("expected serverInfo.name=mcp-memory, got %v", info["name"])
	}
	if info["version"] != "2.0.0" {
		t.Errorf("expected serverInfo.version=2.0.0, got %v", info["version"])
	}
	t.Logf("Protocol: %+v", resp)
}

func TestProtocol_Ping(t *testing.T) {
	requireServerUp(t)
	c := mustConnect(t, "proto:ping")
	defer c.Close()

	result, err := c.CallJSONRPC("ping", nil)
	if err != nil {
		t.Fatalf("ping failed: %v", err)
	}
	if result != "{}" && result != "null" {
		t.Errorf("ping expected empty result, got: %s", result)
	}
}

func TestProtocol_ToolsList(t *testing.T) {
	requireServerUp(t)
	c := mustConnect(t, "proto:tools")
	defer c.Close()

	result, err := c.CallJSONRPC("tools/list", nil)
	if err != nil {
		t.Fatalf("tools/list failed: %v", err)
	}

	var resp struct {
		Tools []struct {
			Name        string      `json:"name"`
			Description string      `json:"description"`
			Schema      interface{} `json:"inputSchema"`
		} `json:"tools"`
	}
	json.Unmarshal([]byte(result), &resp)

	if len(resp.Tools) != 3 {
		t.Fatalf("expected 3 tools, got %d", len(resp.Tools))
	}

	expected := map[string]bool{"memory_retain": false, "memory_recall": false, "memory_reflect": false}
	for _, tool := range resp.Tools {
		if _, ok := expected[tool.Name]; !ok {
			t.Errorf("unexpected tool: %s", tool.Name)
		}
		expected[tool.Name] = true
		t.Logf("Tool: %s — %s", tool.Name, tool.Description)
	}
	for name, found := range expected {
		if !found {
			t.Errorf("missing tool: %s", name)
		}
	}
}

func TestProtocol_UnknownMethod(t *testing.T) {
	requireServerUp(t)
	c := mustConnect(t, "proto:unknown")
	defer c.Close()

	_, err := c.CallJSONRPC("nonexistent/method", nil)
	if err != nil {
		t.Logf("unknown method returned error (expected): %v", err)
	}
}

func TestProtocol_DoubleInitialize(t *testing.T) {
	requireServerUp(t)
	c := mustConnect(t, "proto:double")
	defer c.Close()

	result, err := c.CallJSONRPC("initialize", nil)
	if err != nil {
		t.Fatalf("double initialize failed: %v", err)
	}
	t.Logf("Double initialize result: %s", result[:testutil.Min(len(result), 100)])
}

// ─── 2. Tool Validation ────────────────────────────────────────────────────

func TestTool_UnknownTool(t *testing.T) {
	requireServerUp(t)
	c := mustConnect(t, "tool:unknown")
	defer c.Close()

	params := map[string]interface{}{
		"name":      "nonexistent_tool",
		"arguments": map[string]interface{}{},
	}
	result, err := c.CallJSONRPC("tools/call", params)
	if err != nil {
		t.Logf("Unknown tool error: %v", err)
	} else {
		t.Logf("Unknown tool result: %q", result)
	}
}

func TestTool_Recall_MissingQuery(t *testing.T) {
	requireServerUp(t)
	c := mustConnect(t, "tool:missing")
	defer c.Close()

	params := map[string]interface{}{
		"name":      "memory_recall",
		"arguments": map[string]interface{}{},
	}
	result, err := c.CallJSONRPC("tools/call", params)
	if err != nil {
		t.Logf("Missing query error: %v", err)
	} else {
		t.Logf("Missing query accepted: %s", result[:testutil.Min(len(result), 200)])
	}
}

func TestTool_Retain_MissingContent(t *testing.T) {
	requireServerUp(t)
	c := mustConnect(t, "tool:retmiss")
	defer c.Close()

	params := map[string]interface{}{
		"name":      "memory_retain",
		"arguments": map[string]interface{}{},
	}
	result, err := c.CallJSONRPC("tools/call", params)
	if err != nil {
		t.Logf("Missing content error: %v", err)
	} else {
		t.Logf("Missing content accepted: %s", result[:testutil.Min(len(result), 200)])
	}
}

func TestTool_Reflect_MissingQuery(t *testing.T) {
	requireServerUp(t)
	c := mustConnect(t, "tool:refmiss")
	defer c.Close()

	params := map[string]interface{}{
		"name":      "memory_reflect",
		"arguments": map[string]interface{}{},
	}
	result, err := c.CallJSONRPC("tools/call", params)
	if err != nil {
		t.Logf("Missing query error: %v", err)
	} else {
		t.Logf("Missing query accepted: %s", result[:testutil.Min(len(result), 200)])
	}
}

func TestTool_Recall_EmptyQuery(t *testing.T) {
	requireServerUp(t)
	c := mustConnect(t, "tool:emptyq")
	defer c.Close()

	result, err := c.Recall("")
	if err != nil {
		t.Logf("empty query: %v", err)
	} else {
		t.Logf("empty query result: %s", result[:testutil.Min(len(result), 200)])
	}
}

func TestTool_Retain_EmptyContent(t *testing.T) {
	requireServerUp(t)
	c := mustConnect(t, "tool:emptyc")
	defer c.Close()

	result, err := c.Retain("")
	if err != nil {
		t.Logf("empty content: %v", err)
	} else {
		t.Logf("empty content result: %s", result[:testutil.Min(len(result), 200)])
	}
}

func TestTool_WrongArgumentTypes(t *testing.T) {
	requireServerUp(t)
	c := mustConnect(t, "tool:wrongtype")
	defer c.Close()

	params := map[string]interface{}{
		"name":      "memory_recall",
		"arguments": map[string]interface{}{"query": 12345},
	}
	result, err := c.CallJSONRPC("tools/call", params)
	if err != nil {
		t.Logf("Wrong type error (expected): %v", err)
	} else if strings.Contains(result, "error") {
		t.Logf("Wrong type error in result (expected): %s", result[:testutil.Min(len(result), 200)])
	} else {
		t.Logf("Wrong type result: %s", result[:testutil.Min(len(result), 200)])
	}
}

// ─── 3. Bank Edge Cases ────────────────────────────────────────────────────

func TestBank_ValidFormats(t *testing.T) {
	requireServerUp(t)

	validBanks := []string{
		"simple",
		"with:slash",
		"with-dash",
		"with_underscore",
		"CamelCase",
		"123numeric",
		"a:b:c:deep",
		"outreach:spidex_owner",
		"profile:user-id_123",
	}

	for _, bank := range validBanks {
		c, err := testutil.NewClient(testutil.DefaultServerURL, bank)
		if err != nil {
			t.Errorf("bank=%q: connect failed: %v", bank, err)
			continue
		}
		if err := c.Initialize(); err != nil {
			t.Errorf("bank=%q: init failed: %v", bank, err)
			c.Close()
			continue
		}
		t.Logf("bank=%q: OK", bank)
		c.Close()
	}
}

func TestBank_InvalidFormats(t *testing.T) {
	requireServerUp(t)

	invalidBanks := []string{
		"../traversal",
		"../../etc/passwd",
		"with spaces",
		"with@at",
		"with?question",
		"<script>alert(1)</script>",
	}

	for _, bank := range invalidBanks {
		resp, err := connectSSERaw(bank)
		if err != nil {
			t.Logf("bank=%q: connection error (ok): %v", bank, err)
			continue
		}
		if resp == nil {
			t.Logf("bank=%q: nil response (connection rejected)", bank)
			continue
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		if resp.StatusCode == 200 {
			t.Errorf("bank=%q: expected rejection, got 200", bank)
		} else {
			t.Logf("bank=%q: correctly rejected (status %d): %s", bank, resp.StatusCode, string(body)[:testutil.Min(len(body), 80)])
		}
	}

	resp, err := connectSSERaw("with#hash")
	if err == nil && resp != nil {
		resp.Body.Close()
		t.Logf("bank='with#hash': treated as 'with' (URL fragment), status %d", resp.StatusCode)
	}
}

func TestBank_VeryLongName(t *testing.T) {
	requireServerUp(t)

	longBank := strings.Repeat("a", 500)
	resp, err := connectSSERaw(longBank)
	if err != nil {
		t.Logf("long bank: connection error: %v", err)
		return
	}
	if resp == nil {
		t.Logf("long bank: nil response")
		return
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	t.Logf("long bank (500 chars): status %d, body: %s", resp.StatusCode, string(body)[:testutil.Min(len(body), 80)])
}

func TestBank_URLDecoding(t *testing.T) {
	requireServerUp(t)

	encoded := "outreach%3Aspidex_owner"
	c, err := testutil.NewClient(testutil.DefaultServerURL, encoded)
	if err != nil {
		t.Fatalf("URL-encoded bank connect failed: %v", err)
	}
	defer c.Close()
	if err := c.Initialize(); err != nil {
		t.Fatalf("URL-encoded bank init failed: %v", err)
	}
	t.Logf("URL-encoded bank %q: OK", encoded)
}

func TestBank_Isolation(t *testing.T) {
	requireServerUp(t)

	bankA := "iso:" + fmt.Sprintf("%d", time.Now().UnixNano())
	bankB := "iso:" + fmt.Sprintf("%d", time.Now().UnixNano()+1)

	ca := mustConnect(t, bankA)
	defer ca.Close()
	cb := mustConnect(t, bankB)
	defer cb.Close()

	uniqueContent := fmt.Sprintf("isolation test %d", time.Now().UnixNano())
	_, err := ca.Retain(uniqueContent)
	if err != nil {
		t.Fatalf("retain in A failed: %v", err)
	}

	result, err := cb.Recall(uniqueContent)
	if err != nil {
		t.Fatalf("recall from B failed: %v", err)
	}
	if strings.Contains(result, uniqueContent) {
		t.Errorf("BANK ISOLATION BREACH: bank B found bank A's content!\nA=%q\nB=%q\nResult=%s", bankA, bankB, result)
	} else {
		t.Logf("Bank isolation OK: B did not find A's content")
	}
}

// ─── 4. Session Lifecycle ──────────────────────────────────────────────────

func TestSession_SSEEndpointEvent(t *testing.T) {
	requireServerUp(t)

	resp, err := connectSSERaw("sess:endpoint")
	if err != nil {
		t.Fatalf("SSE connect failed: %v", err)
	}
	defer resp.Body.Close()

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 1<<20), 1<<20)

	deadline := time.After(3 * time.Second)
	for {
		select {
		case <-deadline:
			t.Fatal("timeout waiting for endpoint event")
		default:
		}
		if !scanner.Scan() {
			t.Fatal("SSE stream ended before endpoint event")
		}
		line := scanner.Text()
		if strings.HasPrefix(line, "event: endpoint") {
			if !scanner.Scan() {
				t.Fatal("no data line after endpoint event")
			}
			data := scanner.Text()
			if !strings.HasPrefix(data, "data: /mcp/message?session_id=") {
				t.Errorf("unexpected endpoint data: %s", data)
			}
			sessionID := strings.TrimPrefix(data, "data: /mcp/message?session_id=")
			if len(sessionID) < 10 {
				t.Errorf("session_id too short: %q", sessionID)
			}
			t.Logf("Session endpoint: %s", sessionID)
			return
		}
	}
}

func TestSession_InvalidSessionID(t *testing.T) {
	requireServerUp(t)

	body := rawMCPRequest("tools/list", nil)
	status, resp := postMessage("fake-session-id-12345", body)
	if status != 400 {
		t.Errorf("expected 400 for invalid session, got %d: %s", status, resp)
	} else {
		t.Logf("Invalid session correctly rejected: %d", status)
	}
}

func TestSession_EmptySessionID(t *testing.T) {
	requireServerUp(t)

	body := rawMCPRequest("tools/list", nil)
	status, resp := postMessage("", body)
	if status != 400 {
		t.Errorf("expected 400 for empty session, got %d: %s", status, resp)
	} else {
		t.Logf("Empty session correctly rejected: %d", status)
	}
}

func TestSession_WrongMethod(t *testing.T) {
	requireServerUp(t)

	c := mustConnect(t, "sess:method")
	defer c.Close()

	url := fmt.Sprintf("%s/mcp/message?session_id=%s", testutil.DefaultServerURL, c.SessionID())
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET request failed: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != 405 {
		t.Errorf("expected 405 for GET, got %d", resp.StatusCode)
	} else {
		t.Logf("GET correctly rejected with 405")
	}
}

func TestSession_MalformedJSON(t *testing.T) {
	requireServerUp(t)

	c := mustConnect(t, "sess:malformed")
	defer c.Close()

	status, _ := postMessage(c.SessionID(), "this is not json")
	if status == 202 || status == 200 {
		t.Logf("Malformed JSON: accepted (async processing)")
	} else {
		t.Logf("Malformed JSON: rejected with status %d", status)
	}
}

func TestSession_OversizedBody(t *testing.T) {
	requireServerUp(t)

	c := mustConnect(t, "sess:oversize")
	defer c.Close()

	result, err := c.Retain(strings.Repeat("large content ", 10000))
	if err != nil {
		t.Logf("Large content: %v", err)
	} else {
		t.Logf("Large content result: %s", result[:testutil.Min(len(result), 100)])
	}
}

// ─── 5. Concurrent Access ──────────────────────────────────────────────────

func TestConcurrent_SameBank(t *testing.T) {
	requireServerUp(t)

	bank := fmt.Sprintf("conc:same-%d", time.Now().UnixNano())
	const agents = 5
	var wg sync.WaitGroup
	errs := make(chan error, agents*3)

	for i := 0; i < agents; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			c := mustConnect(t, bank)
			defer c.Close()

			_, err := c.Retain(fmt.Sprintf("agent %d entry at %s", id, time.Now().Format(time.RFC3339Nano)))
			if err != nil {
				errs <- fmt.Errorf("agent%d retain: %w", id, err)
			}

			_, err = c.Recall("agent entry")
			if err != nil {
				errs <- fmt.Errorf("agent%d recall: %w", id, err)
			}
		}(i)
	}

	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("concurrent same bank: %v", err)
	}
}

func TestConcurrent_DifferentBanks(t *testing.T) {
	requireServerUp(t)

	const agents = 10
	var wg sync.WaitGroup
	errs := make(chan error, agents*2)
	durations := make(chan time.Duration, agents)

	for i := 0; i < agents; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			bank := fmt.Sprintf("conc:diff-%d-%d", id, time.Now().UnixNano())
			start := time.Now()

			c := mustConnect(t, bank)
			defer c.Close()

			_, err := c.Retain(fmt.Sprintf("unique content for agent %d", id))
			if err != nil {
				errs <- fmt.Errorf("agent%d retain: %w", id, err)
				return
			}

			result, err := c.Recall(fmt.Sprintf("unique content for agent %d", id))
			if err != nil {
				errs <- fmt.Errorf("agent%d recall: %w", id, err)
				return
			}

			if !strings.Contains(result, fmt.Sprintf("agent %d", id)) {
				t.Logf("agent%d: recall didn't contain expected text", id)
			}

			durations <- time.Since(start)
		}(i)
	}

	wg.Wait()
	close(errs)
	close(durations)

	for err := range errs {
		t.Errorf("concurrent different banks: %v", err)
	}

	var maxDur time.Duration
	for d := range durations {
		if d > maxDur {
			maxDur = d
		}
	}
	t.Logf("Max agent duration: %v", maxDur)
}

func TestConcurrent_RecallDoesNotBlockOnRetain(t *testing.T) {
	requireServerUp(t)

	var wg sync.WaitGroup
	retainStarted := make(chan struct{})
	recallDone := make(chan time.Duration, 1)

	wg.Add(1)
	go func() {
		defer wg.Done()
		c := mustConnect(t, "conc:block")
		defer c.Close()
		close(retainStarted)
		c.Retain("slow retain operation")
	}()

	<-retainStarted
	time.Sleep(100 * time.Millisecond)

	wg.Add(1)
	go func() {
		defer wg.Done()
		c := mustConnect(t, "conc:fast")
		defer c.Close()
		start := time.Now()
		c.Recall("fast recall")
		recallDone <- time.Since(start)
	}()

	wg.Wait()
	close(recallDone)

	dur := <-recallDone
	t.Logf("Recall during retain: %v", dur)
	if dur > 30*time.Second {
		t.Errorf("recall blocked by retain: %v (should be < 30s)", dur)
	}
}

// ─── 6. Worker Pool Saturation ─────────────────────────────────────────────

func TestWorkerPool_QueueFull(t *testing.T) {
	requireServerUp(t)

	const count = 20
	var wg sync.WaitGroup
	errs := make(chan error, count)
	results := make(chan string, count)

	for i := 0; i < count; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			c := mustConnect(t, fmt.Sprintf("pool:%d-%d", id, time.Now().UnixNano()))
			defer c.Close()

			_, err := c.Retain(fmt.Sprintf("pool saturation test %d", id))
			if err != nil {
				errs <- fmt.Errorf("retain %d: %w", id, err)
			} else {
				results <- fmt.Sprintf("retain %d: ok", id)
			}
		}(i)
	}

	wg.Wait()
	close(errs)
	close(results)

	failCount := 0
	for err := range errs {
		t.Logf("Queue saturation error: %v", err)
		failCount++
	}
	successCount := 0
	for range results {
		successCount++
	}
	t.Logf("Queue saturation: %d/%d succeeded, %d failed", successCount, count, failCount)
}

// ─── 7. Data Integrity ─────────────────────────────────────────────────────

func TestIntegrity_RetainThenRecall(t *testing.T) {
	requireServerUp(t)

	bank := fmt.Sprintf("integ:%d", time.Now().UnixNano())
	c := mustConnect(t, bank)
	defer c.Close()

	unique := fmt.Sprintf("integrity test at %s with value %d", time.Now().Format(time.RFC3339), time.Now().UnixNano())
	_, err := c.Retain(unique)
	if err != nil {
		t.Fatalf("retain failed: %v", err)
	}

	time.Sleep(1 * time.Second)

	result, err := c.Recall("integrity test value")
	if err != nil {
		t.Fatalf("recall failed: %v", err)
	}
	t.Logf("Recall after retain: %s", result[:testutil.Min(len(result), 300)])
}

func TestIntegrity_MultipleRetainsSameBank(t *testing.T) {
	requireServerUp(t)

	bank := fmt.Sprintf("integ:multi-%d", time.Now().UnixNano())
	c := mustConnect(t, bank)
	defer c.Close()

	items := []string{
		"The quick brown fox jumps over the lazy dog",
		"Lorem ipsum dolor sit amet, consectetur adipiscing elit",
		"The answer to life, the universe, and everything is 42",
	}

	for i, item := range items {
		_, err := c.Retain(item)
		if err != nil {
			t.Fatalf("retain %d failed: %v", i, err)
		}
		t.Logf("Retained item %d", i)
	}

	time.Sleep(1 * time.Second)
	result, err := c.Recall("answer life universe everything")
	if err != nil {
		t.Fatalf("recall failed: %v", err)
	}
	t.Logf("Recall for 'answer life universe everything': %s", result[:testutil.Min(len(result), 300)])
}

func TestIntegrity_ReflectAfterRetain(t *testing.T) {
	requireServerUp(t)

	bank := fmt.Sprintf("integ:reflect-%d", time.Now().UnixNano())
	c := mustConnect(t, bank)
	defer c.Close()

	_, err := c.Retain("Go is a statically typed, compiled programming language designed at Google")
	if err != nil {
		t.Fatalf("retain 1 failed: %v", err)
	}
	_, err = c.Retain("Go is known for its simplicity, concurrency support with goroutines, and fast compilation")
	if err != nil {
		t.Fatalf("retain 2 failed: %v", err)
	}

	result, err := c.Reflect("What do you know about Go programming language?")
	if err != nil {
		t.Fatalf("reflect failed: %v", err)
	}
	t.Logf("Reflect result: %s", result[:testutil.Min(len(result), 500)])
}

// ─── 8. SSE Edge Cases ─────────────────────────────────────────────────────

func TestSSE_CORS(t *testing.T) {
	requireServerUp(t)

	resp, err := connectSSERaw("sse:cors")
	if err != nil {
		t.Fatalf("connect failed: %v", err)
	}
	resp.Body.Close()

	cors := resp.Header.Get("Access-Control-Allow-Origin")
	if cors != "*" {
		t.Errorf("expected CORS *, got %q", cors)
	} else {
		t.Logf("CORS header correct: %s", cors)
	}
}

func TestSSE_ContentType(t *testing.T) {
	requireServerUp(t)

	resp, err := connectSSERaw("sse:ct")
	if err != nil {
		t.Fatalf("connect failed: %v", err)
	}
	resp.Body.Close()

	ct := resp.Header.Get("Content-Type")
	if !strings.Contains(ct, "text/event-stream") {
		t.Errorf("expected text/event-stream, got %q", ct)
	} else {
		t.Logf("Content-Type correct: %s", ct)
	}
}

func TestSSE_ConnectionDrop(t *testing.T) {
	requireServerUp(t)

	c, err := testutil.NewClient(testutil.DefaultServerURL, "sse:drop")
	if err != nil {
		t.Fatalf("connect failed: %v", err)
	}
	c.Initialize()
	c.Close()

	time.Sleep(1 * time.Second)

	resp, err := http.Get(testutil.DefaultServerURL + "/health")
	if err != nil {
		t.Fatalf("health check failed after drop: %v", err)
	}
	defer resp.Body.Close()

	var health map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&health)
	status, _ := health["status"].(string)
	if status != "running" {
		t.Errorf("server not running after connection drop: %q", status)
	}
	t.Logf("Server healthy after connection drop")
}

func TestSSE_MultipleConnectionsSameBank(t *testing.T) {
	requireServerUp(t)

	bank := fmt.Sprintf("sse:multi-%d", time.Now().UnixNano())
	const conns = 5
	clients := make([]*testutil.Client, conns)

	for i := 0; i < conns; i++ {
		c := mustConnect(t, bank)
		clients[i] = c
	}
	defer func() {
		for _, c := range clients {
			c.Close()
		}
	}()

	for i, c := range clients {
		_, err := c.Recall(fmt.Sprintf("test %d", i))
		if err != nil {
			t.Errorf("client %d recall failed: %v", i, err)
		}
	}
	t.Logf("All %d connections on same bank work", conns)
}

// ─── 9. HTTP Edge Cases ────────────────────────────────────────────────────

func TestHTTP_InvalidMethod(t *testing.T) {
	requireServerUp(t)

	req, _ := http.NewRequest("DELETE", testutil.DefaultServerURL+"/health", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("DELETE health: %v", err)
	}
	resp.Body.Close()
	t.Logf("DELETE /health: %d", resp.StatusCode)

	req, _ = http.NewRequest("PUT", testutil.DefaultServerURL+"/mcp/sse", nil)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PUT mcp/sse: %v", err)
	}
	resp.Body.Close()
	t.Logf("PUT /mcp/sse: %d", resp.StatusCode)
}

func TestHTTP_HealthContainsAllFields(t *testing.T) {
	requireServerUp(t)

	resp, err := http.Get(testutil.DefaultServerURL + "/health")
	if err != nil {
		t.Fatalf("health: %v", err)
	}
	defer resp.Body.Close()

	var health map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&health)

	required := []string{"status", "hindsight", "llama", "reranker", "queue_depth",
		"retain_workers", "reflect_workers", "retain_panics", "reflect_panics",
		"sessions", "uptime", "panics_total", "metrics"}

	for _, field := range required {
		if _, ok := health[field]; !ok {
			t.Errorf("health missing field: %s", field)
		}
	}
	t.Logf("Health has all %d required fields", len(required))
}

// ─── 10. Start/Stop Endpoints ──────────────────────────────────────────────

func TestStartStop_AlreadyRunning(t *testing.T) {
	requireServerUp(t)

	resp, err := http.Post(testutil.DefaultServerURL+"/start", "", nil)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	t.Logf("POST /start (already running): %d %s", resp.StatusCode, string(body))
}

func TestStopEndpoint_DontActuallyStop(t *testing.T) {
	requireServerUp(t)
	t.Skip("skipping /stop to keep server alive for other tests")
}

// ─── 11. Stress: Rapid Connect/Disconnect ──────────────────────────────────

func TestStress_RapidConnectDisconnect(t *testing.T) {
	requireServerUp(t)

	const count = 20
	var wg sync.WaitGroup
	errs := make(chan error, count)

	for i := 0; i < count; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			c, err := testutil.NewClient(testutil.DefaultServerURL, fmt.Sprintf("stress:rapid-%d", id))
			if err != nil {
				errs <- fmt.Errorf("connect %d: %w", id, err)
				return
			}
			c.Initialize()
			c.Close()
		}(i)
	}

	wg.Wait()
	close(errs)

	failCount := 0
	for err := range errs {
		t.Errorf("rapid connect/disconnect: %v", err)
		failCount++
	}
	t.Logf("Rapid connect/disconnect: %d/%d succeeded", count-failCount, count)
}

// ─── 12. Metrics Verification ──────────────────────────────────────────────

func TestMetrics_IncrementOnCalls(t *testing.T) {
	requireServerUp(t)

	resp, _ := http.Get(testutil.DefaultServerURL + "/health")
	var before map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&before)
	resp.Body.Close()

	beforeMetrics, _ := before["metrics"].(map[string]interface{})
	beforeRecall, _ := beforeMetrics["memory.recall_count"].(float64)
	beforeRetain, _ := beforeMetrics["memory.retain_count"].(float64)

	c := mustConnect(t, "metrics:test")
	defer c.Close()

	c.Recall("metrics test")
	c.Retain("metrics test content")

	resp, _ = http.Get(testutil.DefaultServerURL + "/health")
	var after map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&after)
	resp.Body.Close()

	afterMetrics, _ := after["metrics"].(map[string]interface{})
	afterRecall, _ := afterMetrics["memory.recall_count"].(float64)
	afterRetain, _ := afterMetrics["memory.retain_count"].(float64)

	if afterRecall <= beforeRecall {
		t.Errorf("recall count didn't increment: before=%.0f after=%.0f", beforeRecall, afterRecall)
	}
	if afterRetain <= beforeRetain {
		t.Errorf("retain count didn't increment: before=%.0f after=%.0f", beforeRetain, afterRetain)
	}
	t.Logf("Metrics: recall %.0f->%.0f, retain %.0f->%.0f", beforeRecall, afterRecall, beforeRetain, afterRetain)
}

// ─── 13. JSON-RPC Compliance ───────────────────────────────────────────────

func TestJSONRPC_VersionField(t *testing.T) {
	requireServerUp(t)

	c := mustConnect(t, "rpc:version")
	defer c.Close()

	body := `{"id":1,"method":"ping"}`
	status, _ := postMessage(c.SessionID(), body)
	t.Logf("Missing jsonrpc field: status %d", status)
}

func TestJSONRPC_NegativeID(t *testing.T) {
	requireServerUp(t)

	c := mustConnect(t, "rpc:negid")
	defer c.Close()

	body := `{"jsonrpc":"2.0","id":-1,"method":"ping"}`
	status, _ := postMessage(c.SessionID(), body)
	t.Logf("Negative ID: status %d", status)
}

func TestJSONRPC_StringID(t *testing.T) {
	requireServerUp(t)

	c := mustConnect(t, "rpc:strid")
	defer c.Close()

	body := `{"jsonrpc":"2.0","id":"abc-123","method":"ping"}`
	status, _ := postMessage(c.SessionID(), body)
	t.Logf("String ID: status %d", status)
}

func TestJSONRPC_NullID(t *testing.T) {
	requireServerUp(t)

	c := mustConnect(t, "rpc:nullid")
	defer c.Close()

	body := `{"jsonrpc":"2.0","id":null,"method":"ping"}`
	status, _ := postMessage(c.SessionID(), body)
	t.Logf("Null ID: status %d", status)
}

// ─── 14. Notifications (no response expected) ─────────────────────────────

func TestNotification_Initialized(t *testing.T) {
	requireServerUp(t)

	c := mustConnect(t, "notif:init")
	defer c.Close()

	body := `{"jsonrpc":"2.0","method":"notifications/initialized"}`
	status, _ := postMessage(c.SessionID(), body)
	t.Logf("notifications/initialized: status %d", status)
}

// ─── 15. Edge Case: Very Long Content ──────────────────────────────────────

func TestEdgeCase_VeryLongRetainContent(t *testing.T) {
	requireServerUp(t)

	c := mustConnect(t, "edge:long")
	defer c.Close()

	longContent := strings.Repeat("This is a test sentence for long content retention. ", 2000)
	result, err := c.Retain(longContent)
	if err != nil {
		t.Logf("100KB retain: %v", err)
	} else {
		t.Logf("100KB retain result: %s", result[:testutil.Min(len(result), 100)])
	}
}

func TestEdgeCase_UnicodeContent(t *testing.T) {
	requireServerUp(t)

	c := mustConnect(t, "edge:unicode")
	defer c.Close()

	unicodeContent := "日本語テスト: こんにちは世界 🌍🚀 Émojis and àccénts über alles"
	result, err := c.Retain(unicodeContent)
	if err != nil {
		t.Logf("Unicode retain: %v", err)
	} else {
		t.Logf("Unicode retain result: %s", result[:testutil.Min(len(result), 100)])
	}
}

func TestEdgeCase_JSONInContent(t *testing.T) {
	requireServerUp(t)

	c := mustConnect(t, "edge:json")
	defer c.Close()

	jsonContent := `{"nested":"json","array":[1,2,3],"special":"quotes \\\" and \\\\ backslash"}`
	_, err := c.Retain(jsonContent)
	if err != nil {
		t.Logf("JSON retain: %v", err)
	} else {
		t.Logf("JSON retain: OK")
	}
}

// ─── 16. Atomic Counter Verification ───────────────────────────────────────

func TestAtomic_PanicsCounter(t *testing.T) {
	requireServerUp(t)

	resp, _ := http.Get(testutil.DefaultServerURL + "/health")
	var health map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&health)
	resp.Body.Close()

	panics, _ := health["panics_total"].(float64)
	retainPanics, _ := health["retain_panics"].(float64)
	reflectPanics, _ := health["reflect_panics"].(float64)

	t.Logf("Panics: total=%.0f retain=%.0f reflect=%.0f", panics, retainPanics, reflectPanics)

	if panics > 0 {
		t.Errorf("server has had %0.f panics", panics)
	}
}

// ─── 17. Queue Depth Verification ──────────────────────────────────────────

func TestQueue_DepthReported(t *testing.T) {
	requireServerUp(t)

	resp, _ := http.Get(testutil.DefaultServerURL + "/health")
	var health map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&health)
	resp.Body.Close()

	depth, _ := health["queue_depth"].(float64)
	t.Logf("Queue depth: %.0f", depth)

	if depth > 0 {
		t.Logf("Warning: queue not fully drained (depth=%.0f)", depth)
	}
}

// ─── 18. Session Count Tracking ────────────────────────────────────────────

func TestSession_CountIncreases(t *testing.T) {
	requireServerUp(t)

	resp, _ := http.Get(testutil.DefaultServerURL + "/health")
	var before map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&before)
	resp.Body.Close()
	beforeCount, _ := before["sessions"].(float64)

	c := mustConnect(t, "sess:count")
	defer c.Close()

	resp, _ = http.Get(testutil.DefaultServerURL + "/health")
	var after map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&after)
	resp.Body.Close()
	afterCount, _ := after["sessions"].(float64)

	if afterCount <= beforeCount {
		t.Errorf("session count didn't increase: before=%.0f after=%.0f", beforeCount, afterCount)
	} else {
		t.Logf("Session count: %.0f -> %.0f", beforeCount, afterCount)
	}
}
