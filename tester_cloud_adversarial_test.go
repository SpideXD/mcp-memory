package main

import (
	"bytes"
	"context"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// =========================================================================
// PASS 1 — ADVERSARIAL CLOUD MODE TESTS
//
// These tests attack the cloud mode implementation with edge cases the
// coder may not have considered. They verify NOT just that the spec's ACs
// pass, but that the code is robust against malicious or unexpected input.
// =========================================================================

// ─── isCloudURL Edge Cases ───────────────────────────────────────────────

func TestCloud_isCloudURL_edgeCases(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  bool
	}{
		// Spec-listed cases (AC-1 to AC-6)
		{"empty", "", false},
		{"http URL", "http://api.openai.com/v1", true},
		{"https URL", "https://api.cohere.com/v1/rerank", true},
		{"relative path", "./model/qwen3-embedding-0.6b-Q8_0.gguf", false},
		{"absolute path", "/opt/models/test.gguf", false},
		{"ftp URL", "ftp://example.com/model.gguf", false},

		// Edge cases NOT in spec
		{"bare http:// no host", "http://", true},
		{"bare https:// no host", "https://", true},
		{"just http no colon", "http", false},
		{"just https no colon", "https", false},
		{"HTTP uppercase", "HTTP://api.openai.com/v1", false},
		{"HTTPS uppercase", "HTTPS://api.cohere.com/v1", false},
		{"leading space", " http://api.openai.com/v1", false},
		{"trailing space", "http://api.openai.com/v1 ", true},
		{"null byte prefix", "\x00http://api.openai.com/v1", false},
		{"null byte in middle", "http://\x00api.openai.com/v1", true},
		{"unicode host", "http://\u00e9xample.com", true},
		{"10KB non-http tail", strings.Repeat("x", 10*1024-10) + "http://a", false},
		{"very long valid http", "http://" + strings.Repeat("a", 10000), true},
		{"very long valid https", "https://" + strings.Repeat("a", 10000), true},
		{"newline prefix", "\nhttp://api.openai.com/v1", false},
		{"tab prefix", "\thttp://api.openai.com/v1", false},
		{"mixed case Http", "Http://api.openai.com/v1", false},
		{"ws URL", "ws://localhost:8080", false},
		{"wss URL", "wss://localhost:8080", false},
		{"file URL", "file:///path/to/model.gguf", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isCloudURL(tt.input)
			if got != tt.want {
				t.Errorf("isCloudURL(%q) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}

// ─── IsCloudEmbedding / IsCloudReranker Edge Cases ───────────────────────

func TestCloud_IsCloudEmbedding_derivation(t *testing.T) {
	tests := []struct {
		name      string
		modelPath string
		want      bool
	}{
		{"https URL", "https://api.openai.com/v1", true},
		{"http URL", "http://localhost:8080/v1", true},
		{"relative path", "./model/test.gguf", false},
		{"absolute path", "/opt/models/test.gguf", false},
		{"empty string", "", false},
		{"just http://", "http://", true},
		{"just https://", "https://", true},
		{"whitespace only", "   ", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := Config{ModelPath: tt.modelPath}
			got := c.IsCloudEmbedding()
			if got != tt.want {
				t.Errorf("IsCloudEmbedding() with ModelPath=%q = %v, want %v", tt.modelPath, got, tt.want)
			}
		})
	}
}

func TestCloud_IsCloudReranker_derivation(t *testing.T) {
	tests := []struct {
		name          string
		rerankerModel string
		want          bool
	}{
		{"https URL", "https://api.cohere.com/v1/rerank", true},
		{"http URL", "http://localhost:8081/v1/rerank", true},
		{"relative path", "./model/test.gguf", false},
		{"absolute path", "/opt/models/test.gguf", false},
		{"empty string", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := Config{RerankerModel: tt.rerankerModel}
			got := c.IsCloudReranker()
			if got != tt.want {
				t.Errorf("IsCloudReranker() with RerankerModel=%q = %v, want %v", tt.rerankerModel, got, tt.want)
			}
		})
	}
}

// ─── Validate() Cloud Checks ─────────────────────────────────────────────

func TestCloud_Validate_whitespaceOnlyCloudVars(t *testing.T) {
	// AC-14: whitespace-only values must be rejected
	c := Config{
		ModelPath:            "https://api.openai.com/v1",
		CloudEmbeddingAPIKey: "real-key",
		CloudEmbeddingURL:    "  ", // whitespace only
		CloudEmbeddingModel:  "text-embedding-3-small",
		LLMAPIKey:            "sk-test",
		MaxSessions:          1,
		MaxContentBytes:      1,
		RetainWorkers:        1,
		ReflectWorkers:       1,
		StartTimeout:         time.Second,
		StopTimeout:          time.Second,
		ShutdownTimeout:      time.Second,
	}
	err := c.Validate()
	if err == nil {
		t.Error("Validate() with whitespace-only CloudEmbeddingURL = nil, want error")
	} else if !strings.Contains(err.Error(), "CLOUD_EMBEDDING_URL is required") {
		t.Errorf("Validate() error = %q, want 'CLOUD_EMBEDDING_URL is required'", err)
	}
}

func TestCloud_Validate_allCloudVarsWhitespace(t *testing.T) {
	c := Config{
		ModelPath:            "https://api.openai.com/v1",
		CloudEmbeddingAPIKey: "   ",
		CloudEmbeddingURL:    "   ",
		CloudEmbeddingModel:  "   ",
		LLMAPIKey:            "sk-test",
		MaxSessions:          1,
		MaxContentBytes:      1,
		RetainWorkers:        1,
		ReflectWorkers:       1,
		StartTimeout:         time.Second,
		StopTimeout:          time.Second,
		ShutdownTimeout:      time.Second,
	}
	err := c.Validate()
	if err == nil {
		t.Fatal("Validate() with whitespace-only cloud vars = nil, want error")
	}
	if !strings.Contains(err.Error(), "CLOUD_EMBEDDING_API_KEY") {
		t.Errorf("Validate() error = %q, want 'CLOUD_EMBEDDING_API_KEY'", err)
	}
}

func TestCloud_Validate_mixedCloudLocal(t *testing.T) {
	c := Config{
		ModelPath:            "https://api.openai.com/v1",
		CloudEmbeddingAPIKey: "sk-test",
		CloudEmbeddingURL:    "https://api.openai.com/v1",
		CloudEmbeddingModel:  "text-embedding-3-small",
		RerankerModel:        "./model/bge-reranker-base-Q4_k_m.gguf",
		LLMAPIKey:            "sk-test",
		MaxSessions:          1,
		MaxContentBytes:      1,
		RetainWorkers:        1,
		ReflectWorkers:       1,
		StartTimeout:         time.Second,
		StopTimeout:          time.Second,
		ShutdownTimeout:      time.Second,
	}
	err := c.Validate()
	if err != nil {
		t.Errorf("Validate() with mixed cloud/local = %v, want nil", err)
	}
}

func TestCloud_Validate_bothCloudAllVarsSet(t *testing.T) {
	c := Config{
		ModelPath:            "https://api.openai.com/v1",
		CloudEmbeddingAPIKey: "sk-test",
		CloudEmbeddingURL:    "https://api.openai.com/v1",
		CloudEmbeddingModel:  "text-embedding-3-small",
		RerankerModel:        "https://api.cohere.com/v1/rerank",
		CloudRerankerAPIKey:  "cohere-key",
		CloudRerankerURL:     "https://api.cohere.com/v1/rerank",
		CloudRerankerModel:   "rerank-english-v3.0",
		LLMAPIKey:            "sk-test",
		MaxSessions:          1,
		MaxContentBytes:      1,
		RetainWorkers:        1,
		ReflectWorkers:       1,
		StartTimeout:         time.Second,
		StopTimeout:          time.Second,
		ShutdownTimeout:      time.Second,
	}
	err := c.Validate()
	if err != nil {
		t.Errorf("Validate() with both cloud = %v, want nil", err)
	}
}

func TestCloud_Validate_missingRerankerURL_whitespace(t *testing.T) {
	c := Config{
		RerankerModel:       "https://api.cohere.com/v1/rerank",
		CloudRerankerAPIKey: "real-key",
		CloudRerankerURL:    "\t\n  ",
		CloudRerankerModel:  "rerank-english-v3.0",
		LLMAPIKey:           "sk-test",
		MaxSessions:         1,
		MaxContentBytes:     1,
		RetainWorkers:       1,
		ReflectWorkers:      1,
		StartTimeout:        time.Second,
		StopTimeout:         time.Second,
		ShutdownTimeout:     time.Second,
	}
	err := c.Validate()
	if err == nil {
		t.Fatal("Validate() with whitespace-only CloudRerankerURL = nil, want error")
	}
	if !strings.Contains(err.Error(), "CLOUD_RERANKER_URL is required") {
		t.Errorf("Validate() error = %q, want 'CLOUD_RERANKER_URL is required'", err)
	}
}

func TestCloud_Validate_localPathsNoAPINoTTL(t *testing.T) {
	// Verify local mode skips cloud validation and only checks file existence
	c := Config{
		ModelPath:      "./model/qwen3-embedding-0.6b-Q8_0.gguf",
		RerankerModel:  "./model/bge-reranker-base-Q4_k_m.gguf",
		LLMAPIKey:      "sk-test",
		MaxSessions:    1,
		MaxContentBytes: 1,
		RetainWorkers:  1,
		ReflectWorkers: 1,
		StartTimeout:   time.Second,
		StopTimeout:    time.Second,
		ShutdownTimeout: time.Second,
	}
	err := c.Validate()
	if err != nil {
		t.Errorf("Validate() with local paths = %v, want nil", err)
	}
}

// ─── allHealthy() WaitGroup Correctness ──────────────────────────────────

func TestCloud_allHealthy_WaitGroupCounts(t *testing.T) {
	tests := []struct {
		name         string
		cloudEmbed   bool
		cloudRerank  bool
		wantLlama    bool
		wantReranker bool
	}{
		{"both_local", false, false, false, false},
		{"both_cloud", true, true, true, true},
		{"embed_cloud_rerank_local", true, false, true, false},
		{"embed_local_rerank_cloud", false, true, false, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := newTestConfig()
			if tt.cloudEmbed {
				cfg.ModelPath = "https://api.openai.com/v1"
			} else {
				cfg.ModelPath = "./model/qwen3-embedding-0.6b-Q8_0.gguf"
			}
			if tt.cloudRerank {
				cfg.RerankerModel = "https://api.cohere.com/v1/rerank"
			} else {
				cfg.RerankerModel = "./model/bge-reranker-base-Q4_k_m.gguf"
			}
			svc, _ := newTestCloudServices(cfg)

			l, r, h := svc.allHealthy()
			if l != tt.wantLlama {
				t.Errorf("allHealthy() llama = %v, want %v", l, tt.wantLlama)
			}
			if r != tt.wantReranker {
				t.Errorf("allHealthy() reranker = %v, want %v", r, tt.wantReranker)
			}
			_ = h
		})
	}
}

func TestCloud_allHealthy_concurrentCalls(t *testing.T) {
	// Run with -race to detect data races
	cfg := newTestConfig()
	cfg.ModelPath = "https://api.openai.com/v1"
	cfg.RerankerModel = "https://api.cohere.com/v1/rerank"
	svc, _ := newTestCloudServices(cfg)

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			l, r, h := svc.allHealthy()
			if !l || !r {
				t.Errorf("both cloud: ll=%v rr=%v want true,true", l, r)
			}
			_ = h
		}()
	}
	wg.Wait()
}

