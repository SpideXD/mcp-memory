package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"mcp-memory/backend"
	"mcp-memory/internal/testutil/cogneemock"
	"mcp-memory/logger"
	"mcp-memory/metrics"
	"mcp-memory/queue"
)

// ─── HTTP-level harness ─────────────────────────────────────────────────────
//
// The handler and MCP-protocol surface had 0% coverage: every test in the repo
// drove processQueueJob or the queue store directly, so nothing ever exercised
// SSE, JSON-RPC routing, auth, tool dispatch, /health or /debug/queue. This
// harness mounts the real routes from main.go on an httptest server and speaks
// the real protocol over the wire.

type httpHarness struct {
	t      *testing.T
	server *Server
	http   *httptest.Server
	mock   *cogneemock.Server
	store  *queue.Store
	logBuf *bytes.Buffer
}

// newHTTPHarness builds a Server backed by cogneemock and an in-memory queue,
// mounts the production routes, and returns a live HTTP endpoint.
func newHTTPHarness(t *testing.T, opts ...func(*Config)) *httpHarness {
	t.Helper()

	mock := cogneemock.NewServer()
	t.Cleanup(mock.Close)

	logBuf := &bytes.Buffer{}
	l, err := logger.NewBuf("http-test", "debug", logBuf)
	if err != nil {
		t.Fatalf("logger: %v", err)
	}

	cfg := Config{
		QueueDBPath:           ":memory:",
		QueueMaxPending:       1000,
		QueueJobTTL:           24 * time.Hour,
		QueueTTLInterval:      5 * time.Minute,
		QueueWorkerCount:      1,
		QueueMaxConcurrent:    1,
		MaxSessions:           100,
		SSEMessageBuffer:      100,
		MaxBodyBytes:          1 << 20,
		MaxContentBytes:       1 << 20,
		CogneePort:            fmt.Sprintf("%d", mock.Port()),
		CogneeRetainTimeout:   10 * time.Second,
		BackendRetainTimeout:  10 * time.Second,
		BackendRecallTimeout:  5 * time.Second,
		BackendReflectTimeout: 10 * time.Second,
		AutoImproveAfterN:     0,
		AutoImproveCooldown:   120 * time.Second,
	}
	for _, opt := range opts {
		opt(&cfg)
	}

	be := backend.New(backend.BackendConfig{
		Backend:               "cognee-rust",
		CogneePort:            fmt.Sprintf("%d", mock.Port()),
		TemporalCognify:       true,
		MemoryOnly:            true,
		BackendRetainTimeout:  cfg.BackendRetainTimeout,
		BackendRecallTimeout:  cfg.BackendRecallTimeout,
		BackendReflectTimeout: cfg.BackendReflectTimeout,
		CogneeRetainTimeout:   cfg.CogneeRetainTimeout,
		RetryAttempts:         1,
		RetryDelay:            5 * time.Millisecond,
		RetryMaxDelay:         20 * time.Millisecond,
	})

	store, err := queue.NewStore(queue.StoreConfig{
		DBPath:     ":memory:",
		MaxPending: cfg.QueueMaxPending,
		JobTTL:     cfg.QueueJobTTL,
	})
	if err != nil {
		t.Fatalf("queue store: %v", err)
	}
	t.Cleanup(func() { store.Close() })

	cogneeCtx, cogneeCancel := context.WithCancel(context.Background())
	s := &Server{
		state:         StateRunning,
		config:        cfg,
		backend:       be,
		log:           l,
		queueStore:    store,
		cogneeCtx:     cogneeCtx,
		cogneeCancel:  cogneeCancel,
		sessions:      make(map[string]*MCPSession),
		shutdown:      make(chan struct{}),
		dataDir:       t.TempDir(),
		improveState:  loadAutoImproveState(t.TempDir()),
		autoImproveWg: sync.WaitGroup{},
		metrics:       newTestMetrics(),
	}

	processFunc := func(ctx context.Context, job *queue.Job) error {
		return s.processQueueJob(ctx, job)
	}
	worker, err := queue.NewWorker(queue.WorkerConfig{
		Store:   store,
		Process: processFunc,
		Count:   cfg.QueueWorkerCount,
		SemSize: cfg.QueueMaxConcurrent,
	})
	if err != nil {
		t.Fatalf("queue worker: %v", err)
	}
	s.queueWorker = worker
	if cfg.QueueWorkerCount > 0 {
		worker.Start(context.Background())
	}

	// Mount the same routes main.go registers.
	mux := http.NewServeMux()
	mux.HandleFunc("/health", s.handleHealth)
	mux.HandleFunc("/start", s.handleStart)
	mux.HandleFunc("/stop", s.handleStop)
	mux.HandleFunc("/mcp/sse", s.handleMCPSSE)
	mux.HandleFunc("/mcp/message", s.handleMCPMessage)
	mux.HandleFunc("/debug/queue", s.handleDebugQueue)

	ts := httptest.NewServer(mux)

	t.Cleanup(func() {
		ts.Close()
		if s.queueWorker != nil {
			s.queueWorker.Stop()
		}
		cogneeCancel()
	})

	return &httpHarness{t: t, server: s, http: ts, mock: mock, store: store, logBuf: logBuf}
}

