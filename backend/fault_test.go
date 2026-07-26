package backend

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"mcp-memory/internal/testutil/cogneemock"
)

// faultBackend returns a CogneeBackend wired to a cogneemock with fault
// injection available, plus the mock itself.
func faultBackend(t *testing.T, tune func(*BackendConfig)) (*CogneeBackend, *cogneemock.Server) {
	t.Helper()
	mock := cogneemock.NewServer()
	t.Cleanup(mock.Close)

	cfg := BackendConfig{
		Backend:               "cognee-rust",
		CogneePort:            fmt.Sprintf("%d", mock.Port()),
		BackendRetainTimeout:  2 * time.Second,
		BackendRecallTimeout:  2 * time.Second,
		BackendReflectTimeout: 2 * time.Second,
		CogneeRetainTimeout:   2 * time.Second,
		RetryAttempts:         3,
		RetryDelay:            5 * time.Millisecond,
		RetryMaxDelay:         20 * time.Millisecond,
	}
	if tune != nil {
		tune(&cfg)
	}
	return newCogneeBackend(cfg), mock
}

const (
	epRemember = "/api/v1/remember"
	epRecall   = "/api/v1/recall"
	epImprove  = "/api/v1/improve"
	epForget   = "/api/v1/forget"
)

// --- Retry semantics ---------------------------------------------------------

// TestRetry_ExactAttemptCountOn5xx pins the retry budget. Off-by-one here is
// invisible in production until it doubles load on a struggling backend.
func TestRetry_ExactAttemptCountOn5xx(t *testing.T) {
	for _, attempts := range []int{1, 2, 3, 5} {
		t.Run(fmt.Sprintf("attempts=%d", attempts), func(t *testing.T) {
			c, mock := faultBackend(t, func(cfg *BackendConfig) {
				cfg.RetryAttempts = attempts
			})
			mock.SetResponse(epRecall, cogneemock.ResponseConfig{StatusCode: 503, Body: `{"e":1}`})

			if _, err := c.Recall(context.Background(), "bank", "q"); err == nil {
				t.Fatal("expected failure")
			}
			if got := mock.CallCount(epRecall); got != attempts {
				t.Fatalf("made %d requests, want exactly %d", got, attempts)
			}
		})
	}
}

// TestRetry_SucceedsMidSequence proves a transient 5xx run recovers without
// burning the whole retry budget.
func TestRetry_SucceedsMidSequence(t *testing.T) {
	c, mock := faultBackend(t, nil)
	mock.SetSequence(epRecall, []cogneemock.ResponseConfig{
		{StatusCode: 503, Body: `{"e":1}`},
		{StatusCode: 503, Body: `{"e":2}`},
		{StatusCode: 200, Body: `[{"text":"recovered"}]`},
	})

	body, err := c.Recall(context.Background(), "bank", "q")
	if err != nil {
		t.Fatalf("should recover on third attempt: %v", err)
	}
	if !strings.Contains(body, "recovered") {
		t.Fatalf("body = %q", body)
	}
	if got := mock.CallCount(epRecall); got != 3 {
		t.Fatalf("made %d requests, want 3", got)
	}
}

// TestRetry_FourXXNeverRetried covers every 4xx the backend can plausibly see.
func TestRetry_FourXXNeverRetried(t *testing.T) {
	for _, code := range []int{400, 401, 403, 404, 409, 413, 422, 429} {
		t.Run(fmt.Sprintf("%d", code), func(t *testing.T) {
			c, mock := faultBackend(t, nil)
			mock.SetResponse(epRecall, cogneemock.ResponseConfig{StatusCode: code, Body: `{"e":1}`})

			if _, err := c.Recall(context.Background(), "bank", "q"); err == nil {
				t.Fatal("expected error")
			}
			if got := mock.CallCount(epRecall); got != 1 {
				t.Fatalf("4xx retried: %d requests, want 1", got)
			}
		})
	}
}

