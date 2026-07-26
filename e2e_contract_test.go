//go:build e2e

// Package main, E2E contract suite.
//
// These tests drive a *live* mcp-memory against real Cognee, real llama.cpp
// embeddings and a real LLM. They are build-tag gated so they never run in the
// hermetic suite: `go test ./...` will not compile this file.
//
//	go test -tags=e2e -timeout 30m -run TestE2E ./...
//
// Every assertion here is deterministic. LLM output is non-deterministic, so
// recall *quality* belongs in the benchmark suite, not in a gate — a flaky gate
// gets ignored, and an ignored gate is worse than no gate. What is asserted:
// the pipeline moves data, statuses reach terminal states, banks stay isolated,
// and crash recovery works.
//
// Environment:
//
//	MCP_E2E_URL       base URL of a running mcp-memory (default http://localhost:8899)
//	MCP_E2E_TOKEN     bearer token, if the server has one configured
//	MCP_E2E_TIMEOUT   per-retain budget (default 180s — real retains run ~20-30s)
//
// The suite FAILS rather than skips when the server is unreachable. A silent
// skip is how a green E2E run comes to mean nothing.
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// ─── Configuration ───────────────────────────────────────────────────────────

func e2eURL() string {
	if u := os.Getenv("MCP_E2E_URL"); u != "" {
		return strings.TrimRight(u, "/")
	}
	return "http://localhost:8899"
}

func e2eToken() string { return os.Getenv("MCP_E2E_TOKEN") }

