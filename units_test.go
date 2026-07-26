package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// Units that had zero coverage: the alert client, the PID file used for orphan
// recovery, the cloud-embedding detection, and the subprocess environment
// builders. None of these need a live subprocess, they were simply never
// exercised.

// ─── AlertClient ─────────────────────────────────────────────────────────────

func TestUnit_NewAlertClientNilWhenNoURL(t *testing.T) {
	if got := NewAlertClient("", "required"); got != nil {
		t.Fatal("empty ALERT_URL must yield a nil client (alerting disabled)")
	}
	if got := NewAlertClient("http://example.invalid", "optional"); got == nil {
		t.Fatal("a configured URL must yield a client")
	}
}

// TestUnit_AlertClientNilSafety is the property the call sites depend on:
// s.alerts is nil whenever alerting is disabled, and every method is invoked
// unconditionally.
func TestUnit_AlertClientNilSafety(t *testing.T) {
	var a *AlertClient
	a.Send(AlertCritical, "must not panic", map[string]interface{}{"k": "v"})
	if a.IsRequired() {
		t.Error("a nil client must not report itself required")
	}
}

func TestUnit_AlertClientIsRequired(t *testing.T) {
	if NewAlertClient("http://x", "required").IsRequired() != true {
		t.Error(`mode "required" must report required`)
	}
	if NewAlertClient("http://x", "optional").IsRequired() != false {
		t.Error(`mode "optional" must not report required`)
	}
	if NewAlertClient("http://x", "").IsRequired() != false {
		t.Error("an unset mode must default to not-required")
	}
}

func TestUnit_AlertClientSendsStructuredPayload(t *testing.T) {
	var mu sync.Mutex
	var got Alert

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/alert" {
			t.Errorf("alerts must POST to /alert, got %s", r.URL.Path)
		}
		mu.Lock()
		json.NewDecoder(r.Body).Decode(&got)
		mu.Unlock()
		w.WriteHeader(200)
	}))
	defer srv.Close()

	NewAlertClient(srv.URL, "optional").
		Send(AlertError, "disk on fire", map[string]interface{}{"free_mb": 3})

	mu.Lock()
	defer mu.Unlock()
	if got.Service != "memory" {
		t.Errorf("service = %q, want memory", got.Service)
	}
	if got.Level != AlertError {
		t.Errorf("level = %q, want error", got.Level)
	}
	if got.Message != "disk on fire" {
		t.Errorf("message = %q", got.Message)
	}
	if fmt.Sprintf("%v", got.Details["free_mb"]) != "3" {
		t.Errorf("details lost: %v", got.Details)
	}
}

// TestUnit_AlertClientSurvivesUnreachableEndpoint pins the optional-mode
// contract: a dead alert endpoint must never take down the caller.
func TestUnit_AlertClientSurvivesUnreachableEndpoint(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	srv.Close()

	done := make(chan struct{})
	go func() {
		defer close(done)
		NewAlertClient(srv.URL, "optional").Send(AlertWarn, "into the void", nil)
	}()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Send blocked on an unreachable endpoint")
	}
}

func TestUnit_AlertClientCheckHealth(t *testing.T) {
	t.Run("healthy", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(200)
		}))
		defer srv.Close()
		if err := NewAlertClient(srv.URL, "required").CheckHealth(); err != nil {
			t.Errorf("healthy endpoint reported an error: %v", err)
		}
	})

	t.Run("non-200", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(503)
		}))
		defer srv.Close()
		if err := NewAlertClient(srv.URL, "required").CheckHealth(); err == nil {
			t.Error("a 503 alert endpoint must report an error")
		}
	})

	t.Run("unreachable", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
		srv.Close()
		if err := NewAlertClient(srv.URL, "required").CheckHealth(); err == nil {
			t.Error("an unreachable endpoint must report an error")
		}
	})
}

// ─── PID file / orphan recovery ──────────────────────────────────────────────

// withWorkingDir runs fn with the process CWD moved to dir. The PID helpers
// resolve paths from os.Getwd(), so this is the only way to exercise them
// without writing into the repo.
func withWorkingDir(t *testing.T, dir string, fn func()) {
	t.Helper()
	orig, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	defer func() {
		if err := os.Chdir(orig); err != nil {
			t.Fatalf("restore cwd: %v", err)
		}
	}()
	fn()
}