// TestRetry_BackoffGrowsAndCaps measures elapsed time against the expected
// exponential schedule. Retry delay bugs are otherwise silent.
func TestRetry_BackoffGrowsAndCaps(t *testing.T) {
	c, mock := faultBackend(t, func(cfg *BackendConfig) {
		cfg.RetryAttempts = 4
		cfg.RetryDelay = 40 * time.Millisecond
		cfg.RetryMaxDelay = 60 * time.Millisecond // caps the 3rd backoff (160ms -> 60ms)
		cfg.BackendRecallTimeout = 5 * time.Second
	})
	mock.SetResponse(epRecall, cogneemock.ResponseConfig{StatusCode: 500, Body: `{}`})

	start := time.Now()
	if _, err := c.Recall(context.Background(), "bank", "q"); err == nil {
		t.Fatal("expected failure")
	}
	elapsed := time.Since(start)

	// Backoffs between 4 attempts, each capped at 60ms:
	//   40ms, min(80,60)=60ms, min(160,60)=60ms  =>  160ms total.
	// No sleep after the final attempt. The band below is chosen to fail if the
	// schedule degenerates in either direction:
	//   flat 40ms delays   => 120ms (below wantMin)
	//   uncapped 40/80/160 => 280ms (above wantMax)
	const wantMin = 150 * time.Millisecond
	const wantMax = 250 * time.Millisecond
	if elapsed < wantMin {
		t.Fatalf("elapsed %v < %v: backoff is not growing", elapsed, wantMin)
	}
	if elapsed > wantMax {
		t.Fatalf("elapsed %v > %v: backoff is not capped, or sleeps after the last attempt",
			elapsed, wantMax)
	}
}

// TestRetry_NoSleepAfterFinalAttempt isolates the wasted-backoff regression:
// with one attempt there must be no backoff at all.
func TestRetry_NoSleepAfterFinalAttempt(t *testing.T) {
	c, mock := faultBackend(t, func(cfg *BackendConfig) {
		cfg.RetryAttempts = 1
		cfg.RetryDelay = 2 * time.Second
		cfg.RetryMaxDelay = 2 * time.Second
		cfg.BackendRecallTimeout = 10 * time.Second
	})
	mock.SetResponse(epRecall, cogneemock.ResponseConfig{StatusCode: 500, Body: `{}`})

	start := time.Now()
	if _, err := c.Recall(context.Background(), "bank", "q"); err == nil {
		t.Fatal("expected failure")
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("slept %v after the only attempt — no backoff should run", elapsed)
	}
}

// --- Cancellation and timeouts ----------------------------------------------

// TestCancel_DuringBackoffReturnsPromptly is the regression guard for the
// non-cancellable time.Sleep: a cancelled caller must not keep a worker slot
// held for the remainder of the backoff.
func TestCancel_DuringBackoffReturnsPromptly(t *testing.T) {
	c, mock := faultBackend(t, func(cfg *BackendConfig) {
		cfg.RetryAttempts = 4
		cfg.RetryDelay = 2 * time.Second
		cfg.RetryMaxDelay = 4 * time.Second
		cfg.BackendRecallTimeout = 30 * time.Second
	})
	mock.SetResponse(epRecall, cogneemock.ResponseConfig{StatusCode: 500, Body: `{}`})

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(100 * time.Millisecond) // land inside the first backoff
		cancel()
	}()

	start := time.Now()
	if _, err := c.Recall(ctx, "bank", "q"); err == nil {
		t.Fatal("expected cancellation error")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("took %v to observe cancellation — backoff is not cancellable", elapsed)
	}
}

// TestCancel_DuringInFlightRequest covers cancellation while the backend is
// hanging, not merely between attempts.
func TestCancel_DuringInFlightRequest(t *testing.T) {
	c, mock := faultBackend(t, func(cfg *BackendConfig) {
		cfg.BackendRecallTimeout = 30 * time.Second
	})
	mock.SetBehavior(epRecall, cogneemock.BehaviorHang)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	if _, err := c.Recall(ctx, "bank", "q"); err == nil {
		t.Fatal("expected cancellation error")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("hang not interrupted by cancellation: %v", elapsed)
	}
}

// TestTimeout_PerOperationDeadlineApplies asserts a slow backend trips the
// per-operation timeout rather than hanging indefinitely.
func TestTimeout_PerOperationDeadlineApplies(t *testing.T) {
	c, mock := faultBackend(t, func(cfg *BackendConfig) {
		cfg.RetryAttempts = 1
		cfg.BackendRecallTimeout = 200 * time.Millisecond
		cfg.CogneeRetainTimeout = 10 * time.Second // client timeout stays generous
	})
	mock.SetLatency(epRecall, 5*time.Second)

	start := time.Now()
	if _, err := c.Recall(context.Background(), "bank", "q"); err == nil {
		t.Fatal("expected timeout")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("recall took %v — BackendRecallTimeout not applied", elapsed)
	}
}

