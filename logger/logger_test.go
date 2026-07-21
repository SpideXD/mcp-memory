package logger

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---- Helpers ----

func parseJSON(t *testing.T, data []byte) map[string]interface{} {
	t.Helper()
	var m map[string]interface{}
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("failed to parse JSON %q: %v", string(data), err)
	}
	return m
}

// countLines counts non-empty newline-separated log lines.
func countLines(data []byte) int {
	s := strings.TrimSpace(string(data))
	if s == "" {
		return 0
	}
	return len(strings.Split(s, "\n"))
}

// ---- Construction Edge Cases ----

func TestNew_AllValidLevels(t *testing.T) {
	levels := []struct{
		name   string
		level  string
		logFn  func(*Logger, string, ...any)
	}{
		{"debug", "debug", (*Logger).Debug},
		{"info",  "info",  (*Logger).Info},
		{"warn",  "warn",  (*Logger).Warn},
		{"error", "error", (*Logger).Error},
	}
	for _, tt := range levels {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			log, err := NewBuf("test", tt.level, &buf)
			if err != nil {
				t.Fatalf("NewBuf with level %q failed: %v", tt.level, err)
			}
			tt.logFn(log, "should appear")
			if log.Bytes() == nil || len(log.Bytes()) == 0 {
				t.Error("expected non-empty Bytes()")
			}
		})
	}
}

func TestNew_ValidLevelCaseSensitivity(t *testing.T) {
	_, err := New("test", "INFO")
	if err == nil {
		t.Error("expected error for uppercase 'INFO', parseLevel should be case-sensitive")
	}
}

func TestNew_EmptyModule(t *testing.T) {
	log, err := New("", "info")
	if err != nil {
		t.Fatalf("New with empty module should not error, got: %v", err)
	}
	if log == nil {
		t.Fatal("expected non-nil logger")
	}
}

func TestNew_EmptyLevel(t *testing.T) {
	_, err := New("test", "")
	if err == nil {
		t.Error("expected error for empty level")
	}
}

func TestNew_WhitespaceLevel(t *testing.T) {
	_, err := New("test", "  ")
	if err == nil {
		t.Error("expected error for whitespace level")
	}
}

func TestNew_UnknownLevel(t *testing.T) {
	levels := []string{"trace", "fatal", "panic", "all", "verbose", "1", "true"}
	for _, lvl := range levels {
		t.Run(lvl, func(t *testing.T) {
			_, err := New("test", lvl)
			if err == nil {
				t.Errorf("expected error for level %q", lvl)
			}
		})
	}
}

func TestNewBuf_NilWriter(t *testing.T) {
	_, err := NewBuf("test", "info", nil)
	if err == nil {
		t.Error("expected error for nil writer")
	}
}

func TestNewBuf_NonBufferWriter(t *testing.T) {
	// A strings.Builder implements io.Writer but is not a *bytes.Buffer.
	var sb strings.Builder
	log, err := NewBuf("test", "info", &sb)
	if err != nil {
		t.Fatalf("NewBuf with strings.Builder failed: %v", err)
	}
	log.Info("via strings builder")
	// Bytes() should return nil since we didn't pass a *bytes.Buffer.
	if log.Bytes() != nil {
		t.Error("expected nil Bytes() for non-buffer writer")
	}
}

// ---- Output Structure ----

func TestOutput_RequiredFields(t *testing.T) {
	var buf bytes.Buffer
	log, _ := NewBuf("checker", "info", &buf, WithSource())
	log.Info("hello world")

	m := parseJSON(t, log.Bytes())
	required := []string{"time", "level", "source", "module", "msg"}
	for _, key := range required {
		if _, ok := m[key]; !ok {
			t.Errorf("missing required field %q in log output", key)
		}
	}
}

func TestOutput_ModuleValue(t *testing.T) {
	var buf bytes.Buffer
	log, _ := NewBuf("file-watcher", "info", &buf)
	log.Info("x")

	m := parseJSON(t, log.Bytes())
	if m["module"] != "file-watcher" {
		t.Errorf("expected module 'file-watcher', got %v", m["module"])
	}
}

