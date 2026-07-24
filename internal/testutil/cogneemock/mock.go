package cogneemock

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
)

// ResponseConfig overrides the default response for a specific endpoint path.
// Zero-value fields mean "use default."
type ResponseConfig struct {
	StatusCode int    // 0 = default (200)
	Body       string // "" = default response body for that endpoint
}

// Server wraps httptest.Server with request capture and configurable responses.
// Safe for concurrent use.
type Server struct {
	httpServer *httptest.Server
	mu         sync.RWMutex
	requests   []RequestLog
	responses  map[string]ResponseConfig // key = endpoint path, e.g. "/api/v1/improve"
}

// NewServer creates and starts the mock Cognee server.
// Returns immediately (httptest.Server starts in background).
func NewServer() *Server {
	s := &Server{
		responses: make(map[string]ResponseConfig),
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/health", s.captureMiddleware(s.handleHealth))
	mux.HandleFunc("/_requests", s.handleRequests) // not captured
	mux.HandleFunc("/api/v1/remember", s.captureMiddleware(s.handleRemember))
	mux.HandleFunc("/api/v1/recall", s.captureMiddleware(s.handleRecall))
	mux.HandleFunc("/api/v1/improve", s.captureMiddleware(s.handleImprove))
	mux.HandleFunc("/api/v1/forget", s.captureMiddleware(s.handleForget))
	mux.HandleFunc("/", s.captureMiddleware(s.handleNotFound))

	s.httpServer = httptest.NewServer(mux)
	return s
}

// URL returns the base URL of the mock server, e.g. "http://127.0.0.1:PORT".
func (s *Server) URL() string {
	return s.httpServer.URL
}

// Port returns the port number as an int (for CogneePort string conversion).
func (s *Server) Port() int {
	_, portStr, err := net.SplitHostPort(s.httpServer.Listener.Addr().String())
	if err != nil {
		return 0
	}
	port, _ := strconv.Atoi(portStr)
	return port
}

// Close shuts down the mock server. Idempotent.
func (s *Server) Close() {
	s.httpServer.Close()
}

// SetResponse overrides the default response for a given endpoint path.
// Thread-safe. Pass a zero-value ResponseConfig to reset to default.
func (s *Server) SetResponse(endpoint string, cfg ResponseConfig) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if cfg.StatusCode == 0 && cfg.Body == "" {
		delete(s.responses, endpoint)
	} else {
		s.responses[endpoint] = cfg
	}
}

// Requests returns a copy of all captured requests. Thread-safe.
func (s *Server) Requests() []RequestLog {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]RequestLog, len(s.requests))
	copy(out, s.requests)
	return out
}

// ResetRequests clears the request capture buffer. Thread-safe.
func (s *Server) ResetRequests() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requests = nil
}

// LastRequest returns the most recent request to the given path, or nil.
// Thread-safe.
func (s *Server) LastRequest(path string) *RequestLog {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for i := len(s.requests) - 1; i >= 0; i-- {
		if s.requests[i].Path == path {
			entry := s.requests[i]
			return &entry
		}
	}
	return nil
}

// getResponse returns the response config for an endpoint, or defaults.
func (s *Server) getResponse(endpoint string) ResponseConfig {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if cfg, ok := s.responses[endpoint]; ok {
		return cfg
	}
	return ResponseConfig{}
}

// defaultResponseBody returns the default JSON response body for common endpoints.
var defaultResponses = map[string]string{
	"/health":            `{"status":"ready","health":"healthy","version":"0.1.0"}`,
	"/api/v1/remember":   `{"status":"completed","pipeline_run_id":"mock-run-001","dataset_id":"mock-ds-001","dataset_name":"test","items_processed":1,"elapsed_seconds":0.1}`,
	"/api/v1/recall":     `[{"_source":"mock","text":"mock recall result for query"}]`,
	"/api/v1/improve":    `{"status":"PipelineRunCompleted","pipeline_run_id":"mock-improve-001","dataset_id":"mock-ds-001","dataset_name":"test"}`,
	"/api/v1/forget":     `{"dataset_id":"mock-ds-001","status":"success"}`,
	"/not_found":         `{"error":"not found"}`,
}

// writeResponse writes an HTTP response with the given status code and body.
func writeResponse(w http.ResponseWriter, statusCode int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	fmt.Fprint(w, body)
}
