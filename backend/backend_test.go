package backend

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// --- Circuit Breaker Tests ---

func TestCircuitBreaker_ClosedAllowsRequests(t *testing.T) {
	cb := NewCircuitBreaker(2, 10*time.Millisecond)
	if cb.IsTripped() {
		t.Fatal("new breaker should be closed")
	}
}

func TestCircuitBreaker_TripsAtThreshold(t *testing.T) {
	cb := NewCircuitBreaker(2, time.Hour)
	cb.RecordFailure()
	if cb.IsTripped() {
		t.Fatal("should not trip at 1/2 failures")
	}
	cb.RecordFailure()
	if !cb.IsTripped() {
		t.Fatal("should trip at 2/2 failures")
	}
}

func TestCircuitBreaker_HalfOpenGrantsSingleProbe(t *testing.T) {
	cb := NewCircuitBreaker(1, time.Millisecond)
	cb.RecordFailure()
	if !cb.IsTripped() {
		t.Fatal("should be open")
	}
	time.Sleep(2 * time.Millisecond)

	if cb.IsTripped() {
		t.Fatal("should grant probe after cooldown")
	}
	if !cb.IsTripped() {
		t.Fatal("should reject second caller")
	}
}

func TestCircuitBreaker_ProbeSuccessClosesCircuit(t *testing.T) {
	cb := NewCircuitBreaker(1, time.Millisecond)
	cb.RecordFailure()
	time.Sleep(2 * time.Millisecond)

	if cb.IsTripped() {
		t.Fatal("probe should be granted")
	}
	cb.RecordSuccess()
	if cb.IsTripped() {
		t.Fatal("should close after successful probe")
	}
}

func TestCircuitBreaker_ProbeFailureReopensCircuit(t *testing.T) {
	cb := NewCircuitBreaker(1, time.Millisecond)
	cb.RecordFailure()
	if !cb.IsTripped() {
		t.Fatal("should be open")
	}
	time.Sleep(2 * time.Millisecond)

	if cb.IsTripped() {
		t.Fatal("probe should be granted")
	}
	cb.RecordFailure()
	if !cb.IsTripped() {
		t.Fatal("should reopen after failed probe")
	}
}

// testBackend returns a CogneeBackend pointed at srv, with retries disabled so
// call counts are exact. The breaker is replaced by the caller when needed.
func testBackend(t *testing.T, srv *httptest.Server) *CogneeBackend {
	t.Helper()
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse test server URL: %v", err)
	}
	return newCogneeBackend(BackendConfig{
		CogneePort:            u.Port(),
		BackendRetainTimeout:  5 * time.Second,
		BackendRecallTimeout:  5 * time.Second,
		BackendReflectTimeout: 5 * time.Second,
		CogneeRetainTimeout:   5 * time.Second,
		RetryAttempts:         1,
		RetryDelay:            time.Millisecond,
		RetryMaxDelay:         time.Millisecond,
	})
}

// TestCircuitBreaker_ProbeWith4xxDoesNotWedge drives the real call site.
// The wedge was never in CircuitBreaker itself — it was in the 4xx branch of
// Retain/Recall/Reflect/Forget, which returned without recording any outcome,
// leaving halfOpen set forever. Asserting on cb.RecordSuccess() directly would
// pass even against the buggy code, so this must go through the backend.
func TestCircuitBreaker_ProbeWith4xxDoesNotWedge(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(422) // client error — backend is reachable
	}))
	defer srv.Close()

	for _, tc := range []struct {
		name string
		call func(c *CogneeBackend) (string, error)
	}{
		{"Retain", func(c *CogneeBackend) (string, error) {
			return c.Retain(context.Background(), "bank", "x")
		}},
		{"Recall", func(c *CogneeBackend) (string, error) {
			return c.Recall(context.Background(), "bank", "q")
		}},
		{"Reflect", func(c *CogneeBackend) (string, error) {
			return c.Reflect(context.Background(), "bank", "q")
		}},
		{"Forget", func(c *CogneeBackend) (string, error) {
			return c.Forget(context.Background(), "bank", "id")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := testBackend(t, srv)
			c.breaker = NewCircuitBreaker(1, time.Millisecond)

			c.breaker.RecordFailure() // OPEN
			if !c.breaker.IsTripped() {
				t.Fatal("breaker should be open")
			}
			time.Sleep(3 * time.Millisecond) // cooldown expires

			// Probe is granted, hits 4xx. Outcome must be recorded.
			if _, err := tc.call(c); err == nil {
				t.Fatal("expected 4xx error from probe")
			}

			// The breaker must not be stuck. A second call must reach the
			// server rather than fail fast.
			_, err := tc.call(c)
			if err != nil && strings.Contains(err.Error(), "circuit breaker open") {
				t.Fatal("WEDGED: breaker stuck open after 4xx probe")
			}
		})
	}
}