func TestOutput_LevelValues(t *testing.T) {
	tests := []struct {
		method   func(*Logger, string, ...any)
		logFunc  string
		expected string
	}{
		{(*Logger).Debug, "Debug", "DEBUG"},
		{(*Logger).Info, "Info", "INFO"},
		{(*Logger).Warn, "Warn", "WARN"},
		{(*Logger).Error, "Error", "ERROR"},
	}

	for _, tt := range tests {
		t.Run(tt.logFunc, func(t *testing.T) {
			var buf bytes.Buffer
			log, _ := NewBuf("lev", "debug", &buf)
			tt.method(log, "msg")
			m := parseJSON(t, log.Bytes())
			if m["level"] != tt.expected {
				t.Errorf("expected level %q, got %v", tt.expected, m["level"])
			}
		})
	}
}

func TestOutput_SourceStructure(t *testing.T) {
	var buf bytes.Buffer
	log, _ := NewBuf("src", "debug", &buf)
	log.Info("check source") // line ~N

	m := parseJSON(t, log.Bytes())
	src, ok := m["source"]
	if !ok {
		t.Fatal("missing source field")
	}
	srcMap, ok := src.(map[string]interface{})
	if !ok {
		t.Fatalf("source expected object, got %T", src)
	}

	// source.file should end with the test file name.
	file, ok := srcMap["file"].(string)
	if !ok {
		t.Fatalf("source.file expected string, got %T", srcMap["file"])
	}
	if !strings.HasSuffix(file, "logger_test.go") {
		t.Errorf("expected source.file to end with logger_test.go, got %q", file)
	}

	// source.line should be a number.
	line, ok := srcMap["line"].(float64)
	if !ok {
		t.Fatalf("source.line expected number, got %T", srcMap["line"])
	}
	if line < 1 {
		t.Errorf("expected positive source.line, got %f", line)
	}

	// source.function should be present and contain package path.
	fn, ok := srcMap["function"].(string)
	if !ok {
		t.Fatalf("source.function expected string, got %T", srcMap["function"])
	}
	if !strings.Contains(fn, "logger.TestOutput_SourceStructure") {
		t.Errorf("expected source.function to contain test name, got %q", fn)
	}
}

// ---- Message Variations ----

func TestSource_FunctionFullPath(t *testing.T) {
	var buf bytes.Buffer
	log, _ := NewBuf("src", "info", &buf, WithSource())
	log.Info("check source function")

	m := parseJSON(t, log.Bytes())
	src, ok := m["source"].(map[string]interface{})
	if !ok {
		t.Fatalf("source expected object, got %T", m["source"])
	}
	fn, ok := src["function"].(string)
	if !ok {
		t.Fatalf("source.function expected string, got %T", src["function"])
	}
	// Function should be the fully-qualified package path.
	if !strings.HasPrefix(fn, "mcp-memory/logger.") {
		t.Errorf("expected full package path, got %q", fn)
	}
	if !strings.HasSuffix(fn, "TestSource_FunctionFullPath") {
		t.Errorf("expected function name to end with test name, got %q", fn)
	}
}

func TestMsg_Empty(t *testing.T) {
	var buf bytes.Buffer
	log, _ := NewBuf("msg", "info", &buf)
	log.Info("")

	m := parseJSON(t, log.Bytes())
	if m["msg"] != "" {
		t.Errorf("expected empty msg, got %q", m["msg"])
	}
}

func TestMsg_Unicode(t *testing.T) {
	var buf bytes.Buffer
	log, _ := NewBuf("msg", "info", &buf)
	log.Info("héllo wörld 🎉")

	m := parseJSON(t, log.Bytes())
	if m["msg"] != "héllo wörld 🎉" {
		t.Errorf("expected unicode msg, got %q", m["msg"])
	}
}

func TestMsg_Newlines(t *testing.T) {
	var buf bytes.Buffer
	log, _ := NewBuf("msg", "info", &buf)
	msg := "line1\nline2\nline3"
	log.Info(msg)

	m := parseJSON(t, log.Bytes())
	if m["msg"] != msg {
		t.Errorf("expected multi-line msg, got %q", m["msg"])
	}
}

// ---- Attribute Edge Cases ----

func TestAttrs_None(t *testing.T) {
	var buf bytes.Buffer
	log, _ := NewBuf("attr", "info", &buf)
	log.Info("bare")

	m := parseJSON(t, log.Bytes())
	// Should have only the standard fields, no extras.
	for k := range m {
		switch k {
		case "time", "level", "source", "module", "msg":
			continue
		default:
			t.Errorf("unexpected extra key %q in bare log", k)
		}
	}
}

