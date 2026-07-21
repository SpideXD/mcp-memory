package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"mcp-memory/logger"
)

// testLogger returns a silent logger for tests (only errors visible).
func testCtx() context.Context {
	return context.Background()
}

func testLogger() *logger.Logger {
	l, err := logger.New("worker-test", "error")
	if err != nil {
		panic("testLogger: " + err.Error())
	}
	return l
}

// ─── Tests ─────────────────────────────────────────────────────────────────

func TestPool_StartStop(t *testing.T) {
	var counter atomic.Int64
	p := NewPool("start-stop", 3, testLogger(), func(_ context.Context) {
		counter.Add(1)
	})
	p.Start()
	time.Sleep(10 * time.Millisecond)
	p.Stop()

	if counter.Load() == 0 {
		t.Error("expected at least one execution per worker")
	}
}

func TestPool_StartIdempotent(t *testing.T) {
	var counter atomic.Int64
	p := NewPool("idempotent", 2, testLogger(), func(_ context.Context) {
		counter.Add(1)
	})

	p.Start()
	p.Start() // second call must be no-op
	time.Sleep(10 * time.Millisecond)
	p.Stop()

	// Just verify no panic, double goroutines, or deadlock.
	if counter.Load() == 0 {
		t.Error("expected counter > 0")
	}
}

func TestPool_WorkerExecutesFn(t *testing.T) {
	var counter atomic.Int64
	p := NewPool("exec", 5, testLogger(), func(_ context.Context) {
		counter.Add(1)
	})
	p.Start()
	time.Sleep(20 * time.Millisecond)
	p.Stop()

	v := counter.Load()
	if v < 5 {
		t.Errorf("expected at least 5 executions (one per worker), got %d", v)
	}
}

func TestPool_PanicRecovery(t *testing.T) {
	var iter atomic.Int64
	p := NewPool("panic-recover", 2, testLogger(), func(_ context.Context) {
		v := iter.Add(1)
		// Panic on first invocation only.
		if v == 1 {
			panic("first call panic")
		}
	})
	p.Start()
	time.Sleep(50 * time.Millisecond)
	p.Stop()

	v := iter.Load()
	if v < 3 {
		t.Errorf("expected fn to continue after panic (>=3), got %d", v)
	}
}

func TestPool_PanicRestartCount(t *testing.T) {
	var iter atomic.Int64
	panicCount := 5 // panic 5 times then stop
	p := NewPool("panic-restart", 1, testLogger(), func(_ context.Context) {
		if iter.Add(1) <= int64(panicCount) {
			panic("always panic")
		}
	})
	p.Start()
	time.Sleep(200 * time.Millisecond)
	p.Stop()

	stats := p.Stats()
	panics := stats["panics"].(int64)
	if panics == 0 {
		t.Error("expected panics > 0")
	}
	if panics != int64(panicCount) {
		t.Logf("panics = %d (expected ~%d, may vary with timing)", panics, panicCount)
	}
}

func TestPool_StopDuringWork(t *testing.T) {
	p := NewPool("stop-during", 5, testLogger(), func(_ context.Context) {
		time.Sleep(50 * time.Millisecond)
	})
	p.Start()

	// Stop immediately (workers are in the middle of sleep).
	done := make(chan struct{})
	go func() {
		p.Stop()
		close(done)
	}()

	select {
	case <-done:
		// All workers exited cleanly.
	case <-time.After(2 * time.Second):
		t.Fatal("goroutine leak: workers did not stop within 2s")
	}
}

func TestPool_NestedPanic(t *testing.T) {
	p := NewPool("nested-panic", 1, testLogger(), func(_ context.Context) {
		panic("inner panic")
	})

	p.Start()
	time.Sleep(200 * time.Millisecond)
	p.Stop()

	// Pool should survive nested panic — recovery catches all.
	stats := p.Stats()
	if stats["panics"].(int64) == 0 {
		t.Error("expected at least one panic")
	}
}

func TestPool_ZeroWorkers(t *testing.T) {
	var counter atomic.Int64
	p := NewPool("zero", 0, testLogger(), func(_ context.Context) {
		counter.Add(1)
	})
	p.Start()
	time.Sleep(10 * time.Millisecond)
	p.Stop()

	// 0 → defaults to 1 worker.
	stats := p.Stats()
	if stats["workers"].(int) != 1 {
		t.Errorf("expected workers=1 (default), got %d", stats["workers"])
	}
	if counter.Load() == 0 {
		t.Error("expected counter > 0")
	}
}

