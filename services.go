package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"golang.org/x/sync/singleflight"

	"mcp-memory/logger"
)

var errProcessPanic = fmt.Errorf("process goroutine panicked")

type services struct {
	config     Config
	llamaCmd   *exec.Cmd
	cogneeCmd  *exec.Cmd
	httpClient *http.Client
	mu         sync.Mutex
	log        *logger.Logger
	alerts     *AlertClient

	// Cached health status to avoid HTTP requests per tool call
	healthMu      sync.RWMutex
	healthCache   [2]bool // llama, cognee
	healthChecked time.Time
	healthGroup   singleflight.Group // deduplicate concurrent health refreshes

	// Per-service fail/restart tracking for backoff
	llamaFails  serviceFails
	cogneeFails serviceFails
}

type serviceFails struct {
	mu          sync.Mutex
	consecutive int
	restarts    int
	lastRestart time.Time
}

func newServices(config Config, log *logger.Logger, alerts *AlertClient) *services {
	return &services{
		config:     config,
		httpClient: &http.Client{Timeout: config.RequestTimeout},
		log:        log,
		alerts:     alerts,
	}
}

func (svc *services) start() error {
	llamaURL := healthURL(svc.config.LlamaPort)

	// llama-server runs for ALL backends — Cognee uses it as external embeddings provider
	if svc.config.IsCloudEmbedding() {
		svc.log.Info("llama.cpp skipped (cloud embedding mode)")
	} else if svc.check(llamaURL) != nil {
		if err := svc.startLlama(); err != nil {
			return err
		}
		if err := svc.wait(context.Background(), llamaURL, svc.config.StartTimeout); err != nil {
			return err
		}
		svc.log.Info("llama.cpp started")
	} else {
		svc.log.Info("llama.cpp already running")
	}

	switch svc.config.Backend {
	case BackendCogneeRust:
		cogneeURL := healthURL(svc.config.CogneePort)
		if svc.check(cogneeURL) != nil {
			if err := svc.startCogneeRust(); err != nil {
				return err
			}
			if err := svc.wait(context.Background(), cogneeURL, svc.config.StartTimeout); err != nil {
				return err
			}
			svc.log.Info("cognee-rust started")
		} else {
			svc.log.Info("cognee-rust already running")
		}
	}
	return nil
}

func (svc *services) stop() {
	switch svc.config.Backend {
	case BackendCogneeRust:
		svc.stopProcess(&svc.cogneeCmd, "cognee")
	}
	if !svc.config.IsCloudEmbedding() {
		svc.stopProcess(&svc.llamaCmd, "llama.cpp")
	}
}

func (svc *services) monitor(ctx context.Context, panics *atomic.Int64) {
	defer func() {
		if r := recover(); r != nil {
			panics.Add(1)
			svc.log.Error("monitor panic", "panic", fmt.Sprintf("%v", r))
			svc.alerts.Send(AlertCritical, fmt.Sprintf("Health monitor panicked: %v", r), nil)
		}
	}()
	ticker := time.NewTicker(svc.config.HealthCheckInterval)
	defer ticker.Stop()

	const maxRestartsPerHour = 5

	for {
		select {
		case <-ticker.C:
			// llama-server monitored for ALL backends
			if !svc.config.IsCloudEmbedding() {
				go svc.checkAndRestart(ctx, "llama.cpp", healthURL(svc.config.LlamaPort),
					&svc.llamaCmd, svc.startLlama, &svc.llamaFails, maxRestartsPerHour)
			}

			switch svc.config.Backend {
			case BackendCogneeRust:
				go svc.checkAndRestart(ctx, "cognee-rust", healthURL(svc.config.CogneePort),
					&svc.cogneeCmd, svc.startCogneeRust, &svc.cogneeFails, maxRestartsPerHour)
			}

		case <-ctx.Done():
			return
		}
	}
}

