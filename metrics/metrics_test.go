package metrics

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"mcp-memory/logger"
)

func TestHistogram_SingleRecord(t *testing.T) {
	h := NewHistogram("test_single")
	defer clearRegistry()
	h.Record(42)

	snap := h.Snapshot()
	if snap["test_single_count"] != int64(1) {
		t.Errorf("expected count 1, got %v", snap["test_single_count"])
	}
	if snap["test_single_min"] != int64(42) {
		t.Errorf("expected min 42, got %v", snap["test_single_min"])
	}
	if snap["test_single_max"] != int64(42) {
		t.Errorf("expected max 42, got %v", snap["test_single_max"])
	}
}

func TestHistogram_ConcurrentRecords(t *testing.T) {
	h := NewHistogram("test_conc")
	defer clearRegistry()

	var wg sync.WaitGroup
	n := 50
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(val int64) {
			defer wg.Done()
			h.Record(val)
		}(int64(i))
	}
	wg.Wait()

	snap := h.Snapshot()
	if snap["test_conc_count"] != int64(n) {
		t.Errorf("expected count %d, got %v", n, snap["test_conc_count"])
	}
}

func TestHistogram_Percentiles100(t *testing.T) {
	h := NewHistogram("test_pct")
	defer clearRegistry()
	for i := 1; i <= 100; i++ {
		h.Record(int64(i))
	}

	p50, p90, p99 := h.Percentiles()
	if p50 != 50 && p50 != 51 {
		t.Errorf("p50 = %d (expected 50 or 51)", p50)
	}
	if p90 < 89 || p90 > 91 {
		t.Errorf("expected p90 ~90, got %d", p90)
	}
	if p99 < 98 || p99 > 100 {
		t.Errorf("expected p99 ~99, got %d", p99)
	}
}

func TestHistogram_PercentilesWrap(t *testing.T) {
	h := NewHistogram("test_wrap")
	defer clearRegistry()
	n := 2000 // more than ring buffer
	for i := 0; i < n; i++ {
		h.Record(int64(i))
	}

	p50, _, _ := h.Percentiles()
	if p50 < 500 || p50 > 1500 {
		t.Errorf("p50 = %d (ring buffer wraps, expected 500-1500)", p50)
	}
}

func TestHistogram_RateDecay(t *testing.T) {
	h := NewHistogram("test_decay")
	defer clearRegistry()

	h.Record(1)
	h.Record(1)

	// Rate should be positive.
	snap := h.Snapshot()
	rate, ok := snap["test_decay_rate"].(string)
	if !ok || !strings.Contains(rate, "/s") {
		t.Errorf("expected rate string, got %v", snap["test_decay_rate"])
	}
}

func TestHistogram_ZeroRecords(t *testing.T) {
	h := NewHistogram("test_zero")
	defer clearRegistry()

	snap := h.Snapshot()
	v, ok := snap["test_zero_count"]
	if !ok {
		t.Fatal("expected test_zero_count key")
	}
	// Compare as float64 (JSON unmarshal default) or int64.
	switch val := v.(type) {
	case int64:
		if val != 0 {
			t.Errorf("expected count 0, got %d", val)
		}
	case float64:
		if val != 0 {
			t.Errorf("expected count 0, got %f", val)
		}
	default:
		t.Errorf("unexpected type %T for count: %v", v, v)
	}
	if len(snap) != 1 {
		t.Errorf("expected only count key for zero records, got %d keys: %v", len(snap), snap)
	}
}

func TestTimer_Duration(t *testing.T) {
	tmr := NewTimer("test_timer")
	defer clearRegistry()
	h := tmr.Start()
	time.Sleep(5 * time.Millisecond)
	tmr.Stop(h)

	snap := tmr.Snapshot()
	count, ok := snap["test_timer_count"]
	if !ok || count != int64(1) {
		t.Errorf("expected count 1, got %v", count)
	}
	// Duration should be a string like "5ms".
	dur, ok := snap["test_timer_p50"]
	if !ok {
		t.Error("expected p50 in snapshot")
	}
	if _, ok := dur.(string); !ok {
		t.Errorf("expected duration string, got %T", dur)
	}
}

func TestTimer_WithLogger(t *testing.T) {
	var buf bytes.Buffer
	log, _ := logger.NewBuf("test", "debug", &buf)

	tmr := NewTimer("test_timer_log").WithLogger(log)
	h := tmr.Start()
	time.Sleep(1 * time.Millisecond)
	tmr.Stop(h)

	if !strings.Contains(buf.String(), "timer stop") {
		t.Error("expected 'timer stop' in log output")
	}
}

