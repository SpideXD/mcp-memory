package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"time"

	"mcp-memory/logger"
	"mcp-memory/metrics"
)

var bankNamePattern = regexp.MustCompile(`^[a-zA-Z0-9:_-]{1,128}$`)

// newJobID generates a cryptographically random job ID (UUID-like).
// Replaces time.Now().UnixNano() which can collide under concurrent load.
func newJobID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	s.mu.RLock()
	state := string(s.state)
	s.mu.RUnlock()

	// Compute composite status based on actual service health
	llama, reranker, hindsight := s.svc.allHealthy()
	allHealthy := llama && reranker && hindsight

	status := state
	if state == "running" && !allHealthy {
		status = "degraded"
	}

	// Build list of down services
	var down []string
	if !llama { down = append(down, "llama (embedder)") }
	if !reranker { down = append(down, "llama (reranker)") }
	if !hindsight { down = append(down, "hindsight") }
	if down == nil { down = []string{} } // Prevent JSON null
	s.sessionsMu.RLock()
	n := len(s.sessions)
	s.sessionsMu.RUnlock()

	retainStats := s.workers.retainPool.Stats()
	reflectStats := s.workers.reflectPool.Stats()

	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"status":          status,
		"version":         Version,
		"built":           BuildTime,
		"hindsight":       hindsight,
		"llama":           llama,
		"reranker":        reranker,
		"down":            down,
		"queue_depth":     len(s.workers.retainJobs) + len(s.workers.reflectJobs),
		"retain_workers":  retainStats["workers"],
		"retain_panics":   retainStats["panics"],
		"reflect_workers": reflectStats["workers"],
		"reflect_panics":  reflectStats["panics"],
		"sessions":        n,
		"sse_drops":       s.metrics.sseDrops.Value(),
		"uptime":          time.Since(s.startTime).String(),
		"panics_total":    s.panics.Load(),
		"metrics":         metrics.Snapshot("memory"),
	})
}

func (s *Server) handleStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost { http.Error(w, "method not allowed", 405); return }
	if !s.checkAuth(r) { http.Error(w, `{"error":"unauthorized"}`, 401); return }
	w.Header().Set("Content-Type", "application/json")
	s.mu.RLock()
	running := s.state == StateRunning
	s.mu.RUnlock()
	if running {
		json.NewEncoder(w).Encode(map[string]string{"status": "already running"})
		return
	}
	if err := s.Start(); err != nil { w.WriteHeader(500); json.NewEncoder(w).Encode(map[string]string{"error": err.Error()}); return }
	json.NewEncoder(w).Encode(map[string]string{"status": "started"})
}

func (s *Server) handleStop(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost { http.Error(w, "method not allowed", 405); return }
	if !s.checkAuth(r) { http.Error(w, `{"error":"unauthorized"}`, 401); return }
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "shutting down"})
	// Stop in goroutine so HTTP response can be sent before the server dies
	go s.Stop()
}

func (s *Server) handleMCPSSE(w http.ResponseWriter, r *http.Request) {
	if !s.checkAuth(r) { http.Error(w, `{"error":"unauthorized"}`, 401); return }
	// Parse and validate bank from URL
	bank := ""
	if raw := r.URL.Query().Get("bank"); raw != "" {
		decoded, err := url.QueryUnescape(raw)
		if err != nil {
			http.Error(w, `{"error":"invalid bank encoding"}`, 400)
			return
		}
		bank = decoded
		if !bankNamePattern.MatchString(bank) {
			http.Error(w, `{"error":"invalid bank name: use [a-zA-Z0-9:_-] (1-128 chars)"}`, 400)
			return
		}
	}

	// Use write lock from the start to prevent TOCTOU race on session limit
	s.sessionsMu.Lock()
	if len(s.sessions) >= s.config.MaxSessions {
		s.sessionsMu.Unlock()
		http.Error(w, `{"error":"too many sessions"}`, 503)
		return
	}
	id := newSessionID()
	if _, exists := s.sessions[id]; exists {
		s.sessionsMu.Unlock()
		s.log.Warn("session ID collision (1 in 2^128)", "id", id[:8])
		http.Error(w, `{"error":"internal error"}`, 500)
		return
	}
	ch := make(chan string, s.config.SSEMessageBuffer)
	sess := &MCPSession{SessionID: id, Bank: bank, SSEChannel: ch, CreatedAt: time.Now(), LastActive: time.Now()}
	s.sessions[id] = sess
	s.sessionsMu.Unlock()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
	w.Header().Set("X-Accel-Buffering", "no") // Disable nginx buffering for SSE
	defer func() { s.sessionsMu.Lock(); delete(s.sessions, id); s.sessionsMu.Unlock() }()

	fmt.Fprintf(w, "event: endpoint\ndata: /mcp/message?session_id=%s\n\n", id)
	if f, ok := w.(http.Flusher); ok { f.Flush() }
	for {
		select {
		case msg, ok := <-ch:
			if !ok { return }
			fmt.Fprintf(w, "event: message\ndata: %s\n\n", msg)
			if f, ok := w.(http.Flusher); ok { f.Flush() }
			sess.LastActive = time.Now()
		case <-r.Context().Done():
			return
		}
	}
}