func TestAttrs_Many(t *testing.T) {
	var buf bytes.Buffer
	log, _ := NewBuf("attr", "info", &buf)

	// 20 attributes — slog should handle this fine.
	log.Info("many",
		File("a.go"), File("b.go"), // duplicate key — last wins
		Count(1), Count(2), Count(3),
		Port(8080), PID(99),
		Duration(time.Second),
	)

	m := parseJSON(t, log.Bytes())
	// Last value for duplicate key "file" should be "b.go".
	if m["file"] != "b.go" {
		t.Errorf("duplicate attribute: expected last key 'b.go', got %v", m["file"])
	}
	// Last value for duplicate key "count" should be 3.
	if m["count"] != float64(3) {
		t.Errorf("duplicate attribute: expected last count 3, got %v", m["count"])
	}
}

func TestAttrs_NilValue(t *testing.T) {
	var buf bytes.Buffer
	log, _ := NewBuf("attr", "info", &buf)
	log.Info("nil val", "nil_key", nil)

	m := parseJSON(t, log.Bytes())
	// slog serializes nil in different ways depending on version.
	if _, ok := m["nil_key"]; !ok {
		t.Error("expected nil_key to appear in output even with nil value")
	}
}

func TestAttrs_RawStringKey(t *testing.T) {
	var buf bytes.Buffer
	log, _ := NewBuf("attr", "info", &buf)
	log.Info("raw", "my_custom_key", "my_custom_value")

	m := parseJSON(t, log.Bytes())
	if m["my_custom_key"] != "my_custom_value" {
		t.Errorf("expected custom key 'my_custom_value', got %v", m["my_custom_key"])
	}
}

// ---- With() Edge Cases ----

func TestWith_MixedTypes(t *testing.T) {
	var buf bytes.Buffer
	log, _ := NewBuf("with", "info", &buf)

	child := log.With(
		"string_key", "hello",
		"int_key", 42,
		"bool_key", true,
		"float_key", 3.14,
		"nil_key", nil,
	)
	child.Info("mixed types")

	m := parseJSON(t, child.Bytes())

	if m["string_key"] != "hello" {
		t.Errorf("expected 'hello', got %v", m["string_key"])
	}
	if m["int_key"] != float64(42) {
		t.Errorf("expected 42, got %v", m["int_key"])
	}
	if m["bool_key"] != true {
		t.Errorf("expected true, got %v", m["bool_key"])
	}
	if m["float_key"] != 3.14 {
		t.Errorf("expected 3.14, got %v", m["float_key"])
	}
	// nil may serialize as null or be omitted depending on slog version.
	if _, ok := m["nil_key"]; !ok {
		t.Log("note: nil_key was omitted (slog may skip nil values in With())")
	}
}

func TestWith_ZeroArgs(t *testing.T) {
	var buf bytes.Buffer
	log, _ := NewBuf("with", "info", &buf)
	child := log.With() // no args
	child.Info("zero")

	m := parseJSON(t, child.Bytes())
	if m["module"] != "with" {
		t.Errorf("expected module to survive zero-arg With(), got %v", m["module"])
	}
}

func TestWith_OddArgs(t *testing.T) {
	var buf bytes.Buffer
	log, _ := NewBuf("with", "info", &buf)
	// slog.With with odd args: the last key has no value, which slog handles
	// by using "(MISSING)" as the value.
	child := log.With("key1", "val1", "orphan")
	child.Info("odd")

	m := parseJSON(t, child.Bytes())
	if m["key1"] != "val1" {
		t.Errorf("expected key1 'val1', got %v", m["key1"])
	}
	// slog may or may not include the orphan key depending on version.
	// We just verify it doesn't panic.
}

func TestWith_EmptyValues(t *testing.T) {
	var buf bytes.Buffer
	log, _ := NewBuf("with", "info", &buf)
	child := log.With("trace_id", "", "msg2", "")
	child.Info("empty vals")

	m := parseJSON(t, child.Bytes())
	if m["trace_id"] != "" {
		t.Errorf("expected empty trace_id, got %q", m["trace_id"])
	}
}

func TestWithTrace_Empty(t *testing.T) {
	var buf bytes.Buffer
	log, _ := NewBuf("trace", "info", &buf)
	log.WithTrace("").Info("empty trace")

	m := parseJSON(t, log.Bytes())
	if m["trace_id"] != "" {
		t.Errorf("expected empty trace_id, got %q", m["trace_id"])
	}
}