func TestCounter_Tag(t *testing.T) {
	clearRegistry()
	c := NewCounter("test_counter")
	c2 := c.WithTag("project", "backend")

	if c.Name() != "test_counter" {
		t.Errorf("expected 'test_counter', got %s", c.Name())
	}
	if c2.Name() != "test_counter{project=backend}" {
		t.Errorf("expected 'test_counter{project=backend}', got %s", c2.Name())
	}

	snap := global.Snapshot("")
	if _, ok := snap["test_counter_count"]; !ok {
		t.Error("expected base counter in snapshot")
	}
	if _, ok := snap["test_counter{project=backend}_count"]; !ok {
		t.Error("expected tagged counter in snapshot")
	}
}

func TestCounter_RateDecay(t *testing.T) {
	c := NewCounter("test_decay")
	defer clearRegistry()

	c.Inc()
	c.Inc()
	c.Inc()

	snap := c.Snapshot()
	if snap["test_decay_count"] != int64(3) {
		t.Errorf("expected count 3, got %v", snap["test_decay_count"])
	}
	rate, ok := snap["test_decay_rate"].(string)
	if !ok || !strings.Contains(rate, "/s") {
		t.Errorf("expected rate string, got %v", snap["test_decay_rate"])
	}
}

func TestCounter_Concurrent(t *testing.T) {
	c := NewCounter("test_conc_count")
	defer clearRegistry()

	var wg sync.WaitGroup
	n := 100
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.Inc()
		}()
	}
	wg.Wait()

	if c.Value() != int64(n) {
		t.Errorf("expected %d, got %d", n, c.Value())
	}
}

func TestGauge_IncDecSet(t *testing.T) {
	g := NewGauge("test_gauge")
	defer clearRegistry()

	if g.Value() != 0 {
		t.Errorf("expected 0, got %d", g.Value())
	}

	g.Inc()
	if g.Value() != 1 {
		t.Errorf("expected 1, got %d", g.Value())
	}

	g.Dec()
	if g.Value() != 0 {
		t.Errorf("expected 0, got %d", g.Value())
	}

	g.Add(10)
	if g.Value() != 10 {
		t.Errorf("expected 10, got %d", g.Value())
	}

	g.Set(-5)
	if g.Value() != -5 {
		t.Errorf("expected -5, got %d", g.Value())
	}
}

func TestRegistry_SnapshotAll(t *testing.T) {
	clearRegistry()
	NewCounter("r_all_a")
	NewCounter("r_all_b")

	snap := global.Snapshot("")
	if len(snap) < 4 { // 2 counters × 2 keys each
		t.Errorf("expected at least 4 keys, got %d", len(snap))
	}
}

func TestRegistry_SnapshotPrefix(t *testing.T) {
	clearRegistry()
	NewCounter("collector.files")
	NewCounter("indexer.symbols")

	snap := global.Snapshot("collector")
	for k := range snap {
		if !strings.HasPrefix(k, "collector.") {
			t.Errorf("expected all keys to start with 'collector.', got %q", k)
		}
	}

	snap2 := global.Snapshot("indexer")
	for k := range snap2 {
		if !strings.HasPrefix(k, "indexer.") {
			t.Errorf("expected all keys to start with 'indexer.', got %q", k)
		}
	}
}

func TestRegistry_MarshalJSON(t *testing.T) {
	clearRegistry()
	NewCounter("json_test")
	NewGauge("json_gauge")

	data, err := global.MarshalJSON()
	if err != nil {
		t.Fatalf("MarshalJSON: %v", err)
	}
	if !json.Valid(data) {
		t.Fatal("invalid JSON")
	}
}

func TestRegistry_AutoRegister(t *testing.T) {
	clearRegistry()
	_ = NewCounter("auto_reg")
	_ = NewGauge("auto_gauge")
	_ = NewHistogram("auto_hist")

	snap := global.Snapshot("")
	if _, ok := snap["auto_reg_count"]; !ok {
		t.Error("expected auto_reg in snapshot")
	}
	if _, ok := snap["auto_gauge"]; !ok {
		t.Error("expected auto_gauge in snapshot")
	}
	if _, ok := snap["auto_hist_count"]; !ok {
		t.Error("expected auto_hist in snapshot")
	}
}

func TestEWMA_Immediate(t *testing.T) {
	e := newEWMA()
	// Simulate 100 events in the last 1 second → rate = 100/s.
	e.mu.Lock()
	e.lastUpdate = time.Now().Add(-1 * time.Second)
	e.mu.Unlock()
	e.Update(100)
	v := e.Value()
	if v < 50 || v > 150 {
		t.Errorf("expected rate ~100/s, got %f", v)
	}
}