func (s *Server) handleMCPMessage(w http.ResponseWriter, r *http.Request) {
	if !s.checkAuth(r) { http.Error(w, `{"error":"unauthorized"}`, 401); return }
	if r.Method != "POST" { http.Error(w, "method not allowed", 405); return }
	r.Body = http.MaxBytesReader(w, r.Body, s.config.MaxBodyBytes)
	sid := r.URL.Query().Get("session_id")
	if sid == "" { http.Error(w, "session_id required", 400); return }
	s.sessionsMu.RLock(); _, ok := s.sessions[sid]; s.sessionsMu.RUnlock()
	if !ok { http.Error(w, "invalid session", 400); return }
	var req struct{ JSONRPC string `json:"jsonrpc"`; ID interface{} `json:"id"`; Method string `json:"method"`; Params json.RawMessage `json:"params"` }
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil { s.mcpError(sid, nil, -32700, "Parse error"); return }
	if req.JSONRPC != "2.0" { s.mcpError(sid, nil, -32600, "Invalid Request: jsonrpc must be 2.0"); return }
	w.WriteHeader(202)
	go s.safeRouteMCP(sid, req.Method, req.ID, req.Params)
}

func (s *Server) safeRouteMCP(sid string, method string, id interface{}, params json.RawMessage) {
	defer func() {
		if r := recover(); r != nil {
			s.panics.Add(1)
			s.log.Error("routeMCP panic", "panic", fmt.Sprintf("%v", r))
			s.alerts.Send(AlertError, fmt.Sprintf("Tool call panic: %v", r), nil)
			s.metrics.errorCalls.Inc()
			s.writeSSE(sid, id, "error", map[string]interface{}{"code": -32603, "message": "internal error"})
		}
	}()
	s.routeMCP(sid, method, id, params)
}

func (s *Server) routeMCP(sid string, method string, id interface{}, params json.RawMessage) {
	switch method {
	case "initialize":
		s.mcpResult(sid, id, initResponse())
	case "notifications/initialized":
	case "ping":
		s.mcpResult(sid, id, map[string]interface{}{})
	case "tools/list":
		s.mcpResult(sid, id, s.toolsList())
	case "tools/call":
		s.handleToolCall(sid, id, params)
	default:
		s.mcpError(sid, id, -32601, fmt.Sprintf("unknown method: %s", method))
	}
}

