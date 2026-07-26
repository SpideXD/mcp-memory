package backend

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Cancellable sleep waits for d or until ctx is cancelled.
// Returns ctx.Err() if cancelled, nil if the duration elapsed.
func sleepCancel(ctx context.Context, d time.Duration) error {
	select {
	case <-time.After(d):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// doRequest executes an HTTP request with retry logic and exponential backoff.
// Backoff is cancellation-aware (sleepCancel respects ctx.Done()).
// 4xx responses are returned immediately without retry.
// 5xx and connection errors are retried with backoff up to retryAttempts times.
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
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	if retryMaxDelay <= 0 {
		retryMaxDelay = 30 * time.Second
	}

	var lastErr error
	maxBody := int64(10 << 20) // 10 MB

	for attempt := 0; attempt < retryAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("request cancelled: %w", err)
		}

		retryReq := req.Clone(ctx)
		retryReq.Body = io.NopCloser(bytes.NewReader(bodyBytes))
		retryReq.ContentLength = int64(len(bodyBytes))
		retryReq.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(bodyBytes)), nil }

		resp, err := client.Do(retryReq)
		if err != nil {
			lastErr = err
			// Don't sleep after the last attempt
			if attempt < retryAttempts-1 {
				backoff := retryDelay * (1 << uint(attempt))
				if backoff > retryMaxDelay {
					backoff = retryMaxDelay
				}
				if cerr := sleepCancel(ctx, backoff); cerr != nil {
					return nil, fmt.Errorf("request cancelled during backoff: %w", cerr)
				}
			}
			continue
		}

		// Detect truncation — if LimitReader hit the limit, the body was cut.
		lr := io.LimitReader(resp.Body, maxBody+1)
		body, readErr := io.ReadAll(lr)
		resp.Body.Close()
		if readErr != nil {
			lastErr = readErr
			if attempt < retryAttempts-1 {
				backoff := retryDelay * (1 << uint(attempt))
				if backoff > retryMaxDelay {
					backoff = retryMaxDelay
				}
				if cerr := sleepCancel(ctx, backoff); cerr != nil {
					return nil, fmt.Errorf("request cancelled during backoff: %w", cerr)
				}
			}
			continue
		}
		if int64(len(body)) > maxBody {
			return nil, fmt.Errorf("response body exceeds %d byte limit (got %d bytes)", maxBody, len(body))
		}

		if resp.StatusCode != 200 {
			lastErr = fmt.Errorf("HTTP error (%d): %s", resp.StatusCode, string(body))
			// 4xx = client error — don't retry
			if resp.StatusCode >= 400 && resp.StatusCode < 500 {
				return nil, lastErr
			}
			// 5xx — retry with backoff
			if attempt < retryAttempts-1 {
				backoff := retryDelay * (1 << uint(attempt))
				if backoff > retryMaxDelay {
					backoff = retryMaxDelay
				}
				if cerr := sleepCancel(ctx, backoff); cerr != nil {
					return nil, fmt.Errorf("request cancelled during backoff: %w", cerr)
				}
			}
			continue
		}
		return body, nil
	}
	if lastErr == nil {
		return nil, fmt.Errorf("request failed after %d attempts", retryAttempts)
	}
	return nil, fmt.Errorf("request failed after %d attempts: %v", retryAttempts, lastErr)
}