func TestCloud_allHealthy_mixedConcurrent(t *testing.T) {
	// Embedding cloud (nChecks=2: reranker + hindsight)
	cfg := newTestConfig()
	cfg.ModelPath = "https://api.openai.com/v1"
	cfg.RerankerModel = "./model/bge-reranker-base-Q4_k_m.gguf"
	svc, _ := newTestCloudServices(cfg)

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			l, _, _ := svc.allHealthy()
			if !l {
				t.Errorf("goroutine %d: cloud embed but llama=false", id)
			}
		}(i)
	}
	wg.Wait()
}

// ─── Goroutine Recovery Violations (CRITICAL) ────────────────────────────
// The spec explicitly requires: "The anonymous goroutines inside allHealthy()
// must each include defer/recover guarding against panics from svc.check()."
// This is violated in the current code (lines 267, 270, 272).

func TestCloud_allHealthy_missingDeferRecover(t *testing.T) {
	t.Errorf("CRITICAL: allHealthy() goroutines (services.go lines 267,270,272) have defer wg.Done() but NO defer recover() — process crash risk")
}

func TestCloud_checkAndRestart_missingDeferRecover(t *testing.T) {
	t.Errorf("CRITICAL: checkAndRestart() called as goroutine from monitor() has NO defer recover() — 3 goroutines per tick, any panic crashes process")
}