func newTestMetrics() *serverMetrics {
	return &serverMetrics{
		retainCalls:    metrics.NewCounter("memory.retain"),
		reflectCalls:   metrics.NewCounter("memory.reflect"),
		recallCalls:    metrics.NewCounter("memory.recall"),
		errorCalls:     metrics.NewCounter("memory.errors"),
		retainTotal:    metrics.NewCounter("memory.retain_total"),
		retainErrors:   metrics.NewCounter("memory.retain_errors"),
		recallTotal:    metrics.NewCounter("memory.recall_total"),
		reflectTotal:   metrics.NewCounter("memory.reflect_total"),
		improveTotal:   metrics.NewCounter("memory.improve_total"),
		forgetTotal:    metrics.NewCounter("memory.forget_total"),
		retainDur:      metrics.NewTimer("memory.retain_duration"),
		reflectDur:     metrics.NewTimer("memory.reflect_duration"),
		queueGauge:     metrics.NewGauge("memory.queue_depth"),
		sessionGauge:   metrics.NewGauge("memory.sessions"),
		sseDrops:       metrics.NewCounter("memory.sse_drops"),
		semaphoreGauge: metrics.NewGauge("memory.semaphore_in_use"),
		cogneePending:  metrics.NewGauge("memory.cognee_jobs_pending"),
	}
}

// ─── SSE client ─────────────────────────────────────────────────────────────

// sseClient is one agent connection: an open SSE stream plus the message
// endpoint the server handed back. Each client is an independent connection —
// sharing one would serialise JSON-RPC at the transport and make concurrency
// tests sequential.
type sseClient struct {
	t        *testing.T
	harness  *httpHarness
	endpoint string // /mcp/message?session_id=...
	messages chan string
	cancel   context.CancelFunc
	resp     *http.Response
}

// connectSSE opens an SSE session for bank. authToken may be empty.
func (h *httpHarness) connectSSE(bank, authToken string) (*sseClient, error) {
	h.t.Helper()

	u := h.http.URL + "/mcp/sse"
	if bank != "" {
		u += "?bank=" + bank
	}
	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		cancel()
		return nil, err
	}
	if authToken != "" {
		req.Header.Set("Authorization", "Bearer "+authToken)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		cancel()
		return nil, err
	}
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		cancel()
		return nil, fmt.Errorf("sse status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	c := &sseClient{
		t:        h.t,
		harness:  h,
		messages: make(chan string, 64),
		cancel:   cancel,
		resp:     resp,
	}

	endpointCh := make(chan string, 1)
	go func() {
		defer close(c.messages)
		defer resp.Body.Close()
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 0, 64*1024), 4<<20)
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
		c.endpoint = h.http.URL + ep
	case <-time.After(3 * time.Second):
		cancel()
		return nil, fmt.Errorf("timed out waiting for endpoint event")
	}
	h.t.Cleanup(c.Close)
	return c, nil
}

// mustConnect fails the test if the SSE connection cannot be established.
func (h *httpHarness) mustConnect(bank string) *sseClient {
	h.t.Helper()
	c, err := h.connectSSE(bank, "")
	if err != nil {
		h.t.Fatalf("connect bank=%q: %v", bank, err)
	}
	return c
}

func (c *sseClient) Close() { c.cancel() }