func TestEWMA_Decay(t *testing.T) {
	e := newEWMA()
	// Set up a baseline of 100 events/s.
	e.mu.Lock()
	e.lastUpdate = time.Now().Add(-1 * time.Second)
	e.mu.Unlock()
	e.Update(100) // 100/s

	// After 100ms idle, rate should be slightly below 100/s but not zero.
	time.Sleep(100 * time.Millisecond)
	v := e.Value()
	if v <= 0 || v > 100 {
		t.Errorf("expected rate between 0 and 100 after short idle, got %f", v)
	}
}

func TestEWMA_Convergence(t *testing.T) {
	e := newEWMA()

	// Simulate events at a steady 100/s over 1 second: 100 events, each 10ms apart.
	// Start with a baseline: set lastUpdate to 1 second ago and record 100 events.
	e.mu.Lock()
	e.lastUpdate = time.Now().Add(-1 * time.Second)
	e.mu.Unlock()
	e.Update(100) // seed: 100 events in 1 second → 100/s

	// Now pump single events at 10ms intervals. Each Update has elapsed ≈ 10ms,
	// so instantRate = 1/0.01 = 100/s, reinforcing the baseline.
	for i := 0; i < 100; i++ {
		time.Sleep(10 * time.Millisecond)
		e.Update(1)
	}

	v := e.Value()
	if v < 50 || v > 150 {
		t.Errorf("expected rate ~100/s after 1s of 100/s events, got %f", v)
	}
}

func TestEWMA_BurstAndDecay(t *testing.T) {
	e := newEWMA()
	e.Update(1000) // burst

	// With 30-second half-life, 200ms of idle produces minimal decay.
	v := e.Value()
	if v > 1001 {
		t.Errorf("rate above peak after idle: %f", v)
	}

	// Wait long enough to observe decay (30s half-life × 10% ≈ 3s).
	time.Sleep(3 * time.Second)
	v = e.Value()
	if v > 950 {
		t.Errorf("expected significant decay after 3s idle, got %f", v)
	}
}

func TestSys_Snapshot(t *testing.T) {
	s := CaptureSysStats()
	if _, ok := s["sys_goroutines"]; !ok {
		t.Error("expected sys_goroutines")
	}
	if _, ok := s["sys_alloc_mb"]; !ok {
		t.Error("expected sys_alloc_mb")
	}
}

func TestSys_NonZero(t *testing.T) {
	s := CaptureSysStats()
	if s["sys_goroutines"].(int64) <= 0 {
		t.Errorf("expected goroutines > 0, got %d", s["sys_goroutines"])
	}
	if s["sys_alloc_mb"].(int64) < 0 {
		t.Errorf("expected alloc_mb >= 0, got %d", s["sys_alloc_mb"])
	}
}

type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestReporter_DumpsAtInterval(t *testing.T) {
	var lb lockedBuffer
	log, _ := logger.NewBuf("test", "info", &lb)

	clearRegistry()
	NewCounter("rep_test")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	StartReporter(ctx, log, 10*time.Millisecond)
	time.Sleep(30 * time.Millisecond)

	// Stop the reporter before reading.
	cancel()
	time.Sleep(10 * time.Millisecond)

	output := lb.String()
	if !strings.Contains(output, "metrics snapshot") {
		t.Error("expected at least one metrics snapshot in log output")
	}
	if !strings.Contains(output, "rep_test") {
		t.Error("expected registered counter in snapshot output")
	}
}

func TestReporter_WithPrefix(t *testing.T) {
	var lb lockedBuffer
	log, _ := logger.NewBuf("test", "info", &lb)

	clearRegistry()
	NewCounter("foo.bar")
	NewCounter("baz.qux")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	StartReporterWithPrefix(ctx, log, "foo", 10*time.Millisecond)
	time.Sleep(30 * time.Millisecond)

	cancel()
	time.Sleep(10 * time.Millisecond)

	output := lb.String()
	if !strings.Contains(output, "foo.bar") {
		t.Error("expected foo.bar in prefixed snapshot")
	}
}

func TestGauge_Snapshot(t *testing.T) {
	g := NewGauge("test_gauge_snap")
	defer clearRegistry()
	g.Set(42)
	snap := g.Snapshot()
	if snap["test_gauge_snap"] != int64(42) {
		t.Errorf("expected 42, got %v", snap["test_gauge_snap"])
	}
}

func TestCounter_Name(t *testing.T) {
	c := NewCounter("name_test")
	defer clearRegistry()
	if c.Name() != "name_test" {
		t.Errorf("expected 'name_test', got %s", c.Name())
	}
}

func TestHistogram_Name(t *testing.T) {
	h := NewHistogram("hist_name")
	defer clearRegistry()
	if h.Name() != "hist_name" {
		t.Errorf("expected 'hist_name', got %s", h.Name())
	}
}

// clearRegistry clears the global registry for test isolation.
func clearRegistry() {
	global.mu.Lock()
	global.metrics = nil
	global.mu.Unlock()
}