func TestWithTrace_LongString(t *testing.T) {
	var buf bytes.Buffer
	log, _ := NewBuf("trace", "info", &buf)
	long := strings.Repeat("x", 10000)
	log.WithTrace(long).Info("long trace")

	m := parseJSON(t, log.Bytes())
	if m["trace_id"] != long {
		t.Errorf("expected trace_id to match, got length %d vs %d", len(fmt.Sprintf("%v", m["trace_id"])), len(long))
	}
}

func TestWith_Chained(t *testing.T) {
	var buf bytes.Buffer
	log, _ := NewBuf("chain", "info", &buf)

	l1 := log.With("a", 1)
	l2 := l1.With("b", 2)
	l3 := l2.With("c", 3)
	l3.Info("chained")

	m := parseJSON(t, l3.Bytes())
	if m["a"] != float64(1) {
		t.Errorf("expected a=1, got %v", m["a"])
	}
	if m["b"] != float64(2) {
		t.Errorf("expected b=2, got %v", m["b"])
	}
	if m["c"] != float64(3) {
		t.Errorf("expected c=3, got %v", m["c"])
	}
}

func TestWith_OriginalUnchanged(t *testing.T) {
	var buf bytes.Buffer
	log, _ := NewBuf("orig", "info", &buf)

	_ = log.With("should_not_appear", "x")
	log.Info("original should be clean")

	m := parseJSON(t, log.Bytes())
	if _, ok := m["should_not_appear"]; ok {
		t.Error("original logger should not have child's attributes")
	}
}

// ---- Bytes() Edge Cases ----

func TestBytes_AfterMultipleWrites(t *testing.T) {
	var buf bytes.Buffer
	log, _ := NewBuf("bytes", "info", &buf)

	log.Info("first")
	log.Warn("second")
	log.Error("third")

	lines := countLines(log.Bytes())
	if lines != 3 {
		t.Errorf("expected 3 log lines, got %d", lines)
	}
}

func TestBytes_AfterClear(t *testing.T) {
	var buf bytes.Buffer
	log, _ := NewBuf("bytes", "info", &buf)

	log.Info("before")
	buf.Reset()
	log.Info("after")

	if !strings.Contains(string(log.Bytes()), "after") {
		t.Errorf("expected 'after' in log output after reset")
	}
	if strings.Contains(string(log.Bytes()), "before") {
		t.Error("expected 'before' NOT in log output after reset")
	}
}

func TestBytes_EmptyBuffer(t *testing.T) {
	var buf bytes.Buffer
	log, _ := NewBuf("empty", "info", &buf)
	// No log calls — buffer is empty.
	if len(log.Bytes()) != 0 {
		t.Errorf("expected empty Bytes() when nothing logged, got %d bytes", len(log.Bytes()))
	}
}

func TestBytes_DiscardWriter(t *testing.T) {
	log, _ := NewBuf("discard", "info", bytes.NewBuffer(nil))
	log.Info("to discard")
	if log.Bytes() == nil {
		t.Error("expected non-nil Bytes() for *bytes.Buffer passed to NewBuf")
	}
}

// ---- Level Filtering Edge Cases ----

func TestLevelFilter_DebugBlocksNothing(t *testing.T) {
	var buf bytes.Buffer
	log, _ := NewBuf("filter", "debug", &buf)

	log.Debug("d")
	log.Info("i")
	log.Warn("w")
	log.Error("e")

	if countLines(log.Bytes()) != 4 {
		t.Errorf("expected 4 lines at debug level, got %d", countLines(log.Bytes()))
	}
}

func TestLevelFilter_InfoBlocksDebug(t *testing.T) {
	var buf bytes.Buffer
	log, _ := NewBuf("filter", "info", &buf)

	log.Debug("hidden")
	log.Info("visible")
	log.Warn("visible")
	log.Error("visible")

	if countLines(log.Bytes()) != 3 {
		t.Errorf("expected 3 lines at info level, got %d", countLines(log.Bytes()))
	}
}

func TestLevelFilter_WarnBlocksInfo(t *testing.T) {
	var buf bytes.Buffer
	log, _ := NewBuf("filter", "warn", &buf)

	log.Debug("hidden")
	log.Info("hidden")
	log.Warn("visible")
	log.Error("visible")

	if countLines(log.Bytes()) != 2 {
		t.Errorf("expected 2 lines at warn level, got %d", countLines(log.Bytes()))
	}
}

func TestLevelFilter_ErrorBlocksAllButError(t *testing.T) {
	var buf bytes.Buffer
	log, _ := NewBuf("filter", "error", &buf)

	log.Debug("hidden")
	log.Info("hidden")
	log.Warn("hidden")
	log.Error("visible")

	if countLines(log.Bytes()) != 1 {
		t.Errorf("expected 1 line at error level, got %d", countLines(log.Bytes()))
	}
}

