package backend

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
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

func TestCircuitBreaker_ProbeWith4xxDoesNotWedge(t *testing.T) {
	cb := NewCircuitBreaker(1, time.Millisecond)
	cb.RecordFailure()
	if !cb.IsTripped() {
		t.Fatal("should be open")
	}
	time.Sleep(2 * time.Millisecond)

	if cb.IsTripped() {
		t.Fatal("should grant probe")
	}
	// Simulate a 4xx — backend is reachable
	cb.RecordSuccess()

	if cb.IsTripped() {
		t.Fatal("WEDGED: breaker stuck open after 4xx probe")
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
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(422)
	}))
	defer srv.Close()

	req, _ := http.NewRequest("GET", srv.URL, strings.NewReader("test"))
	req.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(strings.NewReader("test")), nil }
	_, err := doRequest(http.DefaultClient, req, 5*time.Second, 3, time.Millisecond, 0)
	if err == nil {
		t.Fatal("expected error")
	}
	if calls != 1 {
		t.Fatalf("4xx should not retry, got %d calls", calls)
	}
}

func TestDoRequest_FiveHundredRetried(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls < 3 {
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
