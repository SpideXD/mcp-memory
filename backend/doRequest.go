package backend

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"time"
)

// doRequest executes an HTTP request with retry logic and exponential backoff.
// It returns the response body on success. Used by both Hindsight and Cognee backends.
func doRequest(client *http.Client, req *http.Request, timeout time.Duration, retryAttempts int, retryDelay, retryMaxDelay time.Duration) ([]byte, error) {
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

	if retryMaxDelay <= 0 {
		retryMaxDelay = 30 * time.Second
	}

	var lastErr error
	maxBody := int64(10 << 20) // 10MB response limit

	for attempt := 0; attempt < retryAttempts; attempt++ {
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("request cancelled: %w", ctx.Err())
		default:
		}

		retryReq := req.Clone(ctx)
		retryReq.Body = io.NopCloser(bytes.NewReader(bodyBytes))
		retryReq.ContentLength = int64(len(bodyBytes))
		retryReq.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(bodyBytes)), nil }

		resp, err := client.Do(retryReq)
		if err != nil {
			lastErr = err
			backoff := retryDelay * (1 << uint(attempt))
			if backoff > retryMaxDelay {
				backoff = retryMaxDelay
			}
			time.Sleep(backoff)
			continue
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
		resp.Body.Close()
		if err != nil {
			lastErr = err
			backoff := retryDelay * (1 << uint(attempt))
			if backoff > retryMaxDelay {
				backoff = retryMaxDelay
			}
			time.Sleep(backoff)
			continue
		}
		if resp.StatusCode != 200 {
			lastErr = fmt.Errorf("HTTP error (%d): %s", resp.StatusCode, string(body))
			backoff := retryDelay * (1 << uint(attempt))
			if backoff > retryMaxDelay {
				backoff = retryMaxDelay
			}
			time.Sleep(backoff)
			continue
		}
		return body, nil
	}
	return nil, fmt.Errorf("request failed after %d attempts: %v", retryAttempts, lastErr)
}