// checkAndRestart checks a service and restarts it with backoff if unhealthy.
// Detects process exit (not just port failure) and applies exponential backoff.
func (svc *services) checkAndRestart(
	ctx context.Context,
	name, url string,
	cmdPtr **exec.Cmd,
	startFn func() error,
	fails *serviceFails,
	maxRestarts int,
) {
	defer func() {
		if r := recover(); r != nil {
			svc.log.Error("checkAndRestart panic", "name", name, "panic", fmt.Sprintf("%v", r))
		}
	}()
	// Check if process exited (stronger signal than HTTP health)
	svc.mu.Lock()
	cmd := *cmdPtr
	svc.mu.Unlock()
	processExited := cmd != nil && cmd.ProcessState != nil && cmd.ProcessState.Exited()

	healthErr := svc.check(url)

	if healthErr == nil && !processExited {
		fails.mu.Lock()
		wasDown := fails.consecutive > 0
		fails.consecutive = 0
		fails.mu.Unlock()
		if wasDown {
			svc.log.Info("service recovered", "name", name)
		}
		return
	}

	fails.mu.Lock()
	fails.consecutive++
	consec := fails.consecutive
	restarts := fails.restarts
	lastRestart := fails.lastRestart
	fails.mu.Unlock()

	if processExited {
		svc.log.Warn("process exited", "name", name, "health", healthErr)
		svc.alerts.Send(AlertError, fmt.Sprintf("%s: process exited unexpectedly", name), nil)
	} else {
		svc.log.Warn("health check failed", "name", name, "consecutive", consec, "error", healthErr)
	}

	if consec < svc.config.ConsecutiveFailures {
		return
	}

	// Limit restarts — if > max in last hour, stop trying
	if restarts >= maxRestarts && time.Since(lastRestart) < time.Hour {
		svc.log.Error("max restarts exceeded", "name", name, "restarts", restarts)
		svc.alerts.Send(AlertCritical, fmt.Sprintf("%s: max restarts exceeded (%d in 1hr)", name, restarts), map[string]interface{}{"restarts": restarts})
		return
	}

	// Exponential backoff: 1s, 2s, 4s, 8s, 16s, 32s (max 30s)
	backoff := time.Duration(1<<uint(min(restarts, 5))) * time.Second
	if backoff < 1*time.Second {
		backoff = 1 * time.Second
	}
	if restarts > 0 {
		svc.log.Info("backing off before restart", "name", name, "backoff", backoff)
		select {
		case <-time.After(backoff):
		case <-ctx.Done():
			return
		}
	}

	svc.log.Warn("restarting service", "name", name, "restart", restarts+1)
	svc.stopProcess(cmdPtr, name)

	if err := startFn(); err != nil {
		svc.log.Error("restart failed", "name", name, "error", err)
		svc.alerts.Send(AlertError, fmt.Sprintf("%s: restart failed: %v", name, err), nil)
		fails.mu.Lock()
		fails.restarts++
		fails.lastRestart = time.Now()
		fails.mu.Unlock()
		return
	}

	if err := svc.wait(ctx, url, svc.config.StartTimeout); err != nil {
		svc.log.Error("service not ready after restart", "name", name, "error", err)
		svc.alerts.Send(AlertError, fmt.Sprintf("%s: not ready after restart", name), map[string]interface{}{"error": err.Error()})
		fails.mu.Lock()
		fails.restarts++
		fails.lastRestart = time.Now()
		fails.mu.Unlock()
		return
	}

	svc.log.Info("service restarted", "name", name)
	fails.mu.Lock()
	fails.consecutive = 0
	fails.restarts++
	fails.lastRestart = time.Now()
	fails.mu.Unlock()
}

func (svc *services) allHealthy() (llama, cognee bool) {
	// Nil receiver: services are not wired yet (or Start never ran). Report
	// unhealthy rather than panicking — /health is a public endpoint and must
	// never take the process's connection handler down.
	if svc == nil {
		return false, false
	}
	// Use cached health with 10s TTL to avoid HTTP requests per tool call
	svc.healthMu.RLock()
	if time.Since(svc.healthChecked) < 10*time.Second {
		l, c := svc.healthCache[0], svc.healthCache[1]
		svc.healthMu.RUnlock()
		return l, c
	}
	svc.healthMu.RUnlock()

	// Cache expired — deduplicate concurrent refreshes via singleflight
	val, _, _ := svc.healthGroup.Do("health", func() (interface{}, error) {
		var l, c bool

		// Cloud embedding: llama is always "healthy"
		if svc.config.IsCloudEmbedding() {
			l = true
		}

		var wg sync.WaitGroup

		nChecks := 1 // cognee
		if !svc.config.IsCloudEmbedding() {
			nChecks++
		}
		wg.Add(nChecks)
		if !svc.config.IsCloudEmbedding() {
			go func() {
				defer func() {
					if rec := recover(); rec != nil {
						svc.log.Error("allHealthy panic", "service", "llama", "panic", fmt.Sprintf("%v", rec))
					}
				}()
				defer wg.Done()
				l = svc.check(healthURL(svc.config.LlamaPort)) == nil
			}()
		}
		go func() {
			defer func() {
				if rec := recover(); rec != nil {
					svc.log.Error("allHealthy panic", "service", "cognee", "panic", fmt.Sprintf("%v", rec))
				}
			}()
			defer wg.Done()
			c = svc.check(healthURL(svc.config.CogneePort)) == nil
		}()

		wg.Wait()

		svc.healthMu.Lock()
		svc.healthCache = [2]bool{l, c}
		svc.healthChecked = time.Now()
		svc.healthMu.Unlock()
		return [2]bool{l, c}, nil
	})
	result, ok := val.([2]bool)
	if !ok {
		return false, false
	}
	return result[0], result[1]
}