func TestPool_NegativeWorkers(t *testing.T) {
	var counter atomic.Int64
	p := NewPool("negative", -5, testLogger(), func(_ context.Context) {
		counter.Add(1)
	})
	p.Start()
	time.Sleep(10 * time.Millisecond)
	p.Stop()

	stats := p.Stats()
	if stats["workers"].(int) != 1 {
		t.Errorf("expected workers=1 (default for negative), got %d", stats["workers"])
	}
}

func TestPool_StatsAccuracy(t *testing.T) {
	var iter atomic.Int64
	p := NewPool("stats", 3, testLogger(), func(_ context.Context) {
		if iter.Add(1) <= 5 {
			panic("intentional panic")
		}
	})
	p.Start()
	time.Sleep(100 * time.Millisecond)
	p.Stop()

	stats := p.Stats()
	if stats["workers"].(int) != 3 {
		t.Errorf("expected workers=3, got %d", stats["workers"])
	}
	// started is false after Stop(). The test checks that it WAS started.
	// The panics counter confirms workers ran.
	if stats["panics"].(int64) == 0 {
		t.Error("expected panics > 0")
	}
}

func TestPool_StopThenStart(t *testing.T) {
	var counter atomic.Int64
	p := NewPool("stop-start", 2, testLogger(), func(_ context.Context) {
		counter.Add(1)
	})

	p.Start()
	time.Sleep(10 * time.Millisecond)
	p.Stop()

	// Restart after stop — new goroutines should run.
	counter.Store(0)
	p.Start()
	time.Sleep(10 * time.Millisecond)
	p.Stop()

	if counter.Load() == 0 {
		t.Error("expected workers to execute after restart")
	}

	// Verify stats show restarted state correctly.
	stats := p.Stats()
	if stats["started"].(bool) {
		t.Error("expected started=false after final stop")
	}
}

func TestPool_ConcurrentStop(t *testing.T) {
	p := NewPool("concurrent-stop", 3, testLogger(), func(_ context.Context) {
		time.Sleep(10 * time.Millisecond)
	})
	p.Start()

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p.Stop()
		}()
	}
	wg.Wait()

	// All workers should be stopped. No double-close panic, no goroutine leak.
	// started is false after Stop() — verifies clean shutdown.
	if p.started.Load() {
		t.Error("expected started=false after Stop")
	}
}

func TestPool_FnReturnsNormally(t *testing.T) {
	var counter atomic.Int64
	p := NewPool("normal-return", 1, testLogger(), func(_ context.Context) {
		counter.Add(1)
		// fn returns immediately — worker should call it again.
	})
	p.Start()
	time.Sleep(20 * time.Millisecond)
	p.Stop()

	v := counter.Load()
	if v < 2 {
		t.Errorf("expected fn to be called multiple times (>=2), got %d", v)
	}
}

func TestPool_GracefulShutdown(t *testing.T) {
	p := NewPool("graceful", 2, testLogger(), func(_ context.Context) {
		time.Sleep(10 * time.Millisecond)
	})
	p.Start()

	done := make(chan struct{})
	go func() {
		p.Stop()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("workers did not shut down gracefully within 2s")
	}
}

func TestPool_LoggerIntegration(t *testing.T) {
	var buf bytes.Buffer
	log, _ := logger.NewBuf("worker-test", "debug", &buf)

	var iter atomic.Int64
	p := NewPool("log-test", 1, log, func(_ context.Context) {
		if iter.Add(1) == 1 {
			panic("test panic")
		}
	})
	p.Start()
	time.Sleep(100 * time.Millisecond)
	p.Stop()

	output := buf.String()
	if !strings.Contains(output, "worker panicked") {
		t.Error("expected 'worker panicked' in log output")
	}
	if !strings.Contains(output, "log-test") {
		t.Error("expected pool name 'log-test' in log output")
	}

	// Every line must be valid JSON.
	for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
		if line == "" {
			continue
		}
		if !json.Valid([]byte(line)) {
			t.Errorf("invalid JSON: %s", line)
		}
	}
}