func TestCloud_stopProcess_missingDeferRecover(t *testing.T) {
	t.Errorf("CRITICAL: stopProcess() goroutine (services.go ~line 480) has NO defer recover() — cmd.Wait() panic crashes process")
}

// ─── stop() Cloud Guards ─────────────────────────────────────────────────

func TestCloud_stop_cloudModeNoPanic(t *testing.T) {
	cfg := newTestConfig()
	cfg.ModelPath = "https://api.openai.com/v1"
	cfg.RerankerModel = "https://api.cohere.com/v1/rerank"
	svc, _ := newTestCloudServices(cfg)

	done := make(chan bool, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				t.Errorf("stop() panicked: %v", r)
			}
			done <- true
		}()
		svc.stop()
		done <- true
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("stop() timed out")
	}
}

func TestCloud_stop_localModeNoPanic(t *testing.T) {
	// Both local (no cmds started) — stop() must still work
	cfg := newTestConfig()
	svc, _ := newTestCloudServices(cfg)
	done := make(chan bool, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				t.Errorf("stop() panicked: %v", r)
			}
			done <- true
		}()
		svc.stop()
		done <- true
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("stop() timed out")
	}
}

// ─── start() Cloud Guards ────────────────────────────────────────────────

func TestCloud_start_skipsLlamaWhenCloudEmbed(t *testing.T) {
	cfg := newTestConfig()
	cfg.ModelPath = "https://api.openai.com/v1"
	cfg.RerankerModel = "./model/bge-reranker-base-Q4_k_m.gguf"
	cfg.LlamaPath = "" // avoid spawn failure side effects
	svc, buf := newTestCloudServices(cfg)
	_ = svc.start()

	out := buf.String()
	if !strings.Contains(out, "llama.cpp skipped (cloud embedding mode)") {
		t.Errorf("start() didn't log cloud skip:\n%s", out)
	}
}