// ---- Attribute Type Helpers ----

func TestAttr_Duration(t *testing.T) {
	var buf bytes.Buffer
	log, _ := NewBuf("attr", "info", &buf)
	log.Info("dur", Duration(0))

	m := parseJSON(t, log.Bytes())
	if m["duration"] != "0s" {
		t.Errorf("expected '0s', got %v", m["duration"])
	}
}

func TestAttr_DurationNegative(t *testing.T) {
	var buf bytes.Buffer
	log, _ := NewBuf("attr", "info", &buf)
	log.Info("neg", Duration(-5*time.Second))

	m := parseJSON(t, log.Bytes())
	if m["duration"] != "-5s" {
		t.Errorf("expected '-5s', got %v", m["duration"])
	}
}

func TestAttr_ErrorNil(t *testing.T) {
	var buf bytes.Buffer
	log, _ := NewBuf("attr", "info", &buf)
	log.Info("nil err", Error(nil))

	// slog serializes a nil error as null.
	m := parseJSON(t, log.Bytes())
	if m["error"] != nil {
		t.Errorf("expected nil error to serialize as null, got %v", m["error"])
	}
}

func TestAttr_CountZero(t *testing.T) {
	var buf bytes.Buffer
	log, _ := NewBuf("attr", "info", &buf)
	log.Info("zero", Count(0))

	m := parseJSON(t, log.Bytes())
	if m["count"] != float64(0) {
		t.Errorf("expected count 0, got %v", m["count"])
	}
}

func TestAttr_CountNegative(t *testing.T) {
	var buf bytes.Buffer
	log, _ := NewBuf("attr", "info", &buf)
	log.Info("neg", Count(-1))

	m := parseJSON(t, log.Bytes())
	if m["count"] != float64(-1) {
		t.Errorf("expected count -1, got %v", m["count"])
	}
}

func TestAttr_FileEmpty(t *testing.T) {
	var buf bytes.Buffer
	log, _ := NewBuf("attr", "info", &buf)
	log.Info("empty", File(""))

	m := parseJSON(t, log.Bytes())
	if m["file"] != "" {
		t.Errorf("expected empty file, got %q", m["file"])
	}
}

func TestAttr_PortZero(t *testing.T) {
	var buf bytes.Buffer
	log, _ := NewBuf("attr", "info", &buf)
	log.Info("zero port", Port(0))

	m := parseJSON(t, log.Bytes())
	if m["port"] != float64(0) {
		t.Errorf("expected port 0, got %v", m["port"])
	}
}

func TestAttr_PIDZero(t *testing.T) {
	var buf bytes.Buffer
	log, _ := NewBuf("attr", "info", &buf)
	log.Info("zero pid", PID(0))

	m := parseJSON(t, log.Bytes())
	if m["pid"] != float64(0) {
		t.Errorf("expected pid 0, got %v", m["pid"])
	}
}

// ---- Concurrency ----

func TestConcurrent_LogWrites(t *testing.T) {
	var buf bytes.Buffer
	log, _ := NewBuf("concurrent", "info", &buf)

	var wg sync.WaitGroup
	n := 50
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			log.Info("concurrent msg", Count(id))
		}(i)
	}
	wg.Wait()

	lines := countLines(log.Bytes())
	if lines != n {
		t.Errorf("expected %d log lines, got %d", n, lines)
	}
}

func TestConcurrent_WithAndLog(t *testing.T) {
	var buf bytes.Buffer
	log, _ := NewBuf("concurrent", "info", &buf)

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			child := log.With("goroutine", id)
			child.Info("from goroutine", Count(id))
		}(i)
	}
	wg.Wait()

	lines := countLines(log.Bytes())
	if lines != 50 {
		t.Errorf("expected 50 log lines, got %d", lines)
	}
}

func TestConcurrent_ReadBytesWhileLogging(t *testing.T) {
	// bytes.Buffer is not safe for concurrent read+write, so this test
	// only verifies that concurrent log writes don't panic.
	// Bytes() is called only after all writes finish.
	var buf bytes.Buffer
	log, _ := NewBuf("concurrent", "info", &buf)

	var wg sync.WaitGroup
	stop := make(chan struct{})

	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					log.Info("spam")
				}
			}
		}()
	}

	time.Sleep(10 * time.Millisecond)
	close(stop)
	wg.Wait()

	// After all writes complete, Bytes() should return valid output.
	if len(log.Bytes()) == 0 {
		t.Error("expected non-empty Bytes() after concurrent writes")
	}
}