// TestTimeout_EachOperationUsesItsOwnBudget guards against the operations being
// wired to the wrong config field — a mis-wiring that is invisible until a slow
// retain is killed by the short recall budget.
func TestTimeout_EachOperationUsesItsOwnBudget(t *testing.T) {
	c, mock := faultBackend(t, func(cfg *BackendConfig) {
		cfg.RetryAttempts = 1
		cfg.BackendRecallTimeout = 150 * time.Millisecond
		cfg.CogneeRetainTimeout = 3 * time.Second // drives retain + reflect
	})
	// Both endpoints are slower than the recall budget but faster than retain's.
	mock.SetLatency(epRecall, 1*time.Second)
	mock.SetLatency(epRemember, 1*time.Second)

	if _, err := c.Recall(context.Background(), "bank", "q"); err == nil {
		t.Fatal("recall should have timed out on its 150ms budget")
	}
	if _, err := c.Retain(context.Background(), "bank", "content 2026"); err != nil {
		t.Fatalf("retain has a 3s budget and must survive a 1s backend: %v", err)
	}
}

// --- Malformed and hostile responses ----------------------------------------

// TestResponse_HostileBodies ensures no transport-level malformation panics or
// hangs the client. Callers parse the body downstream; the contract here is
// that doRequest returns cleanly either way.
func TestResponse_HostileBodies(t *testing.T) {
	cases := []struct {
		name      string
		behavior  cogneemock.Behavior
		wantError bool
	}{
		{"malformed JSON passes through", cogneemock.BehaviorMalformedJSON, false},
		{"HTML error page passes through", cogneemock.BehaviorHTMLError, false},
		{"empty body passes through", cogneemock.BehaviorEmptyBody, false},
		{"connection closed mid-body", cogneemock.BehaviorCloseMidBody, true},
		{"body exceeds 10MB limit", cogneemock.BehaviorHugeBody, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, mock := faultBackend(t, func(cfg *BackendConfig) {
				cfg.RetryAttempts = 1
				cfg.BackendRecallTimeout = 10 * time.Second
				cfg.CogneeRetainTimeout = 10 * time.Second
			})
			mock.SetBehavior(epRecall, tc.behavior)

			done := make(chan struct{})
			var err error
			go func() {
				defer close(done)
				_, err = c.Recall(context.Background(), "bank", "q")
			}()

			select {
			case <-done:
			case <-time.After(15 * time.Second):
				t.Fatal("client hung on hostile response")
			}

			if tc.wantError && err == nil {
				t.Fatalf("expected an error for %s", tc.name)
			}
			if !tc.wantError && err != nil {
				t.Fatalf("unexpected error for %s: %v", tc.name, err)
			}
		})
	}
}

// TestResponse_OversizeIsRejectedNotTruncated is the specific guard for silent
// truncation: an 11MB body must surface as an error, never as a short read that
// downstream JSON parsing blames on the backend.
func TestResponse_OversizeIsRejectedNotTruncated(t *testing.T) {
	c, mock := faultBackend(t, func(cfg *BackendConfig) {
		cfg.RetryAttempts = 1
		cfg.BackendRecallTimeout = 20 * time.Second
	})
	mock.SetBehavior(epRecall, cogneemock.BehaviorHugeBody)

	_, err := c.Recall(context.Background(), "bank", "q")
	if err == nil {
		t.Fatal("oversize body must error, not truncate silently")
	}
	if !strings.Contains(err.Error(), "limit") {
		t.Fatalf("error should name the size limit, got: %v", err)
	}
}

// --- Circuit breaker under the real client ----------------------------------

// TestBreaker_TripsAfterThresholdAndStopsCallingBackend is the property that
// matters operationally: once open, the breaker must stop generating load.
func TestBreaker_TripsAfterThresholdAndStopsCallingBackend(t *testing.T) {
	c, mock := faultBackend(t, func(cfg *BackendConfig) {
		cfg.RetryAttempts = 1
	})
	c.breaker = NewCircuitBreaker(3, time.Hour)
	mock.SetResponse(epRecall, cogneemock.ResponseConfig{StatusCode: 500, Body: `{}`})

	for i := 0; i < 3; i++ {
		if _, err := c.Recall(context.Background(), "bank", "q"); err == nil {
			t.Fatalf("call %d should have failed", i)
		}
	}
	callsAtTrip := mock.CallCount(epRecall)
	if callsAtTrip != 3 {
		t.Fatalf("expected 3 backend calls before trip, got %d", callsAtTrip)
	}

	// Breaker is now open — further calls must fail fast without touching the backend.
	for i := 0; i < 5; i++ {
		_, err := c.Recall(context.Background(), "bank", "q")
		if err == nil || !strings.Contains(err.Error(), "circuit breaker open") {
			t.Fatalf("call %d should fail fast, got: %v", i, err)
		}
	}
	if got := mock.CallCount(epRecall); got != callsAtTrip {
		t.Fatalf("open breaker still reached the backend: %d calls, want %d", got, callsAtTrip)
	}
}

