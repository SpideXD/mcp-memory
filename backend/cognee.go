package backend

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var yearRE = regexp.MustCompile(`\b(19|20)\d{2}\b`)

// isClientError returns true if err represents an HTTP 4xx response.
// 4xx errors prove the backend is reachable — they should close the circuit,
// not trip it.
func isClientError(err error) bool {
	return err != nil && strings.Contains(err.Error(), "HTTP error (4")
}

// CogneeBackend implements the Backend interface for Cognee (Python and Rust).
// Both variants expose identical REST APIs — only the subprocess binary differs.
type CogneeBackend struct {
	baseURL         string
	httpClient      *http.Client
	breaker         *CircuitBreaker
	retainTimeout   time.Duration
	recallTimeout   time.Duration
	reflectTimeout  time.Duration
	retryAttempts   int
	retryDelay      time.Duration
	retryMaxDelay   time.Duration
	temporalCognify bool
	memoryOnly      bool
}

// Compile-time interface assertion.
var _ Backend = (*CogneeBackend)(nil)

func newCogneeBackend(cfg BackendConfig) *CogneeBackend {
	// Use CogneeRetainTimeout (default 900s) for the HTTP client timeout,
	// NOT BackendRetainTimeout (60s default). Cognee retains run a full LLM
	// pipeline and take 20-30s on average, minutes in the worst case
	// (see docs/benchmarks.md). http.Client.Timeout bounds the entire
	// exchange and overrides the per-request context deadline, so setting it
	// too low makes every slow retain fail, retry, and trip the breaker.
	clientTimeout := cfg.BackendRetainTimeout
	if cfg.CogneeRetainTimeout > 0 {
		clientTimeout = cfg.CogneeRetainTimeout
	}
	return &CogneeBackend{
		baseURL:         fmt.Sprintf("http://localhost:%s", cfg.CogneePort),
		httpClient:      &http.Client{Timeout: clientTimeout},
		breaker:         NewCircuitBreaker(5, 30*time.Second),
		retainTimeout:   clientTimeout,
		recallTimeout:   cfg.BackendRecallTimeout,
		reflectTimeout:  clientTimeout,
		retryAttempts:   cfg.RetryAttempts,
		retryDelay:      cfg.RetryDelay,
		retryMaxDelay:   cfg.RetryMaxDelay,
		temporalCognify: cfg.TemporalCognify,
		memoryOnly:      cfg.MemoryOnly,
	}
}

// Health checks Cognee API connectivity. GET /health
func (c *CogneeBackend) Health(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, "GET", c.baseURL+"/health", nil)
	if err != nil {
		return err
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("Cognee health check: status %d", resp.StatusCode)
	}
	return nil
}

// Retain stores content in Cognee. POST /api/v1/remember (multipart form).
// datasetName=bank, content=content (as file field).
// Blocks 20-30s typically while Cognee runs the LLM pipeline, minutes worst case.
func (c *CogneeBackend) Retain(ctx context.Context, bank string, content string) (string, error) {
	if c.breaker.IsTripped() {
		return "", fmt.Errorf("Cognee circuit breaker open — service unavailable")
	}
	outcomeRecorded := false
	defer func() {
		if !outcomeRecorded {
			c.breaker.RecordFailure()
		}
	}()

	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)
	_ = writer.WriteField("datasetName", bank)
	_ = writer.WriteField("temporalCognify", strconv.FormatBool(c.temporalCognify))

	if !yearRE.MatchString(content) {
		content = content + " [" + time.Now().Format("2006-01-02") + "]"
	}
	part, err := writer.CreateFormFile("data", "data.txt")
	if err != nil {
		return "", fmt.Errorf("create form file: %w", err)
	}
	if _, err := io.WriteString(part, content); err != nil {
		return "", fmt.Errorf("write content: %w", err)
	}
	writer.Close()

	u := fmt.Sprintf("%s/api/v1/remember", c.baseURL)
	req, _ := http.NewRequestWithContext(ctx, "POST", u, &buf)
	req.Header.Set("Content-Type", writer.FormDataContentType())

	body, err := doRequest(c.httpClient, req, c.retainTimeout, c.retryAttempts, c.retryDelay, c.retryMaxDelay)
	if err != nil {
		if isClientError(err) {
			// 4xx proves the backend is reachable — close the circuit
			c.breaker.RecordSuccess()
			outcomeRecorded = true
		}
		return "", err
	}
	c.breaker.RecordSuccess()
	outcomeRecorded = true
	return string(body), nil
}