func (s *Server) handleToolCall(sid string, id interface{}, params json.RawMessage) {
	var c struct{ Name string `json:"name"`; Arguments json.RawMessage `json:"arguments"` }
	if json.Unmarshal(params, &c) != nil { s.mcpError(sid, id, -32602, "invalid params"); return }

	// Generate per-request trace ID for end-to-end tracing
	reqID := fmt.Sprintf("%s-%s", c.Name, sid[:8])
	start := time.Now()

	s.sessionsMu.RLock(); sess, ok := s.sessions[sid]; s.sessionsMu.RUnlock()
	if !ok { s.mcpError(sid, id, -32000, "session not found"); return }
	bank := sess.Bank
	if bank == "" { s.mcpError(sid, id, -32000, "bank is required: connect with ?bank=name"); return }

	logReq := func(result string, err error) {
		d := time.Since(start)
		switch {
		case err != nil:
			s.log.Error("tool call failed", "req", reqID, "bank", bank, "duration", d, logger.Error(err))
		case c.Name == "memory_retain" && d > 15*time.Second:
			s.log.Warn("slow retain", "req", reqID, "bank", bank, "duration", d)
		case c.Name == "memory_recall" && d > 5*time.Second:
			s.log.Warn("slow recall", "req", reqID, "bank", bank, "duration", d)
		case c.Name == "memory_reflect" && d > 30*time.Second:
			s.log.Warn("slow reflect", "req", reqID, "bank", bank, "duration", d)
		default:
			s.log.Debug("tool call", "req", reqID, "bank", bank, "duration", d, "result", result)
		}
	}

	switch c.Name {
	case "memory_recall":
		var a struct{ Query string `json:"query"` }
		if err := json.Unmarshal(c.Arguments, &a); err != nil || a.Query == "" {
			s.mcpError(sid, id, -32602, "query is required")
			logReq("", fmt.Errorf("missing query"))
			return
		}
		s.metrics.recallCalls.Inc()
		s.metrics.recallTotal.Inc()
		r, err := s.backend.Recall(context.Background(), bank, a.Query)
		if err != nil { s.mcpError(sid, id, -32000, err.Error()); logReq("", err); return }
		s.mcpToolResult(sid, id, r)
		logReq("ok", nil)

	case "memory_retain":
		var a struct{ Content string `json:"content"` }
		if err := json.Unmarshal(c.Arguments, &a); err != nil || a.Content == "" {
			s.mcpError(sid, id, -32602, "content is required")
			logReq("", fmt.Errorf("missing content"))
			return
		}
		// Validate content size to prevent OOM
		if len(a.Content) > s.config.MaxContentBytes {
			s.mcpError(sid, id, -32602, fmt.Sprintf("content exceeds maximum size (%d bytes)", s.config.MaxContentBytes))
			logReq("", fmt.Errorf("content too large: %d bytes", len(a.Content)))
			return
		}
		s.metrics.retainCalls.Inc()
		s.metrics.retainTotal.Inc()

		if s.backend.IsSync() {
			// ★ HINDSIGHT PATH: queue to worker pool (unchanged)
			r, err := s.queueJob(s.workers.retainJobs, bank, "retain", a.Content)
			if err != nil { s.mcpError(sid, id, -32000, err.Error()); logReq("", err); return }
			s.mcpToolResult(sid, id, r.Data)
			logReq("ok", nil)
			return
		}

		// ★ COGNEE PATH: goroutine-per-retain with semaphore
		jobID := newJobID()

		// Acquire semaphore BEFORE storing in tracker (P2-C3 fix)
		select {
		case s.cogneeSemaphore <- struct{}{}:
			// acquired slot
			s.log.Debug("semaphore_acquired", "bank", bank, "job_id", jobID, "slots", len(s.cogneeSemaphore))
			s.metrics.semaphoreGauge.Set(int64(len(s.cogneeSemaphore)))
		default:
			s.mcpToolResult(sid, id, `{"status":"rejected","reason":"too_many_concurrent_retains"}`)
			return
		}

		// Store AFTER successful semaphore acquire
		if s.jobTracker != nil {
			s.jobTracker.store(jobID, bank)
			s.metrics.cogneePending.Set(int64(s.jobTracker.stats().Pending))
		}

		s.cogneeWg.Add(1)
		go func() {
			defer s.cogneeWg.Done()
			defer func() {
				<-s.cogneeSemaphore
				s.log.Debug("semaphore_released", "bank", bank, "job_id", jobID)
				s.metrics.semaphoreGauge.Set(int64(len(s.cogneeSemaphore)))
			}()
			defer func() {
				if r := recover(); r != nil {
					s.panics.Add(1)
					s.log.Error("cognee retain goroutine panicked", "bank", bank, "job_id", jobID, "panic", fmt.Sprintf("%v", r))
					if s.jobTracker != nil {
						s.jobTracker.fail(jobID, "internal error: panic")
						s.metrics.cogneePending.Set(int64(s.jobTracker.stats().Pending))
					}
				}
			}()

			s.log.Info("goroutine_started", "name", "cognee_retain", "bank", bank, "job_id", jobID)
			defer s.log.Info("goroutine_stopped", "name", "cognee_retain", "bank", bank, "job_id", jobID)

			s.log.Info("cognee retain started", "bank", bank, "job_id", jobID)
			startTime := time.Now()

			detachedCtx, cancel := context.WithTimeout(s.cogneeCtx, s.config.CogneeRetainTimeout)
			defer cancel()

			result, err := s.backend.Retain(detachedCtx, bank, a.Content)
			duration := time.Since(startTime)

			if err != nil {
				s.log.Error("cognee retain failed", "bank", bank, "job_id", jobID, "duration", duration, logger.Error(err))
				s.metrics.errorCalls.Inc()
				s.metrics.retainErrors.Inc()
				if s.jobTracker != nil {
					s.jobTracker.fail(jobID, err.Error())
					s.metrics.cogneePending.Set(int64(s.jobTracker.stats().Pending))
				}
				s.fireErrorWebhook(bank, jobID, err.Error(), "retain")
			} else {
				s.log.Info("cognee retain completed", "bank", bank, "job_id", jobID, "duration", duration)
				if s.jobTracker != nil {
					s.jobTracker.complete(jobID, result)
					s.metrics.cogneePending.Set(int64(s.jobTracker.stats().Pending))
				}
			}

			// Trigger auto-improve after retain
			s.maybeAutoImprove(bank)
		}()

		s.log.Info("retain_queued", "bank", bank, "job_id", jobID)
		s.mcpToolResult(sid, id, fmt.Sprintf(`{"status":"queued","bank":"%s","job_id":"%s"}`, bank, jobID))
		logReq("ok", nil)

	case "memory_reflect":
		var a struct{ Query string `json:"query"` }
		if err := json.Unmarshal(c.Arguments, &a); err != nil || a.Query == "" {
			s.mcpError(sid, id, -32602, "query is required")
			logReq("", fmt.Errorf("missing query"))
			return
		}
		s.metrics.reflectCalls.Inc()
		s.metrics.reflectTotal.Inc()

		if s.backend.IsSync() {
			// ★ HINDSIGHT PATH: queue to worker pool
			r, err := s.queueJob(s.workers.reflectJobs, bank, "reflect", a.Query)
			if err != nil { s.mcpError(sid, id, -32000, err.Error()); logReq("", err); return }
			s.mcpToolResult(sid, id, r.Data)
			logReq("ok", nil)
			return
		}

		// ★ COGNEE PATH: goroutine, immediate response
		s.cogneeWg.Add(1)
		go func() {
			defer s.cogneeWg.Done()
			defer func() {
				if r := recover(); r != nil {
					s.panics.Add(1)
					s.log.Error("cognee reflect goroutine panicked", "bank", bank, "panic", fmt.Sprintf("%v", r))
				}
			}()

			s.log.Info("goroutine_started", "name", "cognee_reflect", "bank", bank)
			defer s.log.Info("goroutine_stopped", "name", "cognee_reflect", "bank", bank)

			detachedCtx, cancel := context.WithTimeout(s.cogneeCtx, s.config.BackendReflectTimeout)
			defer cancel()

			_, err := s.backend.Reflect(detachedCtx, bank, a.Query)
			if err != nil {
				s.log.Error("cognee reflect failed", "bank", bank, logger.Error(err))
				s.metrics.errorCalls.Inc()
			}
		}()

		s.log.Info("reflect_queued", "bank", bank)
		s.mcpToolResult(sid, id, fmt.Sprintf(`{"status":"queued","bank":"%s"}`, bank))
		logReq("ok", nil)

	case "memory_improve":
		s.handleImprove(sid, id, bank, logReq)

	case "memory_forget":
		s.handleForget(sid, id, bank, c.Arguments, logReq)

	case "memory_retain_status":
		s.handleRetainStatus(sid, id, c.Arguments, logReq)

	default:
		s.mcpError(sid, id, -32601, fmt.Sprintf("unknown tool: %s", c.Name))
		logReq("", fmt.Errorf("unknown tool: %s", c.Name))
	}
}

