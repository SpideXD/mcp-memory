package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"mcp-memory/internal/testutil/cogneemock"
	"mcp-memory/queue"
)

// ─── Auth ────────────────────────────────────────────────────────────────────

func TestHTTP_AuthDisabledWhenNoTokenConfigured(t *testing.T) {
	h := newHTTPHarness(t)
	if _, err := h.connectSSE("bank", ""); err != nil {
		t.Fatalf("empty AuthToken must mean open access: %v", err)
	}
}

func TestHTTP_AuthRejectsMissingAndWrongToken(t *testing.T) {
	h := newHTTPHarness(t, func(c *Config) { c.AuthToken = "s3cret" })

	if _, err := h.connectSSE("bank", ""); err == nil {
		t.Fatal("missing Authorization header must be rejected")
	}
	if _, err := h.connectSSE("bank", "wrong"); err == nil {
		t.Fatal("wrong token must be rejected")
	}
	if _, err := h.connectSSE("bank", "s3cret"); err != nil {
		t.Fatalf("correct token must be accepted: %v", err)
	}
}

// TestHTTP_AuthEnforcedOnMessageEndpoint guards against auth being checked at
// connect time only. A leaked session id must not be usable without the token.
func TestHTTP_AuthEnforcedOnMessageEndpoint(t *testing.T) {
	h := newHTTPHarness(t, func(c *Config) { c.AuthToken = "s3cret" })
	c, err := h.connectSSE("bank", "s3cret")
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	if code := c.rpcRaw(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`, ""); code != http.StatusUnauthorized {
		t.Fatalf("message endpoint without token: got %d, want 401", code)
	}
	if code := c.rpcRaw(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`, "s3cret"); code != http.StatusAccepted {
		t.Fatalf("message endpoint with token: got %d, want 202", code)
	}
}