func TestCloud_start_skipsRerankerWhenCloudRerank(t *testing.T) {
	// Both cloud — start() will skip both services and log both skip messages
	cfg := newTestConfig()
	cfg.ModelPath = "https://api.openai.com/v1"
	cfg.RerankerModel = "https://api.cohere.com/v1/rerank"
	svc, buf := newTestCloudServices(cfg)
	_ = svc.start()

	out := buf.String()
	if !strings.Contains(out, "llama reranker skipped (cloud reranker mode)") {
		t.Errorf("start() didn't log reranker cloud skip:\n%s", out)
	}
}

// ─── Config Defaults ─────────────────────────────────────────────────────

func TestCloud_LoadConfig_defaults(t *testing.T) {
	cfg := LoadConfig()
	if cfg.ModelPath != "./model/qwen3-embedding-0.6b-Q8_0.gguf" {
		t.Errorf("default ModelPath = %q", cfg.ModelPath)
	}
	if cfg.RerankerModel != "./model/bge-reranker-base-Q4_k_m.gguf" {
		t.Errorf("default RerankerModel = %q", cfg.RerankerModel)
	}
	if cfg.CloudEmbeddingAPIKey != "" {
		t.Errorf("default CloudEmbeddingAPIKey = %q, want ''", cfg.CloudEmbeddingAPIKey)
	}
	if cfg.CloudRerankerAPIKey != "" {
		t.Errorf("default CloudRerankerAPIKey = %q, want ''", cfg.CloudRerankerAPIKey)
	}
}

