package backend

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"time"
)

// HindsightBackend implements the Backend interface for the Hindsight API.
// All HTTP logic is identical to the original hindsight.go — zero functional changes.
type HindsightBackend struct {
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
var _ Backend = (*HindsightBackend)(nil)

func newHindsightBackend(cfg BackendConfig) *HindsightBackend {
	return &HindsightBackend{
		baseURL:        fmt.Sprintf("http://localhost:%s", cfg.HindsightPort),
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

// Name returns "hindsight".
func (h *HindsightBackend) Name() string { return "hindsight" }

// IsSync returns true — Hindsight operations complete inline.
func (h *HindsightBackend) IsSync() bool { return true }

// Health checks Hindsight API connectivity.
func (h *HindsightBackend) Health(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, "GET", h.baseURL+"/health", nil)
	if err != nil {
		return err
	}
	resp, err := h.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("Hindsight health check: status %d", resp.StatusCode)
	}
	return nil
}

// Retain stores content in Hindsight. POST /v1/default/banks/{bank}/memories
func (h *HindsightBackend) Retain(ctx context.Context, bank string, content string) (string, error) {
	if h.breaker.IsTripped() {
		return "", fmt.Errorf("Hindsight circuit breaker open — service unavailable")
	}
	u := fmt.Sprintf("%s/v1/default/banks/%s/memories",
		h.baseURL, url.PathEscape(bank))
	payload := map[string]interface{}{"items": []map[string]string{{"content": content}}}
	data, _ := json.Marshal(payload)
	req, _ := http.NewRequestWithContext(ctx, "POST", u, bytes.NewReader(data))
	req.Header.Set("Content-Type", "application/json")
	body, err := doRequest(h.httpClient, req, h.retainTimeout, h.retryAttempts, h.retryDelay, h.retryMaxDelay)
	if err != nil {
		h.breaker.RecordFailure()
		return "", err
	}
	h.breaker.RecordSuccess()
	return string(body), nil
}

// Recall searches memory in Hindsight. POST /v1/default/banks/{bank}/memories/recall
func (h *HindsightBackend) Recall(ctx context.Context, bank string, query string) (string, error) {
	if h.breaker.IsTripped() {
		return "", fmt.Errorf("Hindsight circuit breaker open — service unavailable")
	}
	u := fmt.Sprintf("%s/v1/default/banks/%s/memories/recall",
		h.baseURL, url.PathEscape(bank))
	payload := map[string]interface{}{"query": query}
	data, _ := json.Marshal(payload)
	req, _ := http.NewRequestWithContext(ctx, "POST", u, bytes.NewReader(data))
	req.Header.Set("Content-Type", "application/json")
	body, err := doRequest(h.httpClient, req, h.recallTimeout, h.retryAttempts, h.retryDelay, h.retryMaxDelay)
	if err != nil {
		h.breaker.RecordFailure()
		return "", err
	}
	h.breaker.RecordSuccess()
	return string(body), nil
}

// Reflect synthesizes memories in Hindsight. POST /v1/default/banks/{bank}/reflect
func (h *HindsightBackend) Reflect(ctx context.Context, bank string, query string) (string, error) {
	if h.breaker.IsTripped() {
		return "", fmt.Errorf("Hindsight circuit breaker open — service unavailable")
	}
	u := fmt.Sprintf("%s/v1/default/banks/%s/reflect",
		h.baseURL, url.PathEscape(bank))
	payload := map[string]interface{}{"query": query}
	data, _ := json.Marshal(payload)
	req, _ := http.NewRequestWithContext(ctx, "POST", u, bytes.NewReader(data))
	req.Header.Set("Content-Type", "application/json")
	body, err := doRequest(h.httpClient, req, h.reflectTimeout, h.retryAttempts, h.retryDelay, h.retryMaxDelay)
	if err != nil {
		h.breaker.RecordFailure()
		return "", err
	}
	h.breaker.RecordSuccess()
	return string(body), nil
}

// Forget returns ErrNotSupported — Hindsight does not support selective forgetting.
func (h *HindsightBackend) Forget(_ context.Context, _, _ string) (string, error) {
	return "", ErrNotSupported
}