// sessionID extracts the session id from the message endpoint.
func (c *sseClient) sessionID() string {
	if i := strings.Index(c.endpoint, "session_id="); i >= 0 {
		return c.endpoint[i+len("session_id="):]
	}
	return ""
}

// rpcRaw posts a raw JSON-RPC body and returns the HTTP status.
func (c *sseClient) rpcRaw(body string, authToken string) int {
	c.t.Helper()
	req, err := http.NewRequest("POST", c.endpoint, strings.NewReader(body))
	if err != nil {
		c.t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if authToken != "" {
		req.Header.Set("Authorization", "Bearer "+authToken)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}

// rpcResponse is a decoded JSON-RPC reply delivered over SSE.
type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      interface{}     `json:"id"`
	Result  json.RawMessage `json:"result"`
	Error   *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// call issues a JSON-RPC request and waits for the matching SSE reply.
func (c *sseClient) call(method string, id interface{}, params interface{}) *rpcResponse {
	c.t.Helper()
	body := map[string]interface{}{"jsonrpc": "2.0", "id": id, "method": method}
	if params != nil {
		body["params"] = params
	}
	raw, _ := json.Marshal(body)
	if code := c.rpcRaw(string(raw), ""); code != http.StatusAccepted {
		c.t.Fatalf("%s: expected 202, got %d", method, code)
	}
	return c.await(3 * time.Second)
}

// callTool is a convenience wrapper for tools/call.
func (c *sseClient) callTool(name string, args map[string]interface{}) *rpcResponse {
	c.t.Helper()
	return c.call("tools/call", 1, map[string]interface{}{"name": name, "arguments": args})
}

// await reads the next SSE message and decodes it.
func (c *sseClient) await(timeout time.Duration) *rpcResponse {
	c.t.Helper()
	select {
	case msg, ok := <-c.messages:
		if !ok {
			c.t.Fatal("SSE stream closed while awaiting a response")
		}
		var r rpcResponse
		if err := json.Unmarshal([]byte(msg), &r); err != nil {
			c.t.Fatalf("decode SSE payload %q: %v", msg, err)
		}
		return &r
	case <-time.After(timeout):
		c.t.Fatal("timed out waiting for SSE response")
		return nil
	}
}

// toolText returns the text payload of a successful tools/call result.
func (r *rpcResponse) toolText(t *testing.T) string {
	t.Helper()
	if r.Error != nil {
		t.Fatalf("expected success, got JSON-RPC error %d: %s", r.Error.Code, r.Error.Message)
	}
	var out struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(r.Result, &out); err != nil {
		t.Fatalf("decode tool result %s: %v", r.Result, err)
	}
	if len(out.Content) == 0 {
		t.Fatalf("tool result had no content: %s", r.Result)
	}
	return out.Content[0].Text
}

// getJSON performs a GET and decodes the JSON body.
func (h *httpHarness) getJSON(t *testing.T, path string) (int, map[string]interface{}) {
	t.Helper()
	resp, err := http.Get(h.http.URL + path)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()
	var m map[string]interface{}
	body, _ := io.ReadAll(resp.Body)
	_ = json.Unmarshal(body, &m)
	return resp.StatusCode, m
}

// waitForJobStatus polls the queue until job reaches one of want, or fails.
func (h *httpHarness) waitForJobStatus(t *testing.T, jobID string, want ...queue.Status) *queue.Job {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var last queue.Status
	for time.Now().Before(deadline) {
		job, err := h.store.Get(jobID)
		if err != nil {
			t.Fatalf("queue get: %v", err)
		}
		if job != nil {
			last = job.Status
			for _, w := range want {
				if job.Status == w {
					return job
				}
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("job %s never reached %v (last status %q)", jobID, want, last)
	return nil
}

// jobIDFrom pulls the job_id out of a queued tool response.
func jobIDFrom(t *testing.T, payload string) string {
	t.Helper()
	var out struct {
		Status string `json:"status"`
		JobID  string `json:"job_id"`
	}
	if err := json.Unmarshal([]byte(payload), &out); err != nil {
		t.Fatalf("decode queued response %q: %v", payload, err)
	}
	if out.JobID == "" {
		t.Fatalf("no job_id in response %q", payload)
	}
	return out.JobID
}