func healthURL(port string) string { return "http://localhost:" + port + "/health" }

func (svc *services) check(url string) error {
	timeout := svc.config.HealthTimeout
	if timeout > 5*time.Second {
		timeout = 5 * time.Second
	} // Cap health pings
	resp, err := httpGet(svc.httpClient, url, timeout)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("health check: status %d", resp.StatusCode)
	}
	return nil
}

func (svc *services) wait(ctx context.Context, url string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		resp, err := httpGet(svc.httpClient, url, 2*time.Second)
		if err == nil {
			resp.Body.Close()
			return nil
		}
		time.Sleep(1 * time.Second)
	}
	return errTimeout
}

// resolveLlamaPath resolves the llama-server binary path using a fallback chain:
// 1. config.LlamaPath (default: ./bin/llama/llama-server)
// 2. exec.LookPath("llama-server") — system PATH (brew, system package, etc.)
// Returns the resolved path or an error if no valid executable is found.
func (svc *services) resolveLlamaPath() (string, error) {
	// Candidate 1: configured path
	if svc.config.LlamaPath != "" {
		info, err := os.Stat(svc.config.LlamaPath)
		if err == nil && info.Mode().IsRegular() && info.Size() > 0 && info.Mode()&0111 != 0 {
			return svc.config.LlamaPath, nil
		}
	}

	// Candidate 2: system PATH lookup
	lp, err := exec.LookPath("llama-server")
	if err == nil {
		info, statErr := os.Stat(lp)
		if statErr == nil && info.Mode().IsRegular() && info.Size() > 0 && info.Mode()&0111 != 0 {
			return lp, nil
		}
	}

	return "", fmt.Errorf("llama-server not found at %q or on system PATH", svc.config.LlamaPath)
}

func (svc *services) startLlama() error {
	modelPath := svc.config.ModelPath
	if !filepath.IsAbs(modelPath) {
		wd, _ := os.Getwd()
		modelPath = filepath.Join(wd, modelPath)
	}
	if _, err := os.Stat(modelPath); os.IsNotExist(err) {
		return errModelNotFound(modelPath)
	}
	llamaPath, err := svc.resolveLlamaPath()
	if err != nil {
		return err
	}
	cmd := svc.spawn(llamaPath,
		"--model", modelPath, "--embedding",
		"--ctx-size", svc.config.CtxSize,
		"--parallel", "1", // Embeddings are single-request — only 1 slot needed
		"--cache-ram", "128", // Limit prompt cache to 128 MB
		"--cache-type-k", "q4_0",
		"--cache-type-v", "q4_0",
		"--n-gpu-layers", svc.config.GPULayers,
		"--port", svc.config.LlamaPort, "--host", svc.config.LlamaHost,
	)
	if cmd == nil {
		return fmt.Errorf("failed to spawn llama.cpp embedder")
	}
	svc.mu.Lock()
	svc.llamaCmd = cmd
	svc.mu.Unlock()
	return nil
}

// cogneeBaseEnv returns shared env vars common to both Cognee Python and Rust.
func (svc *services) cogneeBaseEnv() []string {
	return append(os.Environ(),
		// LLM config
		"LLM_API_KEY="+svc.config.CogneeLLMApiKey,
		"LLM_MODEL="+svc.config.CogneeLLMModel,
		"LLM_ENDPOINT="+svc.config.CogneeLLMEndpoint,
		"LLM_PROVIDER=openai",
		// Embedding config
		"EMBEDDING_ENDPOINT="+svc.config.CogneeEmbeddingEndpoint,
		"EMBEDDING_API_KEY=not-needed",
		"EMBEDDING_DIMENSIONS=1024",
		// Data isolation
		"COGNEE_DATA_DIR="+svc.config.CogneeDataDir,
		"HTTP_API_PORT="+svc.config.CogneePort,
		// Database providers
		"COGNEE_DB_PROVIDER="+getEnvOrDefault("COGNEE_DB_PROVIDER", "sqlite"),
		"VECTOR_DB_PROVIDER="+getEnvOrDefault("COGNEE_VECTOR_DB_PROVIDER", "lancedb"),
		"GRAPH_DB_PROVIDER="+getEnvOrDefault("COGNEE_GRAPH_DB_PROVIDER", "ladybug"),
		"ENABLE_BACKEND_ACCESS_CONTROL=false",
	)
}

