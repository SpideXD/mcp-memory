// Package testutil provides a reusable MCP client for stress and e2e testing.
package testutil

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// DefaultServerURL is the default MCP memory server address.
const DefaultServerURL = "http://localhost:8899"

// DefaultTimeout is the default JSON-RPC call timeout.
const DefaultTimeout = 120 * time.Second

// Response carries both the JSON-RPC result and error from a response.
type Response struct {
	Result json.RawMessage `json:"result,omitempty"`
	Error  json.RawMessage `json:"error,omitempty"`
}

// AgentError records an error from a specific agent during concurrent operations.
type AgentError struct {
	Agent int
	Op    string
	Err   error
}

// AgentResult records timing for a specific agent's operation.
type AgentResult struct {
	Agent int
	Op    string
	Dur   time.Duration
}

// Client is a reusable MCP SSE + JSON-RPC client.
type Client struct {
	baseURL     string
	sessionID   string
	msgURL      string
	msgID       int
	msgIDMu     sync.Mutex
	httpClient  *http.Client
	sseBody     io.ReadCloser
	responses   map[int]chan Response
	responsesMu sync.Mutex
	closed      atomic.Bool
}

// ServerUp returns true if the MCP memory server is reachable AND running.
func ServerUp() bool {
	c := &http.Client{Timeout: 500 * time.Millisecond}
	resp, err := c.Get(DefaultServerURL + "/health")
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return false
	}
	var health struct {
		Status string `json:"status"`
	}
	json.NewDecoder(resp.Body).Decode(&health)
	return health.Status == "running"
}

// Min returns the smaller of a or b.
func Min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// NewClient creates a new MCP client connected to the given base URL and bank.
func NewClient(baseURL, bank string) (*Client, error) {
	c := &Client{
		baseURL:    baseURL,
		httpClient: &http.Client{Timeout: DefaultTimeout},
		responses:  make(map[int]chan Response),
	}
	sseURL := fmt.Sprintf("%s/mcp/sse?bank=%s", baseURL, bank)
	return c.ConnectSSE(sseURL)
}

// ConnectSSE establishes an SSE connection and extracts the session ID.
func (c *Client) ConnectSSE(sseURL string) (*Client, error) {
	req, _ := http.NewRequest("GET", sseURL, nil)
	req.Header.Set("Accept", "text/event-stream")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("SSE connect: %w", err)
	}

	// Read endpoint event: "event: endpoint\ndata: /mcp/message?session_id=xxx\n\n"
	buf := make([]byte, 4096)
	n, err := resp.Body.Read(buf)
	if err != nil || n == 0 {
		resp.Body.Close()
		return nil, fmt.Errorf("read SSE endpoint: %w", err)
	}

	response := string(buf[:n])
	if !strings.Contains(response, "event: endpoint") {
		resp.Body.Close()
		return nil, fmt.Errorf("no endpoint event in SSE response: %s", response[:Min(len(response), 200)])
	}

	// Extract session_id from data line
	lines := strings.Split(response, "\n")
	for _, line := range lines {
		if strings.HasPrefix(line, "data: ") {
			data := strings.TrimPrefix(line, "data: ")
			if strings.Contains(data, "session_id=") {
				idx := strings.Index(data, "session_id=")
				c.sessionID = data[idx+len("session_id="):]
				break
			}
		}
	}

	if c.sessionID == "" {
		resp.Body.Close()
		return nil, fmt.Errorf("could not extract session_id from SSE response")
	}

	c.sseBody = resp.Body
	go c.readSSE()

	c.msgURL = fmt.Sprintf("%s/mcp/message?session_id=%s", c.baseURL, c.sessionID)
	return c, nil
}