// ---- GenerateTraceID ----

func TestTraceID_Uniqueness(t *testing.T) {
	const n = 1000
	ids := make(map[string]bool, n)
	for i := 0; i < n; i++ {
		id, err := GenerateTraceID()
		if err != nil {
			t.Fatalf("GenerateTraceID: %v", err)
		}
		if ids[id] {
			t.Fatalf("duplicate trace ID after %d iterations: %s", i, id)
		}
		ids[id] = true
	}
}

func TestTraceID_Format(t *testing.T) {
	id, err := GenerateTraceID()
	if err != nil {
		t.Fatalf("GenerateTraceID: %v", err)
	}
	if len(id) != 16 {
		t.Errorf("expected 16 hex chars, got %d: %q", len(id), id)
	}
	for _, c := range id {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			t.Errorf("unexpected character %q in trace ID %q", c, id)
		}
	}
}

// ---- Duration Measured ----

func TestDuration_Measured(t *testing.T) {
	var buf bytes.Buffer
	log, _ := NewBuf("time", "info", &buf)

	start := time.Now()
	// Simulate a small amount of work.
	time.Sleep(time.Millisecond)
	elapsed := time.Since(start)

	log.Info("operation completed", Duration(elapsed))

	m := parseJSON(t, log.Bytes())
	dur, ok := m["duration"].(string)
	if !ok {
		t.Fatalf("expected duration string, got %T", m["duration"])
	}
	// Duration should be parseable and should be >= 1ms.
	d, err := time.ParseDuration(dur)
	if err != nil {
		t.Fatalf("duration %q is not a valid duration: %v", dur, err)
	}
	if d < time.Millisecond {
		t.Errorf("expected duration >= 1ms, got %v", d)
	}
	if d > time.Second {
		t.Errorf("expected duration < 1s, got %v (seems like a bug)", d)
	}
}

func TestDuration_SubMicrosecond(t *testing.T) {
	var buf bytes.Buffer
	log, _ := NewBuf("time", "info", &buf)

	// Elapsed time so small it rounds to zero.
	start := time.Now()
	_ = start // no work
	elapsed := time.Since(start)

	log.Info("instant", Duration(elapsed))

	m := parseJSON(t, log.Bytes())
	dur, ok := m["duration"].(string)
	if !ok {
		t.Fatalf("expected duration string, got %T", m["duration"])
	}
	// Should be a valid duration. Could be "0s" for sub-microsecond work.
	_, err := time.ParseDuration(dur)
	if err != nil {
		t.Fatalf("duration %q is not valid: %v", dur, err)
	}
}

// ---- Error Handling ----

func TestErrorAttr_Structured(t *testing.T) {
	var buf bytes.Buffer
	log, _ := NewBuf("err", "info", &buf)

	err := fmt.Errorf("wrapped: %w", ErrTest)
	log.Error("structured err", Error(err))

	m := parseJSON(t, log.Bytes())
	errVal, ok := m["error"]
	if !ok {
		t.Fatal("expected error field")
	}
	errStr, ok := errVal.(string)
	if !ok {
		t.Fatalf("error expected string, got %T", errVal)
	}
	if !strings.Contains(errStr, "wrapped:") || !strings.Contains(errStr, "test error") {
		t.Errorf("expected wrapped error in output, got %q", errStr)
	}
}

func TestErrorAttr_StructWithFields(t *testing.T) {
	var buf bytes.Buffer
	log, _ := NewBuf("err", "info", &buf)

	err := &detailedError{
		Code:    404,
		Message: "not found",
		Inner:   fmt.Errorf("underlying: %w", ErrTest),
	}
	log.Error("request failed", Error(err))

	m := parseJSON(t, log.Bytes())
	errVal, ok := m["error"]
	if !ok {
		t.Fatal("expected error field")
	}
	errStr, ok := errVal.(string)
	if !ok {
		t.Fatalf("error expected string, got %T", errVal)
	}
	// slog serializes the Error() string, which should contain fields.
	if !strings.Contains(errStr, "404") {
		t.Errorf("expected error string to contain Code 404, got %q", errStr)
	}
	if !strings.Contains(errStr, "not found") {
		t.Errorf("expected error string to contain 'not found', got %q", errStr)
	}
	if !strings.Contains(errStr, "underlying:") || !strings.Contains(errStr, "test error") {
		t.Errorf("expected error string to contain wrapped error, got %q", errStr)
	}
}

