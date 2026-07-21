package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sync"
	"time"
)

// circuitBreaker tracks failures and fails fast when threshold is exceeded.
type circuitBreaker struct {
	mu           sync.Mutex
	failures     int
	threshold    int
	cooldown     time.Duration
	lastFailure  time.Time
	trippedUntil time.Time
}

func newCircuitBreaker(threshold int, cooldown time.Duration) *circuitBreaker {
	return &circuitBreaker{
		threshold: threshold,
		cooldown:  cooldown,
	}
}

// RecordFailure increments the failure count and trips the breaker if threshold reached.
func (cb *circuitBreaker) RecordFailure() {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	cb.failures++
	cb.lastFailure = time.Now()
	if cb.failures >= cb.threshold {
		cb.trippedUntil = time.Now().Add(cb.cooldown)
	}
}

// RecordSuccess resets the failure count.
func (cb *circuitBreaker) RecordSuccess() {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	cb.failures = 0
	cb.trippedUntil = time.Time{}
}

// IsTripped returns true if the circuit breaker is active (failing fast).
func (cb *circuitBreaker) IsTripped() bool {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	if cb.trippedUntil.IsZero() {
		return false
	}
	if time.Now().After(cb.trippedUntil) {
		// Cooldown expired — allow one request through
		cb.trippedUntil = time.Time{}
		cb.failures = 0
		return false
	}
	return true
}

func (s *Server) retainAPI(bank, content string) (string, error) {
	return s.retainAPIWithContext(context.Background(), bank, content)
}

func (s *Server) retainAPIWithContext(ctx context.Context, bank, content string) (string, error) {
	if s.hindsightBreaker.IsTripped() {
		return "", fmt.Errorf("Hindsight circuit breaker open — service unavailable")
	}
	u := fmt.Sprintf("http://localhost:%s/v1/default/banks/%s/memories",
		s.config.HindsightPort, url.PathEscape(bank))
	payload := map[string]interface{}{"items": []map[string]string{{"content": content}}}
	data, _ := json.Marshal(payload)
	req, _ := http.NewRequestWithContext(ctx, "POST", u, bytes.NewReader(data))
	req.Header.Set("Content-Type", "application/json")
	body, err := s.doRequest(req, s.config.HindsightRetainTimeout)
	if err != nil {
		s.hindsightBreaker.RecordFailure()
		return "", err
	}
	s.hindsightBreaker.RecordSuccess()
	return string(body), nil
}

func (s *Server) recallAPI(bank, query string) (string, error) {
	if s.hindsightBreaker.IsTripped() {
		return "", fmt.Errorf("Hindsight circuit breaker open — service unavailable")
	}
	u := fmt.Sprintf("http://localhost:%s/v1/default/banks/%s/memories/recall",
		s.config.HindsightPort, url.PathEscape(bank))
	payload := map[string]interface{}{"query": query}
	data, _ := json.Marshal(payload)
	req, _ := http.NewRequest("POST", u, bytes.NewReader(data))
	req.Header.Set("Content-Type", "application/json")
	body, err := s.doRequest(req, s.config.HindsightRecallTimeout)
	if err != nil {
		s.hindsightBreaker.RecordFailure()
		return "", err
	}
	s.hindsightBreaker.RecordSuccess()
	return string(body), nil
}

func (s *Server) reflectAPI(bank, query string) (string, error) {
	return s.reflectAPIWithContext(context.Background(), bank, query)
}

func (s *Server) reflectAPIWithContext(ctx context.Context, bank, query string) (string, error) {
	if s.hindsightBreaker.IsTripped() {
		return "", fmt.Errorf("Hindsight circuit breaker open — service unavailable")
	}
	u := fmt.Sprintf("http://localhost:%s/v1/default/banks/%s/reflect",
		s.config.HindsightPort, url.PathEscape(bank))
	payload := map[string]interface{}{"query": query}
	data, _ := json.Marshal(payload)
	req, _ := http.NewRequestWithContext(ctx, "POST", u, bytes.NewReader(data))
	req.Header.Set("Content-Type", "application/json")
	body, err := s.doRequest(req, s.config.HindsightReflectTimeout)
	if err != nil {
		s.hindsightBreaker.RecordFailure()
		return "", err
	}
	s.hindsightBreaker.RecordSuccess()
	return string(body), nil
}

func (s *Server) doRequest(req *http.Request, timeout time.Duration) ([]byte, error) {
	if req.Body == nil {
		return nil, fmt.Errorf("request body is nil")
	}
	bodyBytes, err := io.ReadAll(req.Body)
	req.Body.Close()
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}

	ctx := req.Context()
	// Apply per-request timeout to context
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var lastErr error
	maxBody := int64(10 << 20) // 10MB response limit
	maxDelay := s.config.RetryMaxDelay
	if maxDelay <= 0 {
		maxDelay = 30 * time.Second
	}

	for attempt := 0; attempt < s.config.RetryAttempts; attempt++ {
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("request cancelled: %w", ctx.Err())
		default:
		}

		retryReq := req.Clone(ctx)
		retryReq.Body = io.NopCloser(bytes.NewReader(bodyBytes))
		retryReq.ContentLength = int64(len(bodyBytes))
		retryReq.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(bodyBytes)), nil }

		resp, err := s.svc.httpClient.Do(retryReq)
		if err != nil {
			lastErr = err
			// Exponential backoff: delay * 2^attempt, capped at maxDelay
			backoff := s.config.RetryDelay * (1 << uint(attempt))
			if backoff > maxDelay {
				backoff = maxDelay
			}
			time.Sleep(backoff)
			continue
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
		resp.Body.Close()
		if err != nil {
			lastErr = err
			backoff := s.config.RetryDelay * (1 << uint(attempt))
			if backoff > maxDelay {
				backoff = maxDelay
			}
			time.Sleep(backoff)
			continue
		}
		if resp.StatusCode != 200 {
			lastErr = fmt.Errorf("Hindsight error (%d): %s", resp.StatusCode, string(body))
			backoff := s.config.RetryDelay * (1 << uint(attempt))
			if backoff > maxDelay {
				backoff = maxDelay
			}
			time.Sleep(backoff)
			continue
		}
		return body, nil
	}
	return nil, fmt.Errorf("request failed after %d attempts: %v", s.config.RetryAttempts, lastErr)
}