// Recall searches memory in Cognee. POST /api/v1/recall (JSON).
func (c *CogneeBackend) Recall(ctx context.Context, bank string, query string) (string, error) {
	if c.breaker.IsTripped() {
		return "", fmt.Errorf("Cognee circuit breaker open — service unavailable")
	}
	outcomeRecorded := false
	defer func() {
		if !outcomeRecorded {
			c.breaker.RecordFailure()
		}
	}()

	payload := map[string]interface{}{
		"query":    query,
		"datasets": []string{bank},
	}
	data, _ := json.Marshal(payload)

	u := fmt.Sprintf("%s/api/v1/recall", c.baseURL)
	req, _ := http.NewRequestWithContext(ctx, "POST", u, bytes.NewReader(data))
	req.Header.Set("Content-Type", "application/json")

	body, err := doRequest(c.httpClient, req, c.recallTimeout, c.retryAttempts, c.retryDelay, c.retryMaxDelay)
	if err != nil {
		if isClientError(err) {
			c.breaker.RecordSuccess()
			outcomeRecorded = true
		}
		return "", err
	}
	c.breaker.RecordSuccess()
	outcomeRecorded = true
	return string(body), nil
}

// Reflect triggers Cognee's graph improvement. POST /api/v1/improve (JSON).
func (c *CogneeBackend) Reflect(ctx context.Context, bank string, query string) (string, error) {
	if c.breaker.IsTripped() {
		return "", fmt.Errorf("Cognee circuit breaker open — service unavailable")
	}
	outcomeRecorded := false
	defer func() {
		if !outcomeRecorded {
			c.breaker.RecordFailure()
		}
	}()

	payload := map[string]interface{}{
		"dataset_name": bank,
		"data":         query,
	}
	data, _ := json.Marshal(payload)

	u := fmt.Sprintf("%s/api/v1/improve", c.baseURL)
	req, _ := http.NewRequestWithContext(ctx, "POST", u, bytes.NewReader(data))
	req.Header.Set("Content-Type", "application/json")

	body, err := doRequest(c.httpClient, req, c.reflectTimeout, c.retryAttempts, c.retryDelay, c.retryMaxDelay)
	if err != nil {
		if isClientError(err) {
			c.breaker.RecordSuccess()
			outcomeRecorded = true
		}
		return "", err
	}
	c.breaker.RecordSuccess()
	outcomeRecorded = true
	return string(body), nil
}

// Forget removes a specific memory from Cognee. POST /api/v1/forget (JSON).
func (c *CogneeBackend) Forget(ctx context.Context, bank string, contentID string) (string, error) {
	if c.breaker.IsTripped() {
		return "", fmt.Errorf("Cognee circuit breaker open — service unavailable")
	}
	outcomeRecorded := false
	defer func() {
		if !outcomeRecorded {
			c.breaker.RecordFailure()
		}
	}()

	payload := map[string]interface{}{
		"dataset":     bank,
		"data_id":     contentID,
		"memory_only": c.memoryOnly,
	}
	data, _ := json.Marshal(payload)

	u := fmt.Sprintf("%s/api/v1/forget", c.baseURL)
	req, _ := http.NewRequestWithContext(ctx, "POST", u, bytes.NewReader(data))
	req.Header.Set("Content-Type", "application/json")

	body, err := doRequest(c.httpClient, req, c.recallTimeout, c.retryAttempts, c.retryDelay, c.retryMaxDelay)
	if err != nil {
		if isClientError(err) {
			c.breaker.RecordSuccess()
			outcomeRecorded = true
		}
		return "", err
	}
	c.breaker.RecordSuccess()
	outcomeRecorded = true
	return string(body), nil
}

// Name returns "cognee".
func (c *CogneeBackend) Name() string { return "cognee" }