// TestHTTP_ControlEndpointsRequireAuth covers /start and /stop, which can halt
// the whole service.
func TestHTTP_ControlEndpointsRequireAuth(t *testing.T) {
	h := newHTTPHarness(t, func(c *Config) { c.AuthToken = "s3cret" })
	for _, path := range []string{"/start", "/stop"} {
		resp, err := http.Post(h.http.URL+path, "application/json", nil)
		if err != nil {
			t.Fatalf("POST %s: %v", path, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("POST %s without token: got %d, want 401", path, resp.StatusCode)
		}
	}
}

func TestHTTP_ControlEndpointsRejectWrongMethod(t *testing.T) {
	h := newHTTPHarness(t)
	for _, path := range []string{"/start", "/stop"} {
		resp, err := http.Get(h.http.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusMethodNotAllowed {
			t.Errorf("GET %s: got %d, want 405", path, resp.StatusCode)
		}
	}
}

// ─── Bank validation ─────────────────────────────────────────────────────────

// TestHTTP_BankValidation walks the boundary and the hostile inputs. The bank
// is the tenant identifier, so anything that slips through here is a
// cross-tenant or injection risk.
func TestHTTP_BankValidation(t *testing.T) {
	h := newHTTPHarness(t)

	valid := []struct{ name, bank string }{
		{"simple", "outreach"},
		{"profile:user", "outreach:spidex_owner"},
		{"hyphen and underscore", "a-b_c"},
		{"digits", "bank42"},
		{"max length 128", strings.Repeat("a", 128)},
		{"single char", "a"},
	}
	for _, tc := range valid {
		t.Run("valid/"+tc.name, func(t *testing.T) {
			if _, err := h.connectSSE(tc.bank, ""); err != nil {
				t.Errorf("bank %q should be accepted: %v", tc.name, err)
			}
		})
	}

	invalid := []struct{ name, bank string }{
		{"129 chars", strings.Repeat("a", 129)},
		{"path traversal", "../../etc/passwd"},
		{"slash", "a/b"},
		{"space", "a b"},
		{"sql metacharacters", "a';DROP TABLE jobs;--"},
		{"unicode", "café"},
		{"emoji", "bank🔥"},
		{"newline", "a%0Ab"},
		{"null byte", "a%00b"},
		{"dot", "a.b"},
		{"percent", "a%25b"},
		{"asterisk", "a*"},
	}
	for _, tc := range invalid {
		t.Run("invalid/"+tc.name, func(t *testing.T) {
			if _, err := h.connectSSE(tc.bank, ""); err == nil {
				t.Errorf("bank %q must be rejected", tc.name)
			}
		})
	}
}

// TestHTTP_BankEmptyConnectsButToolsFail documents the actual contract: an
// omitted bank is allowed at connect time, and every tool call then fails with
// a clear message rather than silently using a default bank.
func TestHTTP_BankEmptyConnectsButToolsFail(t *testing.T) {
	h := newHTTPHarness(t)
	c := h.mustConnect("")

	resp := c.callTool("memory_recall", map[string]interface{}{"query": "x"})
	if resp.Error == nil {
		t.Fatal("tool call without a bank must fail")
	}
	if !strings.Contains(resp.Error.Message, "bank is required") {
		t.Fatalf("error should explain the missing bank, got: %s", resp.Error.Message)
	}
}

// ─── JSON-RPC framing ────────────────────────────────────────────────────────

func TestHTTP_MessageEndpointRejectsBadRequests(t *testing.T) {
	h := newHTTPHarness(t)
	c := h.mustConnect("bank")

	t.Run("wrong method", func(t *testing.T) {
		resp, err := http.Get(c.endpoint)
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusMethodNotAllowed {
			t.Errorf("GET /mcp/message: got %d, want 405", resp.StatusCode)
		}
	})

	t.Run("missing session_id", func(t *testing.T) {
		resp, err := http.Post(h.http.URL+"/mcp/message", "application/json",
			strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
		if err != nil {
			t.Fatalf("POST: %v", err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("missing session_id: got %d, want 400", resp.StatusCode)
		}
	})

	t.Run("unknown session_id", func(t *testing.T) {
		resp, err := http.Post(h.http.URL+"/mcp/message?session_id=does-not-exist",
			"application/json", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
		if err != nil {
			t.Fatalf("POST: %v", err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("unknown session: got %d, want 400", resp.StatusCode)
		}
	})
}

// TestHTTP_MalformedJSONRPC checks the error codes the MCP spec defines.
func TestHTTP_MalformedJSONRPC(t *testing.T) {
	cases := []struct {
		name     string
		body     string
		wantCode int
	}{
		{"not JSON", `{{{`, -32700},
		{"truncated", `{"jsonrpc":"2.0","id":1`, -32700},
		{"empty body", ``, -32700},
		{"wrong version", `{"jsonrpc":"1.0","id":1,"method":"tools/list"}`, -32600},
		{"missing version", `{"id":1,"method":"tools/list"}`, -32600},
		{"null", `null`, -32600},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHTTPHarness(t)
			c := h.mustConnect("bank")
			// handleMCPMessage answers 202 on the success path but falls through
			// to an implicit 200 when decoding fails — the JSON-RPC error itself
			// travels over SSE either way. Accept any 2xx and assert on the SSE
			// payload, which is the contract that actually matters.
			if code := c.rpcRaw(tc.body, ""); code < 200 || code > 299 {
				t.Fatalf("got HTTP %d, want 2xx (errors travel over SSE)", code)
			}
			resp := c.await(3 * time.Second)
			if resp.Error == nil {
				t.Fatalf("expected a JSON-RPC error for %s", tc.name)
			}
			if resp.Error.Code != tc.wantCode {
				t.Errorf("error code = %d, want %d", resp.Error.Code, tc.wantCode)
			}
		})
	}
}

func TestHTTP_UnknownMethodAndTool(t *testing.T) {
	h := newHTTPHarness(t)
	c := h.mustConnect("bank")

	if resp := c.call("no/such/method", 1, nil); resp.Error == nil || resp.Error.Code != -32601 {
		t.Errorf("unknown method: got %+v, want code -32601", resp.Error)
	}
	if resp := c.callTool("memory_teleport", map[string]interface{}{}); resp.Error == nil || resp.Error.Code != -32601 {
		t.Errorf("unknown tool: got %+v, want code -32601", resp.Error)
	}
}

// TestHTTP_BodySizeLimitEnforced ensures an oversized POST cannot exhaust memory.
func TestHTTP_BodySizeLimitEnforced(t *testing.T) {
	h := newHTTPHarness(t, func(c *Config) { c.MaxBodyBytes = 4096 })
	c := h.mustConnect("bank")

	huge := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"memory_retain","arguments":{"content":%q}}}`,
		strings.Repeat("x", 64*1024))
	if code := c.rpcRaw(huge, ""); code < 200 || code > 299 {
		t.Fatalf("got %d, want 2xx", code)
	}
	resp := c.await(3 * time.Second)
	if resp.Error == nil {
		t.Fatal("body over MaxBodyBytes must produce an error, not a silent truncation")
	}
}

// ─── initialize / tools/list ─────────────────────────────────────────────────

func TestHTTP_InitializeReturnsProtocolVersion(t *testing.T) {
	h := newHTTPHarness(t)
	c := h.mustConnect("bank")

	resp := c.call("initialize", 1, nil)
	if resp.Error != nil {
		t.Fatalf("initialize failed: %s", resp.Error.Message)
	}
	var out struct {
		ProtocolVersion string `json:"protocolVersion"`
		ServerInfo      struct {
			Name string `json:"name"`
		} `json:"serverInfo"`
	}
	if err := json.Unmarshal(resp.Result, &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.ProtocolVersion == "" {
		t.Error("protocolVersion must be advertised")
	}
	if out.ServerInfo.Name != "mcp-memory" {
		t.Errorf("serverInfo.name = %q", out.ServerInfo.Name)
	}
}

// TestHTTP_ToolsListAdvertisesEveryImplementedTool pins the tool surface: a
// tool that is dispatchable but unlisted is invisible to agents, and a listed
// tool that is not dispatchable is a broken promise.
func TestHTTP_ToolsListAdvertisesEveryImplementedTool(t *testing.T) {
	h := newHTTPHarness(t)
	c := h.mustConnect("bank")

	resp := c.call("tools/list", 1, nil)
	if resp.Error != nil {
		t.Fatalf("tools/list failed: %s", resp.Error.Message)
	}
	var out struct {
		Tools []struct {
			Name        string                 `json:"name"`
			Description string                 `json:"description"`
			InputSchema map[string]interface{} `json:"inputSchema"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(resp.Result, &out); err != nil {
		t.Fatalf("decode: %v", err)
	}

	listed := map[string]bool{}
	for _, tool := range out.Tools {
		listed[tool.Name] = true
		if tool.Description == "" {
			t.Errorf("tool %q has no description", tool.Name)
		}
		if tool.InputSchema == nil {
			t.Errorf("tool %q has no inputSchema", tool.Name)
		}
	}
	for _, want := range []string{
		"memory_retain", "memory_recall", "memory_reflect",
		"memory_forget", "memory_retain_status",
	} {
		if !listed[want] {
			t.Errorf("tools/list omits %q", want)
		}
	}
	// memory_improve was removed; it must not reappear.
	if listed["memory_improve"] {
		t.Error("memory_improve was removed and must not be advertised")
	}
}

// ─── Tool argument validation ────────────────────────────────────────────────

func TestHTTP_ToolArgumentValidation(t *testing.T) {
	cases := []struct {
		tool string
		args map[string]interface{}
		name string
	}{
		{"memory_recall", map[string]interface{}{}, "recall without query"},
		{"memory_recall", map[string]interface{}{"query": ""}, "recall with empty query"},
		{"memory_retain", map[string]interface{}{}, "retain without content"},
		{"memory_retain", map[string]interface{}{"content": ""}, "retain with empty content"},
		{"memory_forget", map[string]interface{}{}, "forget without content_id"},
		{"memory_forget", map[string]interface{}{"content_id": ""}, "forget with empty content_id"},
		{"memory_retain_status", map[string]interface{}{}, "status without job_id"},
		{"memory_retain_status", map[string]interface{}{"job_id": ""}, "status with empty job_id"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHTTPHarness(t)
			c := h.mustConnect("bank")
			resp := c.callTool(tc.tool, tc.args)
			if resp.Error == nil {
				t.Fatalf("%s must be rejected", tc.name)
			}
			if resp.Error.Code != -32602 {
				t.Errorf("error code = %d, want -32602 (invalid params)", resp.Error.Code)
			}
		})
	}
}

// TestHTTP_ReflectAcceptsEmptyQuery is the counterpart: reflect with no query
// means "full improve" and must be allowed.
func TestHTTP_ReflectAcceptsEmptyQuery(t *testing.T) {
	h := newHTTPHarness(t)
	c := h.mustConnect("bank")

	resp := c.callTool("memory_reflect", map[string]interface{}{})
	payload := resp.toolText(t)
	if !strings.Contains(payload, `"status":"queued"`) {
		t.Fatalf("reflect with empty query should queue, got: %s", payload)
	}
}

func TestHTTP_RetainRejectsOversizeContent(t *testing.T) {
	h := newHTTPHarness(t, func(c *Config) {
		c.MaxContentBytes = 1024
		c.MaxBodyBytes = 1 << 20
	})
	c := h.mustConnect("bank")

	resp := c.callTool("memory_retain", map[string]interface{}{
		"content": strings.Repeat("x", 2048),
	})
	if resp.Error == nil {
		t.Fatal("content over MaxContentBytes must be rejected")
	}
	if !strings.Contains(resp.Error.Message, "exceeds maximum size") {
		t.Errorf("error should name the size limit, got: %s", resp.Error.Message)
	}

	// Exactly at the limit must still be accepted.
	resp = c.callTool("memory_retain", map[string]interface{}{
		"content": strings.Repeat("x", 1024),
	})
	if resp.Error != nil {
		t.Errorf("content exactly at the limit must be accepted: %s", resp.Error.Message)
	}
}

// ─── End-to-end tool behaviour over the wire ─────────────────────────────────

func TestHTTP_RecallReturnsBackendResult(t *testing.T) {
	h := newHTTPHarness(t)
	h.mock.SetResponse("/api/v1/recall", cogneemock.ResponseConfig{
		Body: `[{"_source":"graph","text":"Alice works at Sentinela"}]`,
	})
	c := h.mustConnect("outreach:alice")

	payload := c.callTool("memory_recall", map[string]interface{}{"query": "where does Alice work"}).toolText(t)
	if !strings.Contains(payload, "Sentinela") {
		t.Fatalf("recall did not return the backend payload: %s", payload)
	}
}

func TestHTTP_RecallSurfacesBackendErrors(t *testing.T) {
	h := newHTTPHarness(t)
	h.mock.SetResponse("/api/v1/recall", cogneemock.ResponseConfig{StatusCode: 500, Body: `{"e":"boom"}`})
	c := h.mustConnect("bank")

	resp := c.callTool("memory_recall", map[string]interface{}{"query": "q"})
	if resp.Error == nil {
		t.Fatal("a failing backend must surface as a JSON-RPC error, not an empty success")
	}
}

// TestHTTP_RetainQueuesAndCompletes walks the full async path: queue, worker,
// backend, terminal status — over the real protocol.
func TestHTTP_RetainQueuesAndCompletes(t *testing.T) {
	h := newHTTPHarness(t)
	c := h.mustConnect("outreach:alice")

	payload := c.callTool("memory_retain", map[string]interface{}{
		"content": "Alice joined Sentinela in 2024",
	}).toolText(t)
	if !strings.Contains(payload, `"status":"queued"`) {
		t.Fatalf("retain should return queued, got: %s", payload)
	}
	jobID := jobIDFrom(t, payload)

	job := h.waitForJobStatus(t, jobID, queue.StatusCompleted)
	if job.Bank != "outreach:alice" {
		t.Errorf("job bank = %q, want outreach:alice", job.Bank)
	}
	if job.Type != "retain" {
		t.Errorf("job type = %q, want retain", job.Type)
	}
	if got := h.mock.CallCount("/api/v1/remember"); got != 1 {
		t.Errorf("backend received %d remember calls, want 1", got)
	}
}

// TestHTTP_RetainStatusReportsLifecycle checks the status tool an agent polls.
func TestHTTP_RetainStatusReportsLifecycle(t *testing.T) {
	h := newHTTPHarness(t)
	c := h.mustConnect("bank")

	jobID := jobIDFrom(t, c.callTool("memory_retain",
		map[string]interface{}{"content": "fact 2026"}).toolText(t))
	h.waitForJobStatus(t, jobID, queue.StatusCompleted)

	payload := c.callTool("memory_retain_status", map[string]interface{}{"job_id": jobID}).toolText(t)
	var status struct {
		JobID  string `json:"job_id"`
		Bank   string `json:"bank"`
		Status string `json:"status"`
	}
	if err := json.Unmarshal([]byte(payload), &status); err != nil {
		t.Fatalf("decode status %q: %v", payload, err)
	}
	if status.JobID != jobID || status.Status != "completed" {
		t.Fatalf("status = %+v, want completed for %s", status, jobID)
	}
}

func TestHTTP_RetainStatusUnknownJob(t *testing.T) {
	h := newHTTPHarness(t)
	c := h.mustConnect("bank")

	payload := c.callTool("memory_retain_status",
		map[string]interface{}{"job_id": "no-such-job"}).toolText(t)
	if !strings.Contains(payload, "not_found") {
		t.Fatalf("unknown job should report not_found, got: %s", payload)
	}
}

// TestHTTP_RetainStatusDoesNotLeakAcrossBanks is a privacy check: a job id
// belonging to one tenant must not expose that tenant's data to another.
func TestHTTP_RetainStatusDoesNotLeakAcrossBanks(t *testing.T) {
	h := newHTTPHarness(t)
	victim := h.mustConnect("email:client_42")
	attacker := h.mustConnect("email:attacker")

	jobID := jobIDFrom(t, victim.callTool("memory_retain",
		map[string]interface{}{"content": "confidential salary data 2026"}).toolText(t))
	h.waitForJobStatus(t, jobID, queue.StatusCompleted)

	payload := attacker.callTool("memory_retain_status",
		map[string]interface{}{"job_id": jobID}).toolText(t)

	if strings.Contains(payload, "confidential salary data") {
		t.Fatalf("CROSS-TENANT LEAK: attacker read victim job content: %s", payload)
	}
	if strings.Contains(payload, "email:client_42") {
		t.Errorf("status exposes the owning bank to another tenant: %s", payload)
	}
}

// TestHTTP_QueueFullIsReportedNotDropped ensures backpressure is visible to the
// agent rather than silently discarded.
func TestHTTP_QueueFullIsReportedNotDropped(t *testing.T) {
	h := newHTTPHarness(t, func(c *Config) {
		c.QueueMaxPending = 2
		c.QueueWorkerCount = 0 // nothing drains the queue
	})
	c := h.mustConnect("bank")

	var rejected int
	for i := 0; i < 6; i++ {
		payload := c.callTool("memory_retain",
			map[string]interface{}{"content": fmt.Sprintf("fact %d 2026", i)}).toolText(t)
		if strings.Contains(payload, "queue_full") {
			rejected++
		}
	}
	if rejected == 0 {
		t.Fatal("queue over capacity must reject with queue_full, not accept silently")
	}
}

// ─── Bank isolation across the whole surface ─────────────────────────────────

// TestHTTP_BankIsolationEndToEnd is the core multi-tenant property: each
// session's bank reaches the backend unchanged and never another session's.
func TestHTTP_BankIsolationEndToEnd(t *testing.T) {
	h := newHTTPHarness(t)
	a := h.mustConnect("tenant:alpha")
	b := h.mustConnect("tenant:beta")

	jobA := jobIDFrom(t, a.callTool("memory_retain",
		map[string]interface{}{"content": "alpha secret 2026"}).toolText(t))
	jobB := jobIDFrom(t, b.callTool("memory_retain",
		map[string]interface{}{"content": "beta secret 2026"}).toolText(t))

	h.waitForJobStatus(t, jobA, queue.StatusCompleted)
	h.waitForJobStatus(t, jobB, queue.StatusCompleted)

	var sawAlpha, sawBeta bool
	for _, req := range h.mock.Requests() {
		if req.Path != "/api/v1/remember" {
			continue
		}
		hasAlphaBank := strings.Contains(req.Body, "tenant:alpha")
		hasBetaBank := strings.Contains(req.Body, "tenant:beta")
		if hasAlphaBank && hasBetaBank {
			t.Fatalf("a single request carried both banks: %s", req.Body)
		}
		if hasAlphaBank {
			sawAlpha = true
			if strings.Contains(req.Body, "beta secret") {
				t.Fatalf("beta content sent under alpha's bank: %s", req.Body)
			}
		}
		if hasBetaBank {
			sawBeta = true
			if strings.Contains(req.Body, "alpha secret") {
				t.Fatalf("alpha content sent under beta's bank: %s", req.Body)
			}
		}
	}
	if !sawAlpha || !sawBeta {
		t.Fatalf("expected both banks to reach the backend (alpha=%v beta=%v)", sawAlpha, sawBeta)
	}
}

// TestHTTP_BankIsImmutableAfterConnect verifies the bank cannot be switched by
// a later request parameter — it is fixed at session creation.
func TestHTTP_BankIsImmutableAfterConnect(t *testing.T) {
	h := newHTTPHarness(t)
	c := h.mustConnect("tenant:alpha")

	// Attempt to override the bank via the message endpoint's query string.
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"memory_recall","arguments":{"query":"q"}}}`
	req, _ := http.NewRequest("POST", c.endpoint+"&bank=tenant:beta", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	resp.Body.Close()
	c.await(3 * time.Second)

	last := h.mock.LastRequest("/api/v1/recall")
	if last == nil {
		t.Fatal("no recall reached the backend")
	}
	if strings.Contains(last.Body, "tenant:beta") {
		t.Fatalf("bank was overridden after connect: %s", last.Body)
	}
	if !strings.Contains(last.Body, "tenant:alpha") {
		t.Fatalf("session bank lost: %s", last.Body)
	}
}

// ─── Sessions ────────────────────────────────────────────────────────────────

func TestHTTP_SessionLimitEnforced(t *testing.T) {
	h := newHTTPHarness(t, func(c *Config) { c.MaxSessions = 3 })

	for i := 0; i < 3; i++ {
		if _, err := h.connectSSE("bank", ""); err != nil {
			t.Fatalf("connection %d should succeed: %v", i, err)
		}
	}
	if _, err := h.connectSSE("bank", ""); err == nil {
		t.Fatal("connection past MaxSessions must be rejected")
	}
}

// TestHTTP_SessionLimitConcurrentTOCTOU hammers the limit from many goroutines.
// A check-then-insert race here lets the cap be exceeded under load.
func TestHTTP_SessionLimitConcurrentTOCTOU(t *testing.T) {
	const limit = 10
	h := newHTTPHarness(t, func(c *Config) { c.MaxSessions = limit })

	var wg sync.WaitGroup
	var mu sync.Mutex
	accepted := 0
	for i := 0; i < 60; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := h.connectSSE("bank", ""); err == nil {
				mu.Lock()
				accepted++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	if accepted > limit {
		t.Fatalf("TOCTOU: %d sessions accepted with MaxSessions=%d", accepted, limit)
	}
	if accepted == 0 {
		t.Fatal("no session was accepted at all")
	}
}

// TestHTTP_SessionReleasedOnDisconnect ensures capacity is reclaimed, otherwise
// the server bleeds session slots until it refuses all connections.
func TestHTTP_SessionReleasedOnDisconnect(t *testing.T) {
	h := newHTTPHarness(t, func(c *Config) { c.MaxSessions = 2 })

	c1, err := h.connectSSE("bank", "")
	if err != nil {
		t.Fatalf("first connect: %v", err)
	}
	if _, err := h.connectSSE("bank", ""); err != nil {
		t.Fatalf("second connect: %v", err)
	}
	if _, err := h.connectSSE("bank", ""); err == nil {
		t.Fatal("third connect should be refused")
	}

	c1.Close()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := h.connectSSE("bank", ""); err == nil {
			return // slot reclaimed
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("session slot was never released after disconnect")
}

// TestHTTP_ConcurrentToolCallsAcrossSessions drives many independent agents at
// once, which is the workload the queue was built for.
func TestHTTP_ConcurrentToolCallsAcrossSessions(t *testing.T) {
	const agents = 20
	h := newHTTPHarness(t, func(c *Config) {
		c.QueueWorkerCount = 4
		c.QueueMaxConcurrent = 3
	})

	clients := make([]*sseClient, agents)
	for i := range clients {
		clients[i] = h.mustConnect(fmt.Sprintf("tenant:agent%d", i))
	}

	var wg sync.WaitGroup
	jobIDs := make([]string, agents)
	for i, c := range clients {
		wg.Add(1)
		go func(i int, c *sseClient) {
			defer wg.Done()
			payload := c.callTool("memory_retain",
				map[string]interface{}{"content": fmt.Sprintf("agent %d fact 2026", i)}).toolText(t)
			jobIDs[i] = jobIDFrom(t, payload)
		}(i, c)
	}
	wg.Wait()

	seen := map[string]bool{}
	for i, id := range jobIDs {
		if id == "" {
			t.Fatalf("agent %d got no job id", i)
		}
		if seen[id] {
			t.Fatalf("duplicate job id %s — id generation collides under concurrency", id)
		}
		seen[id] = true
	}
	for _, id := range jobIDs {
		h.waitForJobStatus(t, id, queue.StatusCompleted)
	}
}

// ─── /health and /debug/queue ────────────────────────────────────────────────

func TestHTTP_HealthReportsShape(t *testing.T) {
	h := newHTTPHarness(t)
	h.mustConnect("bank")

	code, body := h.getJSON(t, "/health")
	if code != http.StatusOK {
		t.Fatalf("/health status = %d", code)
	}
	for _, field := range []string{"status", "sessions", "queue_depth", "uptime", "down"} {
		if _, ok := body[field]; !ok {
			t.Errorf("/health missing field %q (got %v)", field, keysOf(body))
		}
	}
	if _, ok := body["down"].([]interface{}); !ok {
		t.Errorf(`"down" must be a JSON array, never null: %#v`, body["down"])
	}
	if n, ok := body["sessions"].(float64); !ok || n < 1 {
		t.Errorf("sessions = %v, want at least 1", body["sessions"])
	}
}