// cogneeRustEnv returns env vars for the Cognee Rust (http-server) subprocess.
// Uses openai_compatible embedding provider (Rust does not support llama_cpp).
func (svc *services) cogneeRustEnv() []string {
	dataDir := svc.config.CogneeDataDir
	return append(svc.cogneeBaseEnv(),
		"EMBEDDING_PROVIDER=openai_compatible",
		"EMBEDDING_MODEL_NAME=qwen3-embedding-0.6b",
		// Rust binary uses DATA_ROOT_DIRECTORY / SYSTEM_ROOT_DIRECTORY
		"DATA_ROOT_DIRECTORY="+dataDir+"/data",
		"SYSTEM_ROOT_DIRECTORY="+dataDir+"/system",
	)
}

// startCogneeRust spawns the cognee-http-server Rust binary.
func (svc *services) startCogneeRust() error {
	binaryPath := svc.resolveCogneeBinary()
	if binaryPath == "" {
		return fmt.Errorf("COGNEE_BINARY not found")
	}

	cmd := exec.Command(binaryPath)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Env = svc.cogneeRustEnv()

	wd, _ := os.Getwd()
	f, _ := os.OpenFile(filepath.Join(wd, "logs", "cognee-crash.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0666)
	cmd.Stdout, cmd.Stderr = f, f
	if err := cmd.Start(); err != nil {
		if f != nil {
			f.Close()
		}
		return err
	}
	svc.mu.Lock()
	svc.cogneeCmd = cmd
	svc.mu.Unlock()
	svc.log.Info("cognee-rust started", "pid", cmd.Process.Pid)
	return nil
}

// resolveCogneeBinary resolves the Rust Cognee binary path.
// Fallback chain: config.CogneeBinary → cognee-http-server on PATH
func (svc *services) resolveCogneeBinary() string {
	if svc.config.CogneeBinary != "" {
		info, err := os.Stat(svc.config.CogneeBinary)
		if err == nil && info.Mode().IsRegular() && info.Size() > 0 && info.Mode()&0111 != 0 {
			return svc.config.CogneeBinary
		}
	}
	if lp, err := exec.LookPath("cognee-http-server"); err == nil {
		return lp
	}
	return ""
}

// getEnvOrDefault returns the env var value or the default if unset/empty.
func getEnvOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func (svc *services) spawn(name string, args ...string) *exec.Cmd {
	cmd := exec.Command(name, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	wd, _ := os.Getwd()
	f, err := os.OpenFile(filepath.Join(wd, "logs", "llama-crash.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0666)
	if err != nil {
		svc.log.Error("failed to open crash log", "error", err)
		return nil
	}
	cmd.Stderr = f
	cmd.Stdout = f
	if err := cmd.Start(); err != nil {
		f.Close()
		svc.log.Error("failed to start process", "name", name, "error", err)
		return nil
	}
	f.Close() // Child process inherited the fd; close our copy
	return cmd
}

func (svc *services) stopProcess(cmdPtr **exec.Cmd, name string) {
	svc.mu.Lock()
	cmd := *cmdPtr
	*cmdPtr = nil
	svc.mu.Unlock()
	if cmd == nil || cmd.Process == nil {
		return
	}
	pid := cmd.Process.Pid
	svc.log.Info("stopping service", "name", name, "pid", pid)
	// Kill process group to catch all children
	syscall.Kill(-pid, syscall.SIGTERM)
	done := make(chan error, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				svc.log.Error("stopProcess panic", "name", name, "panic", fmt.Sprintf("%v", r))
				done <- errProcessPanic
			}
		}()
		done <- cmd.Wait()
	}()
	t := time.NewTimer(svc.config.StopTimeout)
	select {
	case <-done:
		t.Stop()
	case <-t.C:
		svc.log.Warn("force killing service", "name", name)
		syscall.Kill(-pid, syscall.SIGKILL)
		cmd.Process.Kill()
		// Verify exit
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			svc.log.Error("process refused to die", "name", name, "pid", pid)
			svc.alerts.Send(AlertCritical, fmt.Sprintf("%s: refused to die after SIGKILL (PID %d)", name, pid), nil)
		}
	}
}

// waitAllHealthy polls until all services are healthy or timeout.
func (svc *services) waitAllHealthy(timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			l, c := svc.allHealthy()
			return fmt.Errorf("services not healthy after %v: llama=%v cognee=%v", timeout, l, c)
		case <-ticker.C:
			l, c := svc.allHealthy()
			if l && c {
				return nil
			}
		}
	}
}

func httpGet(client *http.Client, url string, timeout time.Duration) (*http.Response, error) {
	c := &http.Client{Timeout: timeout, Transport: client.Transport}
	return c.Get(url)
}