func initResponse() map[string]interface{} {
	return map[string]interface{}{"protocolVersion": "2024-11-05", "capabilities": map[string]interface{}{"tools": map[string]interface{}{}}, "serverInfo": map[string]interface{}{"name": "mcp-memory", "version": "2.0.0"}}
}

func (s *Server) checkAuth(r *http.Request) bool {
	token := s.config.AuthToken
	if token == "" { return true } // No token configured = open access
	return r.Header.Get("Authorization") == "Bearer "+token
}

// toolsList returns the available MCP tools. Adapts per backend.
func (s *Server) toolsList() map[string]interface{} {
	tools := []map[string]interface{}{
		{"name": "memory_retain", "description": "Store information in long-term memory", "inputSchema": map[string]interface{}{"type": "object", "properties": map[string]interface{}{"content": map[string]interface{}{"type": "string"}}, "required": []string{"content"}}},
		{"name": "memory_recall", "description": "Search memory using semantic search", "inputSchema": map[string]interface{}{"type": "object", "properties": map[string]interface{}{"query": map[string]interface{}{"type": "string"}}, "required": []string{"query"}}},
		{"name": "memory_reflect", "description": "Synthesize memories for insights", "inputSchema": map[string]interface{}{"type": "object", "properties": map[string]interface{}{"query": map[string]interface{}{"type": "string"}}, "required": []string{"query"}}},
	}
	// memory_improve always available
	tools = append(tools, map[string]interface{}{
		"name":        "memory_improve",
		"description":  "Improve memory graph for a dataset",
		"inputSchema": map[string]interface{}{"type": "object", "properties": map[string]interface{}{}, "required": []string{}},
	})
	// Cognee-only tools
	if !s.backend.IsSync() {
		tools = append(tools,
			map[string]interface{}{
				"name":        "memory_forget",
				"description":  "Remove a specific memory from storage",
				"inputSchema": map[string]interface{}{"type": "object", "properties": map[string]interface{}{"content_id": map[string]interface{}{"type": "string"}}, "required": []string{"content_id"}},
			},
			map[string]interface{}{
				"name":        "memory_retain_status",
				"description":  "Check the status of an async retain job",
				"inputSchema": map[string]interface{}{"type": "object", "properties": map[string]interface{}{"job_id": map[string]interface{}{"type": "string"}}, "required": []string{"job_id"}},
			},
		)
	}
	return map[string]interface{}{"tools": tools}
}