func TestErrorAttr_ErrorMessageHandlesNoError(t *testing.T) {
	var buf bytes.Buffer
	log, _ := NewBuf("err", "info", &buf)

	log.Error("no error arg")
	// Just ensure it doesn't panic and has the msg.
	m := parseJSON(t, log.Bytes())
	if m["msg"] != "no error arg" {
		t.Errorf("expected msg 'no error arg', got %q", m["msg"])
	}
}

// ---- JSON Validity ----

func TestOutput_ValidJSONPerLine(t *testing.T) {
	var buf bytes.Buffer
	log, _ := NewBuf("json", "info", &buf)

	log.Info("line 1")
	log.Warn("line 2")
	log.Error("line 3")

	lines := strings.Split(strings.TrimSpace(string(log.Bytes())), "\n")
	for i, line := range lines {
		if !json.Valid([]byte(line)) {
			t.Errorf("line %d is not valid JSON: %q", i+1, line)
		}
	}
}

func TestOutput_NoStdout(t *testing.T) {
	// New() uses os.Stderr. We can't directly test that it writes to stderr
	// vs stdout without capturing stdout, but we can verify the handler
	// options are correct by checking the JSON output structure.
	var buf bytes.Buffer
	log, _ := NewBuf("stderr-test", "info", &buf)
	log.Info("check")

	m := parseJSON(t, log.Bytes())
	// Module should be present — if handler was misconfigured, this would fail.
	if m["module"] != "stderr-test" {
		t.Errorf("expected module 'stderr-test', got %v", m["module"])
	}
}

// sentinel error for testing
var ErrTest = &errTest{}

type errTest struct{}

func (e *errTest) Error() string { return "test error" }

// detailedError is a structured error type used to test Error attr with custom structs.
type detailedError struct {
	Code    int
	Message string
	Inner   error
}

func (e *detailedError) Error() string {
	return fmt.Sprintf("code=%d msg=%s inner=%v", e.Code, e.Message, e.Inner)
}

// ---- New Attr Types ----

func TestAttr_ProjectID(t *testing.T) {
	var buf bytes.Buffer
	log, _ := NewBuf("attr", "info", &buf)
	log.Info("project", ProjectID("backend"))

	m := parseJSON(t, log.Bytes())
	if m["project"] != "backend" {
		t.Errorf("expected 'backend', got %v", m["project"])
	}
}

func TestAttr_Reason(t *testing.T) {
	var buf bytes.Buffer
	log, _ := NewBuf("attr", "info", &buf)
	log.Info("removed", Reason("ttl"))

	m := parseJSON(t, log.Bytes())
	if m["reason"] != "ttl" {
		t.Errorf("expected 'ttl', got %v", m["reason"])
	}
}

func TestAttr_Binary(t *testing.T) {
	var buf bytes.Buffer
	log, _ := NewBuf("attr", "info", &buf)
	log.Info("spawn", Binary("/usr/bin/mcp-memory"))

	m := parseJSON(t, log.Bytes())
	if m["binary"] != "/usr/bin/mcp-memory" {
		t.Errorf("expected '/usr/bin/mcp-memory', got %v", m["binary"])
	}
}

func TestAttr_Idle(t *testing.T) {
	var buf bytes.Buffer
	log, _ := NewBuf("attr", "info", &buf)
	log.Info("idle check", Idle(5*time.Minute))

	m := parseJSON(t, log.Bytes())
	if m["idle"] != "5m0s" {
		t.Errorf("expected '5m0s', got %v", m["idle"])
	}
}

func TestAttr_TTL(t *testing.T) {
	var buf bytes.Buffer
	log, _ := NewBuf("attr", "info", &buf)
	log.Info("ttl config", TTL(30*time.Minute))

	m := parseJSON(t, log.Bytes())
	if m["ttl"] != "30m0s" {
		t.Errorf("expected '30m0s', got %v", m["ttl"])
	}
}

func TestAttr_HealthWait(t *testing.T) {
	var buf bytes.Buffer
	log, _ := NewBuf("attr", "info", &buf)
	log.Info("health", HealthWait(5*time.Second))

	m := parseJSON(t, log.Bytes())
	if m["health_wait"] != "5s" {
		t.Errorf("expected '5s', got %v", m["health_wait"])
	}
}

// ---- Context Propagation ----