// TestHTTP_DebugQueueReportsCounts covers the endpoint M5 shipped and verified
// only by reading the code — it had never been executed by a test.
func TestHTTP_DebugQueueReportsCounts(t *testing.T) {
	h := newHTTPHarness(t, func(c *Config) { c.QueueWorkerCount = 0 })
	c := h.mustConnect("bank")

	for i := 0; i < 3; i++ {
		c.callTool("memory_retain", map[string]interface{}{"content": fmt.Sprintf("f%d 2026", i)})
	}

	code, body := h.getJSON(t, "/debug/queue")
	if code != http.StatusOK {
		t.Fatalf("/debug/queue status = %d", code)
	}
	for _, field := range []string{
		"pending", "running", "completed_total", "failed_total", "dead_total",
		"oldest_pending_age_s", "workers", "max_concurrent", "db_size_kb",
	} {
		if _, ok := body[field]; !ok {
			t.Errorf("/debug/queue missing field %q (got %v)", field, keysOf(body))
		}
	}
	if p, _ := body["pending"].(float64); p != 3 {
		t.Errorf("pending = %v, want 3", body["pending"])
	}
}

func TestHTTP_DebugQueueRejectsNonGET(t *testing.T) {
	h := newHTTPHarness(t)
	resp, err := http.Post(h.http.URL+"/debug/queue", "application/json", nil)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("POST /debug/queue: got %d, want 405", resp.StatusCode)
	}
}

// TestHTTP_DebugQueueTracksCompletion verifies the counters move as jobs drain,
// so operators can actually see progress.
func TestHTTP_DebugQueueTracksCompletion(t *testing.T) {
	h := newHTTPHarness(t)
	c := h.mustConnect("bank")

	jobID := jobIDFrom(t, c.callTool("memory_retain",
		map[string]interface{}{"content": "drain me 2026"}).toolText(t))
	h.waitForJobStatus(t, jobID, queue.StatusCompleted)

	_, body := h.getJSON(t, "/debug/queue")
	if done, _ := body["completed_total"].(float64); done < 1 {
		t.Errorf("completed_total = %v, want at least 1", body["completed_total"])
	}
}

func keysOf(m map[string]interface{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