// readSSE reads the SSE stream and routes JSON-RPC responses by request ID.
// Runs as a background goroutine. Panics are recovered to prevent crashes.
func (c *Client) readSSE() {
	defer func() {
		if r := recover(); r != nil {
			// SSE reader panicked; the caller detects this via CallJSONRPC timeout.
		}
	}()
	defer c.sseBody.Close()

	scanner := bufio.NewScanner(c.sseBody)
	scanner.Buffer(make([]byte, 1<<20), 1<<20)

	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "data: ") {
			currentData := strings.TrimPrefix(line, "data: ")
			var msg struct {
				ID     int             `json:"id"`
				Result json.RawMessage `json:"result"`
				Error  json.RawMessage `json:"error"`
			}
			if err := json.Unmarshal([]byte(currentData), &msg); err != nil || msg.ID == 0 {
				continue
			}
			c.responsesMu.Lock()
			ch, ok := c.responses[msg.ID]
			if ok {
				delete(c.responses, msg.ID)
			}
			c.responsesMu.Unlock()
			if ok {
				resp := Response{Result: msg.Result}
				if len(msg.Error) > 0 && string(msg.Error) != "null" {
					resp.Error = msg.Error
				}
				ch <- resp
			}
		}
	}
}

// CallJSONRPC sends a JSON-RPC request and waits for the SSE response.
func (c *Client) CallJSONRPC(method string, params interface{}) (string, error) {
	c.msgIDMu.Lock()
	c.msgID++
	id := c.msgID
	c.msgIDMu.Unlock()

	body := map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  method,
	}
	if params != nil {
		body["params"] = params
	}

	// Register response channel BEFORE sending
	ch := make(chan Response, 1)
	c.responsesMu.Lock()
	c.responses[id] = ch
	c.responsesMu.Unlock()

	defer func() {
		c.responsesMu.Lock()
		delete(c.responses, id)
		c.responsesMu.Unlock()
	}()

	data, _ := json.Marshal(body)
	resp, err := c.httpClient.Post(c.msgURL, "application/json", strings.NewReader(string(data)))
	if err != nil {
		return "", fmt.Errorf("MCP call %s: %w", method, err)
	}
	resp.Body.Close()
	if resp.StatusCode != 202 {
		return "", fmt.Errorf("MCP call %s: status %d", method, resp.StatusCode)
	}

	select {
	case resp := <-ch:
		if resp.Error != nil {
			return string(resp.Result), fmt.Errorf("JSON-RPC error: %s", string(resp.Error))
		}
		return string(resp.Result), nil
	case <-time.After(DefaultTimeout):
		return "", fmt.Errorf("MCP call %s: timeout waiting for SSE response", method)
	}
}

// SessionID returns the MCP session ID for this client.
func (c *Client) SessionID() string {
	return c.sessionID
}

// Close closes the SSE connection and marks the client as closed.
func (c *Client) Close() {
	if c.closed.Swap(true) {
		return
	}
	if c.sseBody != nil {
		c.sseBody.Close()
	}
}

// Initialize sends the MCP initialize request.
func (c *Client) Initialize() error {
	_, err := c.CallJSONRPC("initialize", nil)
	return err
}

// ListTools requests the tool list from the server.
func (c *Client) ListTools() ([]string, error) {
	_, err := c.CallJSONRPC("tools/list", nil)
	return nil, err
}

// Recall performs a memory_recall tool call.
func (c *Client) Recall(query string) (string, error) {
	params := map[string]interface{}{
		"name":      "memory_recall",
		"arguments": map[string]interface{}{"query": query},
	}
	return c.CallJSONRPC("tools/call", params)
}

// Retain performs a memory_retain tool call.
func (c *Client) Retain(content string) (string, error) {
	params := map[string]interface{}{
		"name":      "memory_retain",
		"arguments": map[string]interface{}{"content": content},
	}
	return c.CallJSONRPC("tools/call", params)
}

// Reflect performs a memory_reflect tool call.
func (c *Client) Reflect(query string) (string, error) {
	params := map[string]interface{}{
		"name":      "memory_reflect",
		"arguments": map[string]interface{}{"query": query},
	}
	return c.CallJSONRPC("tools/call", params)
}