func TestWithTraceID_RoundTrip(t *testing.T) {
	ctx := WithTraceID(context.Background(), "abc123")
	got := TraceIDFromContext(ctx)
	if got != "abc123" {
		t.Errorf("expected 'abc123', got %q", got)
	}
}

func TestWithTraceID_EmptyID(t *testing.T) {
	ctx := WithTraceID(context.Background(), "")
	// Should return the same context (no value set).
	got := TraceIDFromContext(ctx)
	if got != "" {
		t.Errorf("expected empty, got %q", got)
	}
}

func TestTraceIDFromContext_NilContext(t *testing.T) {
	got := TraceIDFromContext(nil)
	if got != "" {
		t.Errorf("expected empty for nil context, got %q", got)
	}
}

func TestTraceIDFromContext_NoValue(t *testing.T) {
	got := TraceIDFromContext(context.Background())
	if got != "" {
		t.Errorf("expected empty for background context, got %q", got)
	}
}

func TestLogger_WithContext(t *testing.T) {
	var buf bytes.Buffer
	log, _ := NewBuf("ctx", "info", &buf)

	ctx := WithTraceID(context.Background(), "trace-42")
	log.WithContext(ctx).Info("from context")

	m := parseJSON(t, log.Bytes())
	if m["trace_id"] != "trace-42" {
		t.Errorf("expected trace_id 'trace-42', got %v", m["trace_id"])
	}
}

func TestLogger_WithContext_NilContext(t *testing.T) {
	var buf bytes.Buffer
	log, _ := NewBuf("ctx", "info", &buf)

	// Should not panic and should not add trace_id.
	log.WithContext(nil).Info("nil context")

	m := parseJSON(t, log.Bytes())
	if _, ok := m["trace_id"]; ok {
		t.Error("expected no trace_id for nil context")
	}
}

func TestLogger_WithContext_NoTraceID(t *testing.T) {
	var buf bytes.Buffer
	log, _ := NewBuf("ctx", "info", &buf)

	// Empty context — should not add trace_id.
	log.WithContext(context.Background()).Info("no trace")

	m := parseJSON(t, log.Bytes())
	if _, ok := m["trace_id"]; ok {
		t.Error("expected no trace_id for empty context")
	}
}

func TestLogger_WithContext_EmptyTraceID(t *testing.T) {
	var buf bytes.Buffer
	log, _ := NewBuf("ctx", "info", &buf)

	ctx := WithTraceID(context.Background(), "")
	log.WithContext(ctx).Info("empty trace")

	m := parseJSON(t, log.Bytes())
	if _, ok := m["trace_id"]; ok {
		t.Error("expected no trace_id for empty trace ID")
	}
}

// ---- Option Pattern ----

func TestOption_WithSource(t *testing.T) {
	var buf bytes.Buffer
	log, _ := NewBuf("opt", "info", &buf, WithSource())
	log.Info("with source")

	m := parseJSON(t, log.Bytes())
	if _, ok := m["source"]; !ok {
		t.Error("expected source field with WithSource()")
	}
}

func TestOption_WithoutSource(t *testing.T) {
	var buf bytes.Buffer
	log, _ := NewBuf("opt", "info", &buf, WithoutSource())
	log.Info("without source")

	m := parseJSON(t, log.Bytes())
	if _, ok := m["source"]; ok {
		t.Error("expected no source field with WithoutSource()")
	}
}

func TestOption_DefaultHasSource(t *testing.T) {
	var buf bytes.Buffer
	log, _ := NewBuf("opt", "info", &buf)
	log.Info("default")

	m := parseJSON(t, log.Bytes())
	if _, ok := m["source"]; !ok {
		t.Error("expected source field by default")
	}
}

// ---- Benchmarks ----

func BenchmarkLoggerInfo(b *testing.B) {
	log, _ := New("bench", "info")
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		log.Info("benchmark message", File("bench.go"), Count(i))
	}
}

func BenchmarkLoggerWith(b *testing.B) {
	log, _ := New("bench", "info")
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		child := log.With("trace_id", "abc123", "request_id", i)
		child.Info("bench")
	}
}

func BenchmarkLoggerWithTrace(b *testing.B) {
	log, _ := New("bench", "info")
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		log.WithTrace("abc123").Info("bench")
	}
}

func BenchmarkLoggerBufInfo(b *testing.B) {
	var buf bytes.Buffer
	log, _ := NewBuf("bench", "info", &buf)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		log.Info("bench", Count(i))
	}
	b.StopTimer()
	buf.Reset()
}
