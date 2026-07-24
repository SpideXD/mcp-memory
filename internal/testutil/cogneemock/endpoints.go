package cogneemock

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// handleHealth handles GET /health.
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeResponse(w, http.StatusMethodNotAllowed, `{"error":"method not allowed"}`)
		return
	}

	cfg := s.getResponse("/health")
	code := cfg.StatusCode
	if code == 0 {
		code = http.StatusOK
	}
	body := cfg.Body
	if body == "" {
		body = defaultResponses["/health"]
	}

	writeResponse(w, code, body)
}

// handleRemember handles POST /api/v1/remember (multipart form).
func (s *Server) handleRemember(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeResponse(w, http.StatusMethodNotAllowed, `{"error":"method not allowed"}`)
		return
	}

	cfg := s.getResponse("/api/v1/remember")
	code := cfg.StatusCode
	if code == 0 {
		code = http.StatusOK
	}
	body := cfg.Body
	if body == "" {
		body = defaultResponses["/api/v1/remember"]
	}

	writeResponse(w, code, body)
}

// handleRecall handles POST /api/v1/recall (JSON).
func (s *Server) handleRecall(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeResponse(w, http.StatusMethodNotAllowed, `{"error":"method not allowed"}`)
		return
	}

	cfg := s.getResponse("/api/v1/recall")
	code := cfg.StatusCode
	if code == 0 {
		code = http.StatusOK
	}
	body := cfg.Body
	if body == "" {
		// Try to extract query from body for dynamic default response
		var payload struct {
			Query string `json:"query"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err == nil && payload.Query != "" {
			body = fmt.Sprintf(`[{"_source":"mock","text":"mock recall result for %s"}]`, escapeJSON(payload.Query))
		} else {
			body = defaultResponses["/api/v1/recall"]
		}
	}

	writeResponse(w, code, body)
}

// handleImprove handles POST /api/v1/improve (JSON).
func (s *Server) handleImprove(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeResponse(w, http.StatusMethodNotAllowed, `{"error":"method not allowed"}`)
		return
	}

	cfg := s.getResponse("/api/v1/improve")
	code := cfg.StatusCode
	if code == 0 {
		code = http.StatusOK
	}
	body := cfg.Body
	if body == "" {
		body = defaultResponses["/api/v1/improve"]
	}

	writeResponse(w, code, body)
}

// handleForget handles POST /api/v1/forget (JSON).
func (s *Server) handleForget(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeResponse(w, http.StatusMethodNotAllowed, `{"error":"method not allowed"}`)
		return
	}

	cfg := s.getResponse("/api/v1/forget")
	code := cfg.StatusCode
	if code == 0 {
		code = http.StatusOK
	}
	body := cfg.Body
	if body == "" {
		body = defaultResponses["/api/v1/forget"]
	}

	writeResponse(w, code, body)
}

// handleRequests handles GET /_requests — returns the full capture log.
// Not overridable. Not captured.
func (s *Server) handleRequests(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeResponse(w, http.StatusMethodNotAllowed, `{"error":"method not allowed"}`)
		return
	}

	s.mu.RLock()
	out := make([]RequestLog, len(s.requests))
	copy(out, s.requests)
	s.mu.RUnlock()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(out)
}

// handleNotFound handles unknown endpoints — returns 404.
// Captured to request log.
func (s *Server) handleNotFound(w http.ResponseWriter, r *http.Request) {
	writeResponse(w, http.StatusNotFound, defaultResponses["/not_found"])
}

// escapeJSON escapes a string for safe embedding in a JSON string value.
func escapeJSON(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	s = strings.ReplaceAll(s, "\n", `\n`)
	s = strings.ReplaceAll(s, "\r", `\r`)
	s = strings.ReplaceAll(s, "\t", `\t`)
	return s
}