func e2eRetainBudget() time.Duration {
	if v := os.Getenv("MCP_E2E_TIMEOUT"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return 180 * time.Second
}

// e2eRunID namespaces every bank this run creates, so repeated runs never
// collide and real data is never touched.
var e2eRunID = fmt.Sprintf("e2e_%d", time.Now().UnixNano())

func e2eBank(name string) string { return fmt.Sprintf("%s:%s", e2eRunID, name) }

// TestMain fails loudly when the stack is not up. This is deliberate: the
// stress suite's silent ServerUp() skip is why a 4-second "ok" could be
// mistaken for a passing E2E run.
func TestMain(m *testing.M) {
	if err := e2ePreflight(); err != nil {
		fmt.Fprintf(os.Stderr, "\nE2E PREFLIGHT FAILED: %v\n\n"+
			"These tests require a running stack:\n"+
			"  BACKEND=cognee-rust make run\n"+
			"then re-run with -tags=e2e. Set MCP_E2E_URL to point elsewhere.\n\n", err)
		os.Exit(1)
	}
	os.Exit(m.Run())
}

// e2ePreflight verifies the server answers and reports its dependencies up.
func e2ePreflight() error {
	c := &http.Client{Timeout: 10 * time.Second}
	resp, err := c.Get(e2eURL() + "/health")
	if err != nil {
		return fmt.Errorf("GET %s/health: %w", e2eURL(), err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("/health returned %d", resp.StatusCode)
	}
	var h struct {
		Status string   `json:"status"`
		Down   []string `json:"down"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&h); err != nil {
		return fmt.Errorf("decode /health: %w", err)
	}
	if h.Status != "running" {
		return fmt.Errorf("server status %q (down: %v) — dependencies are not healthy",
			h.Status, h.Down)
	}
	return nil
}

// ─── Live client ─────────────────────────────────────────────────────────────

type e2eClient struct {
	t        *testing.T
	endpoint string
	messages chan string
	closeFn  func()
}

// e2eConnect opens a real SSE session against the live server.
func e2eConnect(t *testing.T, bank string) *e2eClient {
	t.Helper()

	req, err := http.NewRequest("GET", e2eURL()+"/mcp/sse?bank="+bank, nil)
	if err != nil {
		t.Fatalf("build SSE request: %v", err)
	}
	if tok := e2eToken(); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}

	// No client timeout: SSE streams for the life of the session.
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		t.Fatalf("open SSE for bank %q: %v", bank, err)
	}
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("SSE for bank %q: status %d: %s", bank, resp.StatusCode, strings.TrimSpace(string(body)))
	}

	c := &e2eClient{t: t, messages: make(chan string, 64)}
	endpointCh := make(chan string, 1)
	c.closeFn = func() { resp.Body.Close() }

	go func() {
		defer close(c.messages)
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 0, 64*1024), 8<<20)
		var event string
		for sc.Scan() {
			line := sc.Text()
			switch {
			case strings.HasPrefix(line, "event: "):
				event = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				data := strings.TrimPrefix(line, "data: ")
				if event == "endpoint" {
					select {
					case endpointCh <- data:
					default:
					}
				} else {
					select {
					case c.messages <- data:
					default:
					}
				}
			}
		}
	}()

	select {
	case ep := <-endpointCh:
		c.endpoint = e2eURL() + ep
	case <-time.After(15 * time.Second):
		c.closeFn()
		t.Fatalf("no endpoint event for bank %q within 15s", bank)
	}

	t.Cleanup(c.closeFn)
	return c
}

// call issues a JSON-RPC request and waits up to timeout for the SSE reply.
func (c *e2eClient) call(method string, params interface{}, timeout time.Duration) *rpcResponse {
	c.t.Helper()

	body, _ := json.Marshal(map[string]interface{}{
		"jsonrpc": "2.0", "id": 1, "method": method, "params": params,
	})
	req, err := http.NewRequest("POST", c.endpoint, strings.NewReader(string(body)))
	if err != nil {
		c.t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if tok := e2eToken(); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		c.t.Fatalf("post %s: %v", method, err)
	}
	resp.Body.Close()

	select {
	case msg, ok := <-c.messages:
		if !ok {
			c.t.Fatalf("SSE closed while awaiting %s", method)
		}
		var r rpcResponse
		if err := json.Unmarshal([]byte(msg), &r); err != nil {
			c.t.Fatalf("decode %s reply %q: %v", method, msg, err)
		}
		return &r
	case <-time.After(timeout):
		c.t.Fatalf("timed out after %v awaiting %s", timeout, method)
		return nil
	}
}

func (c *e2eClient) tool(name string, args map[string]interface{}, timeout time.Duration) *rpcResponse {
	c.t.Helper()
	return c.call("tools/call", map[string]interface{}{"name": name, "arguments": args}, timeout)
}

// retainAndWait stores content and blocks until the job reaches a terminal
// state, returning the final status.
func (c *e2eClient) retainAndWait(content string) (jobID, status string) {
	c.t.Helper()

	payload := c.tool("memory_retain", map[string]interface{}{"content": content}, 30*time.Second).toolText(c.t)
	jobID = jobIDFrom(c.t, payload)

	deadline := time.Now().Add(e2eRetainBudget())
	for time.Now().Before(deadline) {
		st := c.jobStatus(jobID)
		switch st {
		case "completed", "failed", "dead":
			return jobID, st
		}
		time.Sleep(time.Second)
	}
	c.t.Fatalf("job %s did not finish within %v (last status: %s)",
		jobID, e2eRetainBudget(), c.jobStatus(jobID))
	return jobID, ""
}

func (c *e2eClient) jobStatus(jobID string) string {
	c.t.Helper()
	payload := c.tool("memory_retain_status", map[string]interface{}{"job_id": jobID}, 30*time.Second).toolText(c.t)
	var out struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal([]byte(payload), &out); err != nil {
		c.t.Fatalf("decode status %q: %v", payload, err)
	}
	return out.Status
}

func e2eGetJSON(t *testing.T, path string) map[string]interface{} {
	t.Helper()
	resp, err := (&http.Client{Timeout: 15 * time.Second}).Get(e2eURL() + path)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()
	var m map[string]interface{}
	body, _ := io.ReadAll(resp.Body)
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("decode %s: %v (body: %s)", path, err, body)
	}
	return m
}

// ─── Contract tests ──────────────────────────────────────────────────────────

// TestE2E_HealthReportsAllServicesUp is the cheapest real signal: the server
// and both subprocesses agree they are alive.
func TestE2E_HealthReportsAllServicesUp(t *testing.T) {
	h := e2eGetJSON(t, "/health")

	if h["status"] != "running" {
		t.Fatalf("status = %v, want running (down: %v)", h["status"], h["down"])
	}
	for _, svc := range []string{"cognee", "llama"} {
		up, ok := h[svc].(bool)
		if !ok {
			t.Errorf("/health has no boolean %q field", svc)
			continue
		}
		if !up {
			t.Errorf("%s reports down", svc)
		}
	}
	if down, ok := h["down"].([]interface{}); ok && len(down) > 0 {
		t.Errorf("services down: %v", down)
	}
}

// TestE2E_RetainRecallRoundTrip is the core pipeline: store a fact through the
// real LLM extraction pipeline, then find it again by semantic search.
//
// The assertion is deliberately weak on wording — the LLM decides phrasing — but
// strong on substance: the distinctive entity must appear. If this fails, the
// pipeline is broken, not merely imprecise.
func TestE2E_RetainRecallRoundTrip(t *testing.T) {
	bank := e2eBank("roundtrip")
	c := e2eConnect(t, bank)

	const entity = "Zorbatech"
	jobID, status := c.retainAndWait("Priya Raman is the lead architect at " + entity + " since 2024.")
	if status != "completed" {
		t.Fatalf("retain job %s ended %q, want completed", jobID, status)
	}

	payload := c.tool("memory_recall",
		map[string]interface{}{"query": "Where does Priya Raman work?"},
		60*time.Second).toolText(t)

	if payload == "" {
		t.Fatal("recall returned an empty payload")
	}
	if !strings.Contains(payload, entity) {
		t.Fatalf("recall did not surface %q — the stored fact is not retrievable.\nGot: %s",
			entity, truncate(payload, 800))
	}
}

// TestE2E_BankIsolation is the privacy gate. Two banks, two secrets; neither
// may surface in the other's recall. A failure here is a data breach, not a
// quality issue, so it is asserted strictly.
func TestE2E_BankIsolation(t *testing.T) {
	bankA := e2eBank("iso_alpha")
	bankB := e2eBank("iso_beta")

	const secretA = "Quintarian"
	const secretB = "Velmoril"

	ca := e2eConnect(t, bankA)
	cb := e2eConnect(t, bankB)

	if _, st := ca.retainAndWait("The alpha project codename is " + secretA + ", set in 2025."); st != "completed" {
		t.Fatalf("alpha retain ended %q", st)
	}
	if _, st := cb.retainAndWait("The beta project codename is " + secretB + ", set in 2025."); st != "completed" {
		t.Fatalf("beta retain ended %q", st)
	}

	fromA := ca.tool("memory_recall",
		map[string]interface{}{"query": "What is the project codename?"}, 60*time.Second).toolText(t)
	fromB := cb.tool("memory_recall",
		map[string]interface{}{"query": "What is the project codename?"}, 60*time.Second).toolText(t)

	if strings.Contains(fromA, secretB) {
		t.Fatalf("LEAK: bank %s recalled bank %s's secret %q\nGot: %s",
			bankA, bankB, secretB, truncate(fromA, 800))
	}
	if strings.Contains(fromB, secretA) {
		t.Fatalf("LEAK: bank %s recalled bank %s's secret %q\nGot: %s",
			bankB, bankA, secretA, truncate(fromB, 800))
	}
}

// TestE2E_RetainStatusCrossBankDenied is the live counterpart to the hermetic
// cross-tenant test: a job ID from one bank must be invisible to another.
func TestE2E_RetainStatusCrossBankDenied(t *testing.T) {
	victim := e2eConnect(t, e2eBank("victim"))
	attacker := e2eConnect(t, e2eBank("attacker"))

	jobID, status := victim.retainAndWait("Confidential: acquisition of Norvex closes in 2026.")
	if status != "completed" {
		t.Fatalf("victim retain ended %q", status)
	}

	payload := attacker.tool("memory_retain_status",
		map[string]interface{}{"job_id": jobID}, 30*time.Second).toolText(t)

	if !strings.Contains(payload, "not_found") {
		t.Fatalf("cross-bank job status must report not_found, got: %s", truncate(payload, 500))
	}
	for _, leak := range []string{"Norvex", "victim", "Confidential"} {
		if strings.Contains(payload, leak) {
			t.Fatalf("cross-bank status leaked %q: %s", leak, truncate(payload, 500))
		}
	}
}

// TestE2E_QueueDrainsUnderConcurrency drives several agents at once — the
// workload the SQLite queue exists for — and requires every job to terminate.
func TestE2E_QueueDrainsUnderConcurrency(t *testing.T) {
	const agents = 4

	type result struct {
		jobID  string
		status string
	}
	results := make([]result, agents)

	var wg sync.WaitGroup
	for i := 0; i < agents; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			c := e2eConnect(t, e2eBank(fmt.Sprintf("conc%d", i)))
			id, st := c.retainAndWait(fmt.Sprintf(
				"Agent %d recorded observation number %d in 2026.", i, i*17))
			results[i] = result{id, st}
		}(i)
	}
	wg.Wait()

	seen := map[string]bool{}
	for i, r := range results {
		if r.jobID == "" {
			t.Errorf("agent %d produced no job id", i)
			continue
		}
		if seen[r.jobID] {
			t.Errorf("duplicate job id %s across concurrent agents", r.jobID)
		}
		seen[r.jobID] = true
		if r.status != "completed" {
			t.Errorf("agent %d job %s ended %q, want completed", i, r.jobID, r.status)
		}
	}

	q := e2eGetJSON(t, "/debug/queue")
	if pending, _ := q["pending"].(float64); pending > 0 {
		t.Errorf("queue still has %v pending after all jobs terminated", pending)
	}
}

// TestE2E_DebugQueueReflectsRealWork asserts the operational endpoint tracks
// actual work rather than reporting static zeros.
func TestE2E_DebugQueueReflectsRealWork(t *testing.T) {
	before := e2eGetJSON(t, "/debug/queue")
	beforeDone, _ := before["completed_total"].(float64)

	c := e2eConnect(t, e2eBank("debugq"))
	if _, st := c.retainAndWait("Telemetry checkpoint recorded in 2026."); st != "completed" {
		t.Fatalf("retain ended %q", st)
	}

	after := e2eGetJSON(t, "/debug/queue")
	afterDone, _ := after["completed_total"].(float64)

	if afterDone <= beforeDone {
		t.Errorf("completed_total did not advance: %v -> %v", beforeDone, afterDone)
	}
	for _, field := range []string{"pending", "running", "workers", "max_concurrent", "db_size_kb"} {
		if _, ok := after[field]; !ok {
			t.Errorf("/debug/queue missing %q", field)
		}
	}
	if size, _ := after["db_size_kb"].(float64); size <= 0 {
		t.Errorf("db_size_kb = %v — the queue DB should exist on disk", after["db_size_kb"])
	}
}

// TestE2E_ReflectCompletes exercises the graph-synthesis path end to end.
func TestE2E_ReflectCompletes(t *testing.T) {
	bank := e2eBank("reflect")
	c := e2eConnect(t, bank)

	if _, st := c.retainAndWait("Marisol Vega founded Halcyon Labs in 2023."); st != "completed" {
		t.Fatalf("seed retain ended %q", st)
	}

	payload := c.tool("memory_reflect", map[string]interface{}{"query": ""}, 30*time.Second).toolText(t)
	jobID := jobIDFrom(t, payload)

	deadline := time.Now().Add(e2eRetainBudget())
	for time.Now().Before(deadline) {
		switch st := c.jobStatus(jobID); st {
		case "completed":
			return
		case "failed", "dead":
			t.Fatalf("reflect job %s ended %q", jobID, st)
		}
		time.Sleep(time.Second)
	}
	t.Fatalf("reflect job %s did not finish within %v", jobID, e2eRetainBudget())
}

// TestE2E_OversizeContentRejectedBeforeBackend confirms input validation runs
// before any expensive LLM work is dispatched.
func TestE2E_OversizeContentRejectedBeforeBackend(t *testing.T) {
	c := e2eConnect(t, e2eBank("oversize"))

	resp := c.tool("memory_retain",
		map[string]interface{}{"content": strings.Repeat("x", 2<<20)}, 30*time.Second)
	if resp.Error == nil {
		t.Fatal("2MB content must be rejected, not queued to the LLM pipeline")
	}
}

// TestE2E_InvalidBankRejectedAtConnect verifies validation runs at the edge on
// the live server, not only in unit tests.
func TestE2E_InvalidBankRejectedAtConnect(t *testing.T) {
	for _, bank := range []string{"../etc/passwd", "bad bank", strings.Repeat("a", 129)} {
		req, err := http.NewRequest("GET", e2eURL()+"/mcp/sse?bank="+bank, nil)
		if err != nil {
			t.Fatalf("build request: %v", err)
		}
		if tok := e2eToken(); tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
		resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
		if err != nil {
			t.Fatalf("connect %q: %v", bank, err)
		}
		resp.Body.Close()
		if resp.StatusCode == 200 {
			t.Errorf("invalid bank %q was accepted", bank)
		}
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "… (truncated)"
}
