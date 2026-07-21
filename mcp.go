package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"mcp-memory/logger"
)

func (s *Server) mcpResult(sid string, id, result interface{}) {
	s.writeSSE(sid, id, "result", result)
}

func (s *Server) mcpError(sid string, id interface{}, code int, msg string) {
	s.writeSSE(sid, id, "error", map[string]interface{}{"code": code, "message": msg})
}

func (s *Server) mcpToolResult(sid string, id interface{}, text string) {
	s.mcpResult(sid, id, map[string]interface{}{"content": []map[string]interface{}{{"type": "text", "text": text}}})
}

func newSessionID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func (s *Server) writeSSE(sid string, id interface{}, key string, val interface{}) {
	s.sessionsMu.RLock()
	sess, ok := s.sessions[sid]
	s.sessionsMu.RUnlock()
	if !ok || sess.IsClosed() {
		return
	}
	data, err := json.Marshal(map[string]interface{}{"jsonrpc": "2.0", "id": id, key: val})
	if err != nil {
		s.log.Error("SSE marshal failed", "session", sid[:8], logger.Error(err))
		return
	}
	msg := string(data)
	select {
	case sess.SSEChannel <- msg:
	default:
		// Track drop count in metrics
		s.metrics.sseDrops.Inc()
		// Log the dropped message content for debugging
		s.log.Warn("dropped SSE message",
			"session", sid[:8],
			"key", key,
			"message_size", len(msg),
			"buffer_cap", cap(sess.SSEChannel),
		)
		s.alerts.Send(AlertWarn, "SSE buffer full — agent too slow", map[string]interface{}{
			"session": sid[:8],
			"key":     key,
			"drops":   s.metrics.sseDrops.Value(),
		})
		// Write error to channel on next attempt
		errMsg := fmt.Sprintf(`{"jsonrpc":"2.0","id":%v,"error":{"code":-32000,"message":"SSE buffer full, agent too slow"}}`, id)
		select {
		case sess.SSEChannel <- errMsg:
		default:
		}
	}
}
