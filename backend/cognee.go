package backend

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"time"
)

// CogneeBackend implements the Backend interface for Cognee (Python and Rust).
// Both variants expose identical REST APIs — only the subprocess binary differs.
type CogneeBackend struct {
	baseURL        string
	httpClient     *http.Client
	breaker        *CircuitBreaker
	retryAttempts  int
	retryDelay     time.Duration
	retryMaxDelay  time.Duration
	retainTimeout  time.Duration
	recallTimeout  time.Duration
	reflectTimeout time.Duration
}

// Compile-time interface assertion.
var _ Backend = (*CogneeBackend)(nil)

func newCogneeBackend(cfg BackendConfig) *CogneeBackend {
	return &CogneeBackend{
		baseURL:        fmt.Sprintf("http://localhost:%s", cfg.CogneePort),
		httpClient:     &http.Client{Timeout: cfg.BackendRetainTimeout},
		breaker:        NewCircuitBreaker(cfg.CircuitBreakerThreshold, cfg.CircuitBreakerCooldown),
		retryAttempts:  cfg.RetryAttempts,
		retryDelay:     cfg.RetryDelay,
		retryMaxDelay:  cfg.RetryMaxDelay,
		retainTimeout:  cfg.BackendRetainTimeout,
		recallTimeout:  cfg.BackendRecallTimeout,
		reflectTimeout: cfg.BackendReflectTimeout,
	}
}

// Name returns "cognee".
func (c *CogneeBackend) Name() string { return "cognee" }

// IsSync returns false — Cognee operations are dispatched in detached goroutines.
func (c *CogneeBackend) IsSync() bool { return false }

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
// Blocks 2-10 minutes while Cognee processes LLM pipeline.
func (c *CogneeBackend) Retain(ctx context.Context, bank string, content string) (string, error) {
	if c.breaker.IsTripped() {
		return "", fmt.Errorf("Cognee circuit breaker open — service unavailable")
	}

	// Build multipart form body
	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)
	_ = writer.WriteField("datasetName", bank)
	part, err := writer.CreateFormFile("content", "data.txt")
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
		c.breaker.RecordFailure()
		return "", err
	}
	c.breaker.RecordSuccess()
	return string(body), nil
}

// Recall searches memory in Cognee. POST /api/v1/recall (JSON).
func (c *CogneeBackend) Recall(ctx context.Context, bank string, query string) (string, error) {
	if c.breaker.IsTripped() {
		return "", fmt.Errorf("Cognee circuit breaker open — service unavailable")
	}

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
		c.breaker.RecordFailure()
		return "", err
	}
	c.breaker.RecordSuccess()
	return string(body), nil
}

// Reflect triggers Cognee's graph improvement. POST /api/v1/improve (JSON).
// Empty query triggers a full dataset improvement.
func (c *CogneeBackend) Reflect(ctx context.Context, bank string, query string) (string, error) {
	if c.breaker.IsTripped() {
		return "", fmt.Errorf("Cognee circuit breaker open — service unavailable")
	}

	payload := map[string]interface{}{
		"dataset_name": bank,
		"query":        query,
	}
	data, _ := json.Marshal(payload)

	u := fmt.Sprintf("%s/api/v1/improve", c.baseURL)
	req, _ := http.NewRequestWithContext(ctx, "POST", u, bytes.NewReader(data))
	req.Header.Set("Content-Type", "application/json")

	body, err := doRequest(c.httpClient, req, c.reflectTimeout, c.retryAttempts, c.retryDelay, c.retryMaxDelay)
	if err != nil {
		c.breaker.RecordFailure()
		return "", err
	}
	c.breaker.RecordSuccess()
	return string(body), nil
}

// Forget removes a specific memory from Cognee. POST /api/v1/forget (JSON).
// memory_only=true preserves the graph structure.
func (c *CogneeBackend) Forget(ctx context.Context, bank string, contentID string) (string, error) {
	if c.breaker.IsTripped() {
		return "", fmt.Errorf("Cognee circuit breaker open — service unavailable")
	}

	payload := map[string]interface{}{
		"dataset":     bank,
		"content_id":  contentID,
		"memory_only": true,
	}
	data, _ := json.Marshal(payload)

	u := fmt.Sprintf("%s/api/v1/forget", c.baseURL)
	req, _ := http.NewRequestWithContext(ctx, "POST", u, bytes.NewReader(data))
	req.Header.Set("Content-Type", "application/json")

	body, err := doRequest(c.httpClient, req, c.recallTimeout, c.retryAttempts, c.retryDelay, c.retryMaxDelay)
	if err != nil {
		c.breaker.RecordFailure()
		return "", err
	}
	c.breaker.RecordSuccess()
	return string(body), nil
}