// TestCircuitBreaker_ProbeEarlyReturnDoesNotWedge covers the defer guard: if a
// method returns after the probe is granted but before any outcome is recorded
// (transport error, panic, early return), the deferred RecordFailure must
// re-open the circuit rather than leave it half-open forever.
func TestCircuitBreaker_ProbeTransportErrorReopens(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	srv.Close() // closed immediately — every request is a transport error

	c := testBackend(t, srv)
	c.breaker = NewCircuitBreaker(1, time.Hour)

	c.breaker.RecordFailure() // OPEN, long cooldown
	time.Sleep(time.Millisecond)

	// Force half-open by expiring the trip window.
	c.breaker = NewCircuitBreaker(1, time.Millisecond)
	c.breaker.RecordFailure()
	time.Sleep(3 * time.Millisecond)

	if _, err := c.Retain(context.Background(), "bank", "x"); err == nil {
		t.Fatal("expected transport error")
	}
	// Probe failed → breaker must be OPEN again, not half-open-forever.
	if !c.breaker.IsTripped() {
		t.Fatal("failed probe should re-open the circuit")
	}
}

func TestCircuitBreaker_ConcurrentProbes(t *testing.T) {
	cb := NewCircuitBreaker(1, time.Millisecond)
	cb.RecordFailure()
	time.Sleep(2 * time.Millisecond)

	var wg sync.WaitGroup
	results := make(chan bool, 10)
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results <- cb.IsTripped()
		}()
	}
	wg.Wait()
	close(results)

	passed := 0
	for r := range results {
		if !r {
			passed++
		}
	}
	if passed != 1 {
		t.Fatalf("exactly 1 goroutine should get probe, got %d", passed)
	}
}

func TestCircuitBreaker_HalfOpenReArmsAfterTimeout(t *testing.T) {
	cb := NewCircuitBreaker(1, time.Millisecond)
	cb.RecordFailure()
	time.Sleep(2 * time.Millisecond)
	cb.IsTripped() // halfOpen set
	// Probe outcome never recorded — after 40ms (past 30ms min re-arm), should re-arm
	time.Sleep(40 * time.Millisecond)

	if cb.IsTripped() {
		t.Fatal("should re-arm after probe timeout")
	}
}

func TestCircuitBreaker_RecordSuccessFromClosed(t *testing.T) {
	cb := NewCircuitBreaker(2, time.Hour)
	cb.RecordSuccess()
	cb.RecordFailure()
	cb.RecordSuccess() // reset
	cb.RecordFailure()
	if cb.IsTripped() {
		t.Fatal("should be at 1/2 after reset")
	}
}

// --- Constructor Wiring Tests ---

// TestNewCogneeBackend_TimeoutWiring pins the timeout wiring. A regression here
// is invisible to every other test but breaks production: http.Client.Timeout
// bounds the entire exchange and overrides per-request context deadlines, so a
// client timeout below the real retain duration (~20-30s, docs/benchmarks.md)
// makes every slow retain fail, retry, and trip the circuit breaker.
func TestNewCogneeBackend_TimeoutWiring(t *testing.T) {
	cfg := BackendConfig{
		CogneePort:            "9999",
		BackendRetainTimeout:  60 * time.Second,
		BackendRecallTimeout:  10 * time.Second,
		BackendReflectTimeout: 60 * time.Second,
		CogneeRetainTimeout:   900 * time.Second,
	}
	c := newCogneeBackend(cfg)

	if c.httpClient.Timeout != 900*time.Second {
		t.Errorf("http client timeout = %v, want 900s (CogneeRetainTimeout); "+
			"a lower value silently caps every Cognee call", c.httpClient.Timeout)
	}
	if c.retainTimeout != 900*time.Second {
		t.Errorf("retainTimeout = %v, want 900s", c.retainTimeout)
	}
	if c.reflectTimeout != 900*time.Second {
		t.Errorf("reflectTimeout = %v, want 900s", c.reflectTimeout)
	}
	if c.recallTimeout != 10*time.Second {
		t.Errorf("recallTimeout = %v, want 10s (BackendRecallTimeout)", c.recallTimeout)
	}
}