// handleImprove triggers graph improvement for the given bank.
// Routes through the dedicated improve worker pool (AC-M6.8).
func (s *Server) handleImprove(sid string, id interface{}, bank string, logReq func(string, error)) {
	s.log.Info("memory_improve called", "bank", bank)
	s.metrics.improveTotal.Inc()

	if s.backend.IsSync() {
		// Hindsight: direct call with timeout
		ctx, cancel := context.WithTimeout(context.Background(), s.config.BackendReflectTimeout)
		defer cancel()
		r, err := s.backend.Reflect(ctx, bank, "")
		if err != nil {
			s.mcpError(sid, id, -32000, err.Error())
			logReq("", err)
			return
		}
		s.mcpToolResult(sid, id, r)
		logReq("ok", nil)
		return
	}

	// Cognee: push to dedicated improveJobs channel and wait for result
	ctx, cancel := context.WithTimeout(context.Background(), s.config.BackendReflectTimeout)
	defer cancel()

	resultCh := make(chan MemoryResult, 1)
	job := MemoryJob{Bank: bank, Method: "improve", Data: "", Result: resultCh, Ctx: ctx, Cancel: cancel}

	select {
	case s.workers.improveJobs <- job:
		// queued successfully
	default:
		s.mcpError(sid, id, -32000, "improve queue full")
		logReq("", fmt.Errorf("improve queue full"))
		return
	}

	select {
	case result := <-resultCh:
		if result.Err != nil {
			s.mcpError(sid, id, -32000, result.Err.Error())
			logReq("", result.Err)
			return
		}
		s.mcpToolResult(sid, id, result.Data)
		logReq("ok", nil)
	case <-ctx.Done():
		s.mcpError(sid, id, -32000, "improve operation timed out")
		logReq("", ctx.Err())
	}
}