// TestBreaker_RecoversViaProbe walks the full state machine against a backend
// that comes back up.
func TestBreaker_RecoversViaProbe(t *testing.T) {
	c, mock := faultBackend(t, func(cfg *BackendConfig) {
		cfg.RetryAttempts = 1
	})
	c.breaker = NewCircuitBreaker(2, 50*time.Millisecond)
	mock.SetResponse(epRecall, cogneemock.ResponseConfig{StatusCode: 500, Body: `{}`})

	for i := 0; i < 2; i++ {
		_, _ = c.Recall(context.Background(), "bank", "q")
	}
	if _, err := c.Recall(context.Background(), "bank", "q"); !strings.Contains(errString(err), "circuit breaker open") {
		t.Fatalf("breaker should be open, got: %v", err)
	}

	// Backend recovers; wait out the cooldown so the next call is the probe.
	mock.SetResponse(epRecall, cogneemock.ResponseConfig{})
	time.Sleep(80 * time.Millisecond)

	if _, err := c.Recall(context.Background(), "bank", "q"); err != nil {
		t.Fatalf("probe should succeed against a healthy backend: %v", err)
	}
	// Circuit is closed again — subsequent calls flow normally.
	if _, err := c.Recall(context.Background(), "bank", "q"); err != nil {
		t.Fatalf("breaker should be closed after successful probe: %v", err)
	}
}

// TestBreaker_ConcurrentFirstWaveThenTrips documents and pins the breaker's
// behaviour under a concurrent burst against a dead backend.
//
// A check-then-act breaker cannot retroactively stop calls that already passed
// IsTripped(), so the entire first wave reaches the backend — every goroutine
// clears the gate before any of them has recorded a failure. That is inherent
// to the design and matches how gobreaker/Hystrix behave; the breaker's job is
// to protect the *subsequent* waves. This test pins that contract so a future
// change cannot quietly make it worse.
func TestBreaker_ConcurrentFirstWaveThenTrips(t *testing.T) {
	c, mock := faultBackend(t, func(cfg *BackendConfig) {
		cfg.RetryAttempts = 1
	})
	c.breaker = NewCircuitBreaker(5, time.Hour)
	mock.SetResponse(epRecall, cogneemock.ResponseConfig{StatusCode: 500, Body: `{}`})

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = c.Recall(context.Background(), "bank", "q")
		}()
	}
	wg.Wait()

	firstWave := mock.CallCount(epRecall)
	if !c.breaker.IsTripped() {
		t.Fatal("breaker must be open after 50 concurrent failures")
	}

	// The second wave is what the breaker exists to stop: not one call may reach
	// the backend.
	for i := 0; i < 25; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = c.Recall(context.Background(), "bank", "q")
		}()
	}
	wg.Wait()

	if got := mock.CallCount(epRecall); got != firstWave {
		t.Fatalf("open breaker leaked %d calls to a dead backend", got-firstWave)
	}
}

// TestBreaker_ClientErrorsDoNotTrip confirms a stream of 4xx never opens the
// circuit — bad input from one agent must not deny service to every other agent.
func TestBreaker_ClientErrorsDoNotTrip(t *testing.T) {
	c, mock := faultBackend(t, func(cfg *BackendConfig) {
		cfg.RetryAttempts = 1
	})
	c.breaker = NewCircuitBreaker(3, time.Hour)
	mock.SetResponse(epRecall, cogneemock.ResponseConfig{StatusCode: 422, Body: `{"e":"bad"}`})

	for i := 0; i < 20; i++ {
		_, err := c.Recall(context.Background(), "bank", "q")
		if err == nil {
			t.Fatal("expected 4xx error")
		}
		if strings.Contains(err.Error(), "circuit breaker open") {
			t.Fatalf("4xx tripped the breaker on call %d", i)
		}
	}
	if c.breaker.IsTripped() {
		t.Fatal("breaker must stay closed under pure client errors")
	}
	if got := mock.CallCount(epRecall); got != 20 {
		t.Fatalf("all 20 calls should reach the backend, got %d", got)
	}
}

// --- Request shape -----------------------------------------------------------

// TestRequestShape_RetainSendsBankAndContent verifies the multipart payload
// actually carries what Cognee needs, including the temporal flag.
func TestRequestShape_RetainSendsBankAndContent(t *testing.T) {
	c, mock := faultBackend(t, func(cfg *BackendConfig) {
		cfg.TemporalCognify = true
	})
	if _, err := c.Retain(context.Background(), "outreach:alice", "Alice joined in 2024"); err != nil {
		t.Fatalf("retain: %v", err)
	}
	req := mock.LastRequest(epRemember)
	if req == nil {
		t.Fatal("no request captured")
	}
	for _, want := range []string{"datasetName=outreach:alice", "Alice joined in 2024", "temporalCognify=true"} {
		if !strings.Contains(req.Body, want) {
			t.Errorf("request body missing %q\ngot: %s", want, req.Body)
		}
	}
}

