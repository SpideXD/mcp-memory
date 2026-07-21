package main

import (
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
		s.mcpResult(sid, id, toolsList())
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
		r, err := s.recallAPI(bank, a.Query)
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
		r, err := s.queueJob(s.workers.retainJobs, bank, "retain", a.Content)
		if err != nil { s.mcpError(sid, id, -32000, err.Error()); logReq("", err); return }
		s.mcpToolResult(sid, id, r.Data)
		logReq("ok", nil)
	case "memory_reflect":
		var a struct{ Query string `json:"query"` }
		if err := json.Unmarshal(c.Arguments, &a); err != nil || a.Query == "" {
			s.mcpError(sid, id, -32602, "query is required")
			logReq("", fmt.Errorf("missing query"))
			return
		}
		s.metrics.reflectCalls.Inc()
		r, err := s.queueJob(s.workers.reflectJobs, bank, "reflect", a.Query)
		if err != nil { s.mcpError(sid, id, -32000, err.Error()); logReq("", err); return }
		s.mcpToolResult(sid, id, r.Data)
		logReq("ok", nil)
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

func toolsList() map[string]interface{} {
	return map[string]interface{}{"tools": []map[string]interface{}{
		{"name": "memory_retain", "description": "Store information in long-term memory", "inputSchema": map[string]interface{}{"type": "object", "properties": map[string]interface{}{"content": map[string]interface{}{"type": "string"}}, "required": []string{"content"}}},
		{"name": "memory_recall", "description": "Search memory using semantic search", "inputSchema": map[string]interface{}{"type": "object", "properties": map[string]interface{}{"query": map[string]interface{}{"type": "string"}}, "required": []string{"query"}}},
		{"name": "memory_reflect", "description": "Synthesize memories for insights", "inputSchema": map[string]interface{}{"type": "object", "properties": map[string]interface{}{"query": map[string]interface{}{"type": "string"}}, "required": []string{"query"}}},
	}}
}