// TestNewCogneeBackend_FallsBackWhenCogneeTimeoutUnset ensures the 900s default
// is not required — an unset CogneeRetainTimeout falls back to the generic value.
func TestNewCogneeBackend_FallsBackWhenCogneeTimeoutUnset(t *testing.T) {
	c := newCogneeBackend(BackendConfig{
		CogneePort:           "9999",
		BackendRetainTimeout: 45 * time.Second,
		BackendRecallTimeout: 10 * time.Second,
		CogneeRetainTimeout:  0, // unset
	})
	if c.httpClient.Timeout != 45*time.Second {
		t.Errorf("http client timeout = %v, want 45s fallback", c.httpClient.Timeout)
	}
	if c.retainTimeout != 45*time.Second {
		t.Errorf("retainTimeout = %v, want 45s fallback", c.retainTimeout)
	}
}

// TestNewCogneeBackend_PassthroughFields guards the remaining constructor wiring.
func TestNewCogneeBackend_PassthroughFields(t *testing.T) {
	c := newCogneeBackend(BackendConfig{
		CogneePort:          "8123",
		CogneeRetainTimeout: time.Second,
		RetryAttempts:       7,
		RetryDelay:          3 * time.Second,
		RetryMaxDelay:       11 * time.Second,
		TemporalCognify:     true,
		MemoryOnly:          true,
	})
	if c.baseURL != "http://localhost:8123" {
		t.Errorf("baseURL = %q", c.baseURL)
	}
	if c.retryAttempts != 7 || c.retryDelay != 3*time.Second || c.retryMaxDelay != 11*time.Second {
		t.Errorf("retry wiring: attempts=%d delay=%v max=%v",
			c.retryAttempts, c.retryDelay, c.retryMaxDelay)
	}
	if !c.temporalCognify || !c.memoryOnly {
		t.Errorf("feature flags: temporal=%v memoryOnly=%v", c.temporalCognify, c.memoryOnly)
	}
	if c.breaker == nil {
		t.Error("breaker must be initialised")
	}
}

// --- doRequest Tests ---

func TestDoRequest_RetryAttemptsZero(t *testing.T) {
	req, _ := http.NewRequest("GET", "http://127.0.0.1:1", strings.NewReader("test"))
	req.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(strings.NewReader("test")), nil }
	_, err := doRequest(http.DefaultClient, req, time.Second, 0, time.Millisecond, 0)
	if err == nil || !strings.Contains(err.Error(), "0 attempts") {
		t.Fatalf("expected '0 attempts' error, got: %v", err)
	}
}

func TestDoRequest_CancellableBackoff(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(503)
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL, strings.NewReader("test"))
	req.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(strings.NewReader("test")), nil }

	start := time.Now()
	cancel()
	_, err := doRequest(&http.Client{Timeout: time.Second}, req, 5*time.Second, 3, time.Second, 2*time.Second)
	if err == nil {
		t.Fatal("expected error")
	}
	if time.Since(start) > 500*time.Millisecond {
		t.Fatalf("backoff should respect cancellation, took %v", time.Since(start))
	}
}

func TestDoRequest_FourXXNotRetried(t *testing.T) {
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(422)
	}))
	defer srv.Close()

	req, _ := http.NewRequest("GET", srv.URL, strings.NewReader("test"))
	req.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(strings.NewReader("test")), nil }
	_, err := doRequest(http.DefaultClient, req, 5*time.Second, 3, time.Millisecond, 0)
	if err == nil {
		t.Fatal("expected error")
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("4xx should not retry, got %d calls", n)
	}
}

func TestDoRequest_FiveHundredRetried(t *testing.T) {
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		if n < 3 {
			w.WriteHeader(503)
		} else {
			w.WriteHeader(200)
			w.Write([]byte(`"ok"`))
		}
	}))
	defer srv.Close()

	req, _ := http.NewRequest("GET", srv.URL, strings.NewReader("test"))
	req.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(strings.NewReader("test")), nil }
	body, err := doRequest(http.DefaultClient, req, 5*time.Second, 3, time.Millisecond, 0)
	if err != nil {
		t.Fatalf("expected success: %v", err)
	}
	if string(body) != `"ok"` {
		t.Fatalf("got %q", body)
	}
}