// TestRequestShape_RetainAutoStampsUndatedContent covers the date auto-stamp:
// undated content must gain a date so temporal_cognify can build a timeline,
// and already-dated content must be left alone.
func TestRequestShape_RetainAutoStampsUndatedContent(t *testing.T) {
	c, mock := faultBackend(t, nil)

	if _, err := c.Retain(context.Background(), "bank", "no year here"); err != nil {
		t.Fatalf("retain: %v", err)
	}
	stamped := mock.LastRequest(epRemember)
	if !strings.Contains(stamped.Body, time.Now().Format("2006-01-02")) {
		t.Errorf("undated content was not stamped: %s", stamped.Body)
	}

	if _, err := c.Retain(context.Background(), "bank", "happened in 1999"); err != nil {
		t.Fatalf("retain: %v", err)
	}
	dated := mock.LastRequest(epRemember)
	if strings.Contains(dated.Body, time.Now().Format("2006-01-02")) {
		t.Errorf("already-dated content should not be stamped: %s", dated.Body)
	}
}

// TestRequestShape_BankIsolation asserts each operation sends the caller's bank
// and never a different one. Cross-bank leakage is a privacy failure.
func TestRequestShape_BankIsolation(t *testing.T) {
	c, mock := faultBackend(t, nil)
	ctx := context.Background()

	if _, err := c.Recall(ctx, "email:client_42", "who"); err != nil {
		t.Fatalf("recall: %v", err)
	}
	if got := mock.LastRequest(epRecall); !strings.Contains(got.Body, "email:client_42") {
		t.Errorf("recall lost the bank: %s", got.Body)
	}

	if _, err := c.Reflect(ctx, "email:client_42", ""); err != nil {
		t.Fatalf("reflect: %v", err)
	}
	if got := mock.LastRequest(epImprove); !strings.Contains(got.Body, "email:client_42") {
		t.Errorf("reflect lost the bank: %s", got.Body)
	}

	if _, err := c.Forget(ctx, "email:client_42", "id-1"); err != nil {
		t.Fatalf("forget: %v", err)
	}
	if got := mock.LastRequest(epForget); !strings.Contains(got.Body, "email:client_42") {
		t.Errorf("forget lost the bank: %s", got.Body)
	}
}

// TestRequestShape_ForgetHonoursMemoryOnly pins the memory_only flag, which
// decides whether the graph structure survives a forget.
func TestRequestShape_ForgetHonoursMemoryOnly(t *testing.T) {
	for _, memoryOnly := range []bool{true, false} {
		t.Run(fmt.Sprintf("memoryOnly=%v", memoryOnly), func(t *testing.T) {
			c, mock := faultBackend(t, func(cfg *BackendConfig) {
				cfg.MemoryOnly = memoryOnly
			})
			if _, err := c.Forget(context.Background(), "bank", "cid"); err != nil {
				t.Fatalf("forget: %v", err)
			}
			want := fmt.Sprintf(`"memory_only":%v`, memoryOnly)
			if got := mock.LastRequest(epForget); !strings.Contains(got.Body, want) {
				t.Errorf("want %s in body, got: %s", want, got.Body)
			}
		})
	}
}

// --- Health ------------------------------------------------------------------

// TestHealth_ReportsBackendState covers the path the watchdog depends on.
func TestHealth_ReportsBackendState(t *testing.T) {
	c, mock := faultBackend(t, nil)

	if err := c.Health(context.Background()); err != nil {
		t.Fatalf("healthy backend should report nil: %v", err)
	}

	mock.SetResponse("/health", cogneemock.ResponseConfig{StatusCode: 503, Body: `{"status":"down"}`})
	if err := c.Health(context.Background()); err == nil {
		t.Fatal("unhealthy backend must report an error")
	}
}

// TestHealth_RespectsContextCancellation ensures a hanging health check cannot
// stall the health monitor loop.
func TestHealth_RespectsContextCancellation(t *testing.T) {
	c, mock := faultBackend(t, nil)
	mock.SetBehavior("/health", cogneemock.BehaviorHang)

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()

	start := time.Now()
	if err := c.Health(ctx); err == nil {
		t.Fatal("expected a timeout error")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("health check ignored the context deadline: %v", elapsed)
	}
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	if errors.Is(err, context.Canceled) {
		return "canceled"
	}
	return err.Error()
}