// ─── filepath.Base for Trap 3 Fix ────────────────────────────────────────

func TestCloud_trap3_filepathBase_fix(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"./model/bge-reranker-base-Q4_k_m.gguf", "bge-reranker-base-Q4_k_m.gguf"},
		{"../../model/bge-reranker-base-Q4_k_m.gguf", "bge-reranker-base-Q4_k_m.gguf"},
		{"/absolute/path/model.gguf", "model.gguf"},
		{"just-a-filename.gguf", "just-a-filename.gguf"},
		{"", "."},
		{"/", "/"},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := filepath.Base(tt.input)
			if got != tt.expected {
				t.Errorf("filepath.Base(%q) = %q, want %q", tt.input, got, tt.expected)
			}
		})
	}
}

// ─── Goroutine Leak Check ────────────────────────────────────────────────

func TestCloud_goroutineLeak_newServices(t *testing.T) {
	before := runtime.NumGoroutine()
	for i := 0; i < 10; i++ {
		cfg := newTestConfig()
		cfg.ModelPath = "https://api.openai.com/v1"
		cfg.RerankerModel = "https://api.cohere.com/v1/rerank"
		svc, _ := newTestCloudServices(cfg)
		_ = svc
	}
	after := runtime.NumGoroutine()
	if delta := after - before; delta > 5 {
		t.Errorf("possible goroutine leak: %d -> %d (delta=%d)", before, after, delta)
	}
}

// ─── waitAllHealthy With Cloud Mode ──────────────────────────────────────

func TestCloud_waitAllHealthy_cloudModeTimeout(t *testing.T) {
	// Both cloud: waitAllHealthy should only wait for Hindsight (1 HTTP check)
	cfg := newTestConfig()
	cfg.ModelPath = "https://api.openai.com/v1"
	cfg.RerankerModel = "https://api.cohere.com/v1/rerank"
	svc, _ := newTestCloudServices(cfg)

	err := svc.waitAllHealthy(200 * time.Millisecond)
	if err == nil {
		t.Log("waitAllHealthy returned nil (no hindsight running? unexpected)")
	} else {
		msg := err.Error()
		if !strings.Contains(msg, "llama=true") || !strings.Contains(msg, "reranker=true") {
			t.Errorf("waitAllHealthy error = %q, want 'llama=true reranker=true ...'", msg)
		}
	}
}

// ─── monitor() Cloud Mode — No Goroutines for Cloud Services ────────────

func TestCloud_monitor_noCloudGoroutines(t *testing.T) {
	cfg := newTestConfig()
	cfg.ModelPath = "https://api.openai.com/v1"
	cfg.RerankerModel = "https://api.cohere.com/v1/rerank"
	cfg.HealthCheckInterval = 50 * time.Millisecond
	svc, _ := newTestCloudServices(cfg)

	ctx, cancel := context.WithCancel(context.Background())
	var panics atomic.Int64

	before := runtime.NumGoroutine()
	go svc.monitor(ctx, &panics)

	// Let monitor run for a few ticks
	time.Sleep(160 * time.Millisecond)
	cancel()
	time.Sleep(50 * time.Millisecond) // settle

	delta := runtime.NumGoroutine() - before

	t.Logf("monitor ran in both-cloud mode, goroutine delta=%d", delta)
	if panics.Load() > 0 {
		t.Errorf("monitor panicked %d times", panics.Load())
	}
	// In cloud mode, only 1 checkAndRestart goroutine per tick is spawned (hindsight)
	// So delta should be near 0 after cancel (goroutines should complete quickly)
	if delta > 10 {
		t.Errorf("possible goroutine leak in monitor with cloud mode: delta=%d", delta)
	}
}

// ─── Helper ──────────────────────────────────────────────────────────────

func newTestCloudServices(cfg Config) (*services, *bytes.Buffer) {
	l, buf := newTestLogger()
	alerts := NewAlertClient("", "optional")
	return newServices(cfg, l, alerts), buf
}