func TestUnit_SavePidsSkipsWhenNoChildren(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "logs"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	svc := newServices(Config{}, testLogger(), nil)

	withWorkingDir(t, dir, func() {
		svc.savePids() // no llamaCmd / cogneeCmd
		if _, err := os.Stat(filepath.Join(dir, "logs/.mcp-pids.json")); !os.IsNotExist(err) {
			t.Error("no children means no PID file should be written")
		}
	})
}

func TestUnit_ClearPidsRemovesFileAndIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	logs := filepath.Join(dir, "logs")
	if err := os.MkdirAll(logs, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	path := filepath.Join(logs, ".mcp-pids.json")
	if err := os.WriteFile(path, []byte(`{"llama":123}`), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	svc := newServices(Config{}, testLogger(), nil)
	withWorkingDir(t, dir, func() {
		svc.clearPids()
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Error("clearPids did not remove the PID file")
		}
		svc.clearPids() // must not panic on a missing file
	})
}

// TestUnit_CleanupOrphansHandlesHostileFiles covers the paths a crashed or
// tampered-with PID file can produce. A panic here happens at startup, before
// any logging exists, so it must be impossible.
func TestUnit_CleanupOrphansHandlesHostileFiles(t *testing.T) {
	cases := []struct {
		name    string
		content string
	}{
		{"absent", ""},
		{"empty", ``},
		{"not JSON", `{{{not json`},
		{"wrong shape", `["llama", 1]`},
		{"negative pid", `{"llama":-1}`},
		{"zero pid", `{"llama":0}`},
		{"pid 1 (init)", `{"llama":1}`},
		{"absurd pid", `{"llama":999999999}`},
		{"string pid", `{"llama":"abc"}`},
		{"null value", `{"llama":null}`},
		{"empty object", `{}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			logs := filepath.Join(dir, "logs")
			if err := os.MkdirAll(logs, 0o755); err != nil {
				t.Fatalf("mkdir: %v", err)
			}
			if tc.name != "absent" {
				if err := os.WriteFile(filepath.Join(logs, ".mcp-pids.json"),
					[]byte(tc.content), 0o644); err != nil {
					t.Fatalf("write: %v", err)
				}
			}
			withWorkingDir(t, dir, func() {
				cleanupOrphans() // must not panic and must not kill anything unrelated
			})
		})
	}
}

// TestUnit_CleanupOrphansIgnoresRecycledPid is the safety property that keeps
// orphan recovery from becoming a hazard: a PID recorded under one name must
// not be killed once it belongs to a different process. The test process is
// alive, is not a child of ours, and does not match the recorded name — so it
// must survive. Without the identity check this test kills the test binary.
func TestUnit_CleanupOrphansIgnoresRecycledPid(t *testing.T) {
	dir := t.TempDir()
	logs := filepath.Join(dir, "logs")
	if err := os.MkdirAll(logs, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// Record our own PID under a name we certainly do not have.
	body := fmt.Sprintf(`{"llama":{"pid":%d,"name":"llama-server"}}`, os.Getpid())
	if err := os.WriteFile(filepath.Join(logs, ".mcp-pids.json"), []byte(body), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	withWorkingDir(t, dir, func() {
		cleanupOrphans()
	})
	// Reaching here means the recycled PID was not signalled.
}

// TestUnit_CleanupOrphansSkipsLegacyEntriesWithoutName covers the old
// {"llama":N} file format: with no recorded name the entry cannot be
// identity-checked, so it must be skipped rather than killed on faith.
func TestUnit_CleanupOrphansSkipsLegacyEntriesWithoutName(t *testing.T) {
	dir := t.TempDir()
	logs := filepath.Join(dir, "logs")
	if err := os.MkdirAll(logs, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	body := fmt.Sprintf(`{"llama":%d}`, os.Getpid())
	if err := os.WriteFile(filepath.Join(logs, ".mcp-pids.json"), []byte(body), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	withWorkingDir(t, dir, func() {
		cleanupOrphans()
	})
	// Surviving proves a nameless legacy entry is not blindly killed.
}

// TestUnit_ParsePidFileFormats pins both accepted encodings.
func TestUnit_ParsePidFileFormats(t *testing.T) {
	current, ok := parsePidFile([]byte(`{"llama":{"pid":42,"name":"llama-server"}}`))
	if !ok || current["llama"].PID != 42 || current["llama"].Name != "llama-server" {
		t.Errorf("current format parsed as %+v (ok=%v)", current, ok)
	}
	legacy, ok := parsePidFile([]byte(`{"llama":7,"cognee":9}`))
	if !ok || legacy["llama"].PID != 7 || legacy["cognee"].PID != 9 {
		t.Errorf("legacy format parsed as %+v (ok=%v)", legacy, ok)
	}
	if legacy["llama"].Name != "" {
		t.Error("legacy entries must carry no name")
	}
	if _, ok := parsePidFile([]byte(`{{{`)); ok {
		t.Error("malformed JSON must not parse")
	}
}

// TestUnit_ProcessNameResolves sanity-checks the identity primitive.
func TestUnit_ProcessNameResolves(t *testing.T) {
	if name := processName(os.Getpid()); name == "" {
		t.Skip("ps unavailable in this environment")
	}
	if name := processName(999999999); name != "" {
		t.Errorf("processName of a nonexistent PID = %q, want empty", name)
	}
}

// TestUnit_CleanupOrphansActuallyKills is the regression guard for a dead
// liveness probe. cleanupOrphans used proc.Signal(os.Signal(nil)), which
// os.Process.Signal rejects with "os: unsupported signal type" for every
// process, alive or not — so the kill branch was unreachable and orphan
// recovery never reaped anything, despite the docs promising it survives
// kill -9. This spawns a real detached process and requires it to die.
func TestUnit_CleanupOrphansActuallyKills(t *testing.T) {
	cmd := exec.Command("sleep", "120")
	if err := cmd.Start(); err != nil {
		t.Fatalf("spawn victim: %v", err)
	}
	pid := cmd.Process.Pid

	// Reap concurrently. This victim is our own child, so once it exits it
	// stays a zombie until we Wait() — and a zombie still answers signal 0,
	// which would make the liveness poll below never observe the death. A real
	// orphan is reparented to init, which reaps it immediately.
	reaped := make(chan struct{})
	go func() { _ = cmd.Wait(); close(reaped) }()

	dir := t.TempDir()
	logs := filepath.Join(dir, "logs")
	if err := os.MkdirAll(logs, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	body := fmt.Sprintf(`{"llama":{"pid":%d,"name":%q}}`, pid, processName(pid))
	if err := os.WriteFile(filepath.Join(logs, ".mcp-pids.json"), []byte(body), 0o644); err != nil {
		t.Fatalf("write pid file: %v", err)
	}

	// Probe directly rather than via processAlive: this guard must not depend
	// on the function under test.
	if cmd.Process.Signal(syscall.Signal(0)) != nil {
		t.Fatal("victim died before the test started")
	}

	withWorkingDir(t, dir, func() {
		cleanupOrphans()
	})

	select {
	case <-reaped:
		return // cleanupOrphans killed it
	case <-time.After(15 * time.Second):
		_ = cmd.Process.Kill() // do not leak the victim
		<-reaped
		t.Fatalf("orphan PID %d survived cleanupOrphans — orphan recovery is a no-op", pid)
	}
}

// TestUnit_ProcessAliveProbe pins the probe itself. os.Signal(nil) reports every
// process as dead; syscall.Signal(0) is the correct zero-signal probe.
func TestUnit_ProcessAliveProbe(t *testing.T) {
	self, err := os.FindProcess(os.Getpid())
	if err != nil {
		t.Fatalf("find self: %v", err)
	}
	if !processAlive(self) {
		t.Fatal("the running test process must report alive")
	}
	if self.Signal(os.Signal(nil)) == nil {
		t.Fatal("os.Signal(nil) unexpectedly succeeded — the probe rationale changed")
	}
}

func TestUnit_WorkingDirIsAbsolute(t *testing.T) {
	svc := newServices(Config{}, testLogger(), nil)
	if wd := svc.workingDir(); !filepath.IsAbs(wd) {
		t.Errorf("workingDir() = %q, want an absolute path", wd)
	}
}

// ─── Cloud embedding detection ───────────────────────────────────────────────

func TestUnit_IsCloudURL(t *testing.T) {
	cloud := []string{
		"http://api.example.com/v1",
		"https://api.example.com/v1",
		"https://x",
	}
	local := []string{
		"", "./model/qwen3.gguf", "/abs/path/model.gguf", "model.gguf",
		"ftp://example.com/m.gguf",
		"HTTPS://api.example.com", // scheme match is case-sensitive by design
		" https://leading-space",
	}
	for _, s := range cloud {
		if !isCloudURL(s) {
			t.Errorf("isCloudURL(%q) = false, want true", s)
		}
	}
	for _, s := range local {
		if isCloudURL(s) {
			t.Errorf("isCloudURL(%q) = true, want false", s)
		}
	}
}

func TestUnit_IsCloudEmbedding(t *testing.T) {
	if !(Config{ModelPath: "https://embed.example.com/v1"}).IsCloudEmbedding() {
		t.Error("an HTTPS ModelPath must select the cloud embedding path")
	}
	if (Config{ModelPath: "./model/qwen3-embedding-0.6b-Q8_0.gguf"}).IsCloudEmbedding() {
		t.Error("a filesystem ModelPath must not select the cloud path")
	}
	if (Config{}).IsCloudEmbedding() {
		t.Error("an empty ModelPath must not select the cloud path")
	}
}

// ─── Subprocess environment builders ─────────────────────────────────────────

func envValue(env []string, key string) (string, bool) {
	prefix := key + "="
	// Later entries win in exec, so scan backwards.
	for i := len(env) - 1; i >= 0; i-- {
		if strings.HasPrefix(env[i], prefix) {
			return strings.TrimPrefix(env[i], prefix), true
		}
	}
	return "", false
}

// TestUnit_CogneeRustEnvWiring pins the env contract for the Rust backend.
// docs/cognee-deepseek-compatibility.md records that these exact values were
// the difference between a working pipeline and silent extraction failures, so
// a regression here is expensive to rediscover.
func TestUnit_CogneeRustEnvWiring(t *testing.T) {
	cfg := Config{
		CogneeLLMApiKey:         "sk-test-key",
		CogneeLLMModel:          "deepseek/deepseek-v4-flash",
		CogneeLLMEndpoint:       "https://openrouter.ai/api/v1",
		CogneeEmbeddingEndpoint: "http://localhost:8080/v1",
		CogneeDataDir:           "/tmp/cognee-rust-data",
		CogneePort:              "8003",
	}
	env := newServices(cfg, testLogger(), nil).cogneeRustEnv()

	want := map[string]string{
		"LLM_API_KEY":           "sk-test-key",
		"LLM_MODEL":             "deepseek/deepseek-v4-flash",
		"LLM_ENDPOINT":          "https://openrouter.ai/api/v1",
		"EMBEDDING_ENDPOINT":    "http://localhost:8080/v1",
		"EMBEDDING_DIMENSIONS":  "1024", // must match qwen3-embedding-0.6b
		"HTTP_API_PORT":         "8003",
		"EMBEDDING_PROVIDER":    "openai_compatible", // Rust has no llama_cpp provider
		"DATA_ROOT_DIRECTORY":   "/tmp/cognee-rust-data/data",
		"SYSTEM_ROOT_DIRECTORY": "/tmp/cognee-rust-data/system",
	}
	for k, v := range want {
		got, ok := envValue(env, k)
		if !ok {
			t.Errorf("%s is not set", k)
			continue
		}
		if got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}
}

// TestUnit_CogneePythonEnvUsesJSONInstructorMode guards the DeepSeek fix: the
// Python backend must not fall back to tool-calling, which DeepSeek rejects
// outright ("Thinking mode does not support this tool_choice").
func TestUnit_CogneePythonEnvUsesJSONInstructorMode(t *testing.T) {
	env := newServices(Config{CogneePort: "8000"}, testLogger(), nil).cogneePythonEnv()

	if got, _ := envValue(env, "LLM_INSTRUCTOR_MODE"); got != "json_mode" {
		t.Errorf("LLM_INSTRUCTOR_MODE = %q, want json_mode — tool_choice breaks DeepSeek", got)
	}
	if got, _ := envValue(env, "EMBEDDING_PROVIDER"); got != "llama_cpp" {
		t.Errorf("EMBEDDING_PROVIDER = %q, want llama_cpp", got)
	}
}

// TestUnit_CogneeEnvVariantsDiffer catches the two backends being wired to the
// same embedding provider, which silently breaks one of them.
func TestUnit_CogneeEnvVariantsDiffer(t *testing.T) {
	svc := newServices(Config{CogneePort: "8000"}, testLogger(), nil)
	py, _ := envValue(svc.cogneePythonEnv(), "EMBEDDING_PROVIDER")
	rs, _ := envValue(svc.cogneeRustEnv(), "EMBEDDING_PROVIDER")
	if py == rs {
		t.Errorf("both backends use EMBEDDING_PROVIDER=%q; Rust needs openai_compatible "+
			"and Python needs llama_cpp", py)
	}
}

func TestUnit_GetEnvOrDefault(t *testing.T) {
	const key = "MCP_TEST_ENV_ONLY"
	t.Setenv(key, "")
	if got := getEnvOrDefault(key, "fallback"); got != "fallback" {
		t.Errorf("empty env should fall back, got %q", got)
	}
	t.Setenv(key, "explicit")
	if got := getEnvOrDefault(key, "fallback"); got != "explicit" {
		t.Errorf("set env should win, got %q", got)
	}
}

func TestUnit_HealthURL(t *testing.T) {
	if got := healthURL("8080"); got != "http://localhost:8080/health" {
		t.Errorf("healthURL(8080) = %q", got)
	}
}

// ─── services.check ──────────────────────────────────────────────────────────

func TestUnit_ServicesCheck(t *testing.T) {
	t.Run("healthy", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(200)
		}))
		defer srv.Close()
		svc := newServices(Config{RequestTimeout: 5 * time.Second, HealthTimeout: time.Second},
			testLogger(), nil)
		if err := svc.check(srv.URL); err != nil {
			t.Errorf("healthy service reported: %v", err)
		}
	})

	t.Run("non-200", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(503)
		}))
		defer srv.Close()
		svc := newServices(Config{RequestTimeout: 5 * time.Second, HealthTimeout: time.Second},
			testLogger(), nil)
		if err := svc.check(srv.URL); err == nil {
			t.Error("a 503 must be reported as unhealthy")
		}
	})

	t.Run("unreachable", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
		srv.Close()
		svc := newServices(Config{RequestTimeout: 2 * time.Second, HealthTimeout: time.Second},
			testLogger(), nil)
		if err := svc.check(srv.URL); err == nil {
			t.Error("an unreachable service must be reported as unhealthy")
		}
	})

	// A hung dependency must not stall the health loop: check() caps its
	// timeout at 5s regardless of the configured HealthTimeout.
	t.Run("hanging service is capped", func(t *testing.T) {
		block := make(chan struct{})
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			<-block
		}))
		defer func() { close(block); srv.Close() }()

		svc := newServices(Config{RequestTimeout: time.Minute, HealthTimeout: time.Hour},
			testLogger(), nil)
		start := time.Now()
		if err := svc.check(srv.URL); err == nil {
			t.Error("a hanging service must eventually fail the check")
		}
		if elapsed := time.Since(start); elapsed > 10*time.Second {
			t.Errorf("check took %v — the 5s cap is not applied", elapsed)
		}
	})
}

// TestUnit_AllHealthyNilReceiver documents the guard that keeps /health from
// panicking before services are wired.
func TestUnit_AllHealthyNilReceiver(t *testing.T) {
	var svc *services
	llama, cognee := svc.allHealthy()
	if llama || cognee {
		t.Error("a nil services must report everything down, not healthy")
	}
}

// ─── misc ────────────────────────────────────────────────────────────────────

// TestUnit_NewJobIDIsUniqueUnderConcurrency guards against collisions, which
// would silently overwrite another agent's job row.
func TestUnit_NewJobIDIsUniqueUnderConcurrency(t *testing.T) {
	const n = 2000
	ids := make([]string, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ids[i] = newJobID()
		}(i)
	}
	wg.Wait()

	seen := make(map[string]bool, n)
	for _, id := range ids {
		if id == "" {
			t.Fatal("newJobID returned an empty string")
		}
		if seen[id] {
			t.Fatalf("duplicate job id %q across %d concurrent calls", id, n)
		}
		seen[id] = true
	}
}

func TestUnit_NewSessionIDIsUniqueUnderConcurrency(t *testing.T) {
	const n = 2000
	ids := make([]string, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ids[i] = newSessionID()
		}(i)
	}
	wg.Wait()

	seen := make(map[string]bool, n)
	for _, id := range ids {
		if len(id) < 8 {
			// handleToolCall slices sid[:8] for the trace id.
			t.Fatalf("session id %q is shorter than the 8 chars callers slice", id)
		}
		if seen[id] {
			t.Fatalf("duplicate session id %q", id)
		}
		seen[id] = true
	}
}

// TestUnit_SessionCloseUnderConcurrency hammers the CompareAndSwap guard.
func TestUnit_SessionCloseUnderConcurrency(t *testing.T) {
	sess := &MCPSession{SSEChannel: make(chan string, 1)}
	var wg sync.WaitGroup
	var closed atomic.Int64
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sess.Close()
			closed.Add(1)
		}()
	}
	wg.Wait()
	if !sess.IsClosed() {
		t.Error("session should be closed")
	}
}