// handleForget removes a specific memory from the backend.
func (s *Server) handleForget(sid string, id interface{}, bank string, args json.RawMessage, logReq func(string, error)) {
	var a struct{ ContentID string `json:"content_id"` }
	if err := json.Unmarshal(args, &a); err != nil || a.ContentID == "" {
		s.mcpError(sid, id, -32602, "content_id is required")
		logReq("", fmt.Errorf("missing content_id"))
		return
	}
	s.metrics.forgetTotal.Inc()
	r, err := s.backend.Forget(context.Background(), bank, a.ContentID)
	if err != nil {
		s.mcpError(sid, id, -32000, err.Error())
		logReq("", err)
		return
	}
	s.mcpToolResult(sid, id, r)
	logReq("ok", nil)
}

// handleRetainStatus checks the status of an async retain job.
func (s *Server) handleRetainStatus(sid string, id interface{}, args json.RawMessage, logReq func(string, error)) {
	var a struct{ JobID string `json:"job_id"` }
	if err := json.Unmarshal(args, &a); err != nil || a.JobID == "" {
		s.mcpError(sid, id, -32602, "job_id is required")
		logReq("", fmt.Errorf("missing job_id"))
		return
	}
	if s.jobTracker == nil {
		s.mcpError(sid, id, -32000, "job tracking not available with current backend")
		logReq("", fmt.Errorf("jobTracker nil"))
		return
	}
	result := s.jobTracker.get(a.JobID)
	if result == nil {
		s.mcpToolResult(sid, id, `{"status":"not_found"}`)
		logReq("not_found", nil)
		return
	}
	data, _ := json.Marshal(result)
	s.mcpToolResult(sid, id, string(data))
	logReq("ok", nil)
}

// fireErrorWebhook sends an error notification to the configured webhook URL.
// Non-blocking — runs in its own goroutine. Retries 3 times with exponential
// backoff (1s, 2s, 4s). Failures are logged at WARN level and never crash
// the server. If webhookURL is empty, this is a no-op.
func (s *Server) fireErrorWebhook(bank, jobID, errMsg, operation string) {
	url := s.config.ErrorWebhookURL
	if url == "" {
		return
	}
	go func() {
		defer func() {
			if r := recover(); r != nil {
				s.log.Error("error webhook goroutine panicked", "panic", fmt.Sprintf("%v", r))
			}
		}()
		s.log.Info("webhook_fired", "url", url, "bank", bank, "job_id", jobID, "operation", operation)

		payload := map[string]interface{}{
			"backend":   s.backend.Name(),
			"bank":      bank,
			"job_id":    jobID,
			"error":     errMsg,
			"timestamp": time.Now().UTC().Format(time.RFC3339),
			"operation": operation,
		}
		body, _ := json.Marshal(payload)

		client := &http.Client{Timeout: 10 * time.Second}
		var lastErr error
		for attempt := 0; attempt < 4; attempt++ {
			if attempt > 0 {
				backoff := time.Duration(1<<uint(attempt-1)) * time.Second // 1s, 2s, 4s
				time.Sleep(backoff)
			}
			resp, err := client.Post(url, "application/json", bytes.NewReader(body))
			if err != nil {
				lastErr = err
				s.log.Debug("webhook_retry_failed", "url", url, "job_id", jobID, "attempt", attempt+1, "error", err)
				continue
			}
			resp.Body.Close()
			if resp.StatusCode >= 200 && resp.StatusCode < 300 {
				return // success
			}
			lastErr = fmt.Errorf("HTTP status %d", resp.StatusCode)
			s.log.Debug("webhook_retry_failed", "url", url, "job_id", jobID, "attempt", attempt+1, "status", resp.StatusCode)
		}
		s.log.Warn("webhook_failed", "url", url, "job_id", jobID, "attempts", 4, "last_error", lastErr)
	}()
}
