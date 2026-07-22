package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"golang.org/x/sync/singleflight"

	"mcp-memory/logger"
)

var errProcessPanic = fmt.Errorf("process goroutine panicked")

type services struct {
	config           Config
	llamaCmd         *exec.Cmd
	llamaRerankerCmd *exec.Cmd
	hindsightCmd     *exec.Cmd
	httpClient       *http.Client
	mu               sync.Mutex
	log              *logger.Logger
	alerts           *AlertClient

	// Cached health status to avoid 3 HTTP requests per tool call
	healthMu      sync.RWMutex
	healthCache   [3]bool // llama, reranker, hindsight
	healthChecked time.Time
	healthGroup   singleflight.Group // deduplicate concurrent health refreshes

	// Per-service fail/restart tracking for backoff
	llamaFails     serviceFails
	rerankerFails  serviceFails
	hindsightFails serviceFails
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
	rerankerURL := healthURL(svc.config.LlamaRerankerPort)
	hindsightURL := healthURL(svc.config.HindsightPort)

	if svc.config.IsCloudEmbedding() {
		svc.log.Info("llama.cpp skipped (cloud embedding mode)")
	} else if svc.check(llamaURL) != nil {
		if err := svc.startLlama(); err != nil { return err }
		if err := svc.wait(context.Background(), llamaURL, svc.config.StartTimeout); err != nil { return err }
		svc.log.Info("llama.cpp started")
	} else {
		svc.log.Info("llama.cpp already running")
	}
	if svc.config.IsCloudReranker() {
		svc.log.Info("llama reranker skipped (cloud reranker mode)")
	} else if svc.check(rerankerURL) != nil {
		if err := svc.startLlamaReranker(); err != nil { return err }
		if err := svc.wait(context.Background(), rerankerURL, svc.config.StartTimeout); err != nil { return err }
		svc.log.Info("llama reranker started")
	} else {
		svc.log.Info("llama reranker already running")
	}
	if svc.check(hindsightURL) != nil {
		if err := svc.startHindsight(); err != nil { return err }
		if err := svc.wait(context.Background(), hindsightURL, svc.config.StartTimeout); err != nil { return err }
		svc.log.Info("Hindsight started")
	} else {
		svc.log.Info("Hindsight already running")
	}
	return nil
}

func (svc *services) stop() {
	svc.stopProcess(&svc.hindsightCmd, "Hindsight")
	if !svc.config.IsCloudReranker() {
		svc.stopProcess(&svc.llamaRerankerCmd, "llama.cpp reranker")
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
			// Spawn each service check in its own goroutine to prevent
			// a slow restart on one service from blocking others.
			if !svc.config.IsCloudEmbedding() {
				go svc.checkAndRestart(ctx, "llama.cpp", healthURL(svc.config.LlamaPort),
					&svc.llamaCmd, svc.startLlama, &svc.llamaFails, maxRestartsPerHour)
			}

			if !svc.config.IsCloudReranker() {
				go svc.checkAndRestart(ctx, "llama reranker", healthURL(svc.config.LlamaRerankerPort),
					&svc.llamaRerankerCmd, svc.startLlamaReranker, &svc.rerankerFails, maxRestartsPerHour)
			}

			go svc.checkAndRestart(ctx, "Hindsight", healthURL(svc.config.HindsightPort),
				&svc.hindsightCmd, svc.startHindsight, &svc.hindsightFails, maxRestartsPerHour)

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
	if backoff < 1*time.Second { backoff = 1 * time.Second }
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

func (svc *services) allHealthy() (llama, reranker, hindsight bool) {
	// Use cached health with 10s TTL to avoid 3 HTTP requests per tool call
	svc.healthMu.RLock()
	if time.Since(svc.healthChecked) < 10*time.Second {
		l, r, h := svc.healthCache[0], svc.healthCache[1], svc.healthCache[2]
		svc.healthMu.RUnlock()
		return l, r, h
	}
	svc.healthMu.RUnlock()

	// Cache expired — deduplicate concurrent refreshes via singleflight
	val, _, _ := svc.healthGroup.Do("health", func() (interface{}, error) {
		var l, r, h bool

		// Count how many goroutines we actually need to launch
		nChecks := 0
		if !svc.config.IsCloudEmbedding() { nChecks++ }
		if !svc.config.IsCloudReranker() { nChecks++ }
		nChecks++ // hindsight always checked

		// Cloud services are always "healthy" — no local process to check
		if svc.config.IsCloudEmbedding() {
			l = true
		}
		if svc.config.IsCloudReranker() {
			r = true
		}

		var wg sync.WaitGroup
		wg.Add(nChecks)

		if !svc.config.IsCloudEmbedding() {
			go func() {
				defer func() { if r := recover(); r != nil { svc.log.Error("allHealthy panic", "service", "llama", "panic", fmt.Sprintf("%v", r)) } }()
				defer wg.Done()
				l = svc.check(healthURL(svc.config.LlamaPort)) == nil
			}()
		}
		if !svc.config.IsCloudReranker() {
			go func() {
				defer func() { if r := recover(); r != nil { svc.log.Error("allHealthy panic", "service", "reranker", "panic", fmt.Sprintf("%v", r)) } }()
				defer wg.Done()
				r = svc.check(healthURL(svc.config.LlamaRerankerPort)) == nil
			}()
		}
		go func() {
			defer func() { if r := recover(); r != nil { svc.log.Error("allHealthy panic", "service", "hindsight", "panic", fmt.Sprintf("%v", r)) } }()
			defer wg.Done()
			h = svc.check(healthURL(svc.config.HindsightPort)) == nil
		}()
		wg.Wait()

		svc.healthMu.Lock()
		svc.healthCache = [3]bool{l, r, h}
		svc.healthChecked = time.Now()
		svc.healthMu.Unlock()
		return [3]bool{l, r, h}, nil
	})
	result, ok := val.([3]bool)
	if !ok {
		return false, false, false
	}
	return result[0], result[1], result[2]
}

func healthURL(port string) string { return "http://localhost:" + port + "/health" }

func (svc *services) check(url string) error {
	timeout := svc.config.HealthTimeout
	if timeout > 5*time.Second { timeout = 5 * time.Second } // Cap health pings
	resp, err := httpGet(svc.httpClient, url, timeout)
	if err != nil { return err }
	resp.Body.Close()
	if resp.StatusCode != 200 { return fmt.Errorf("health check: status %d", resp.StatusCode) }
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
		if err == nil { resp.Body.Close(); return nil }
		time.Sleep(1 * time.Second)
	}
	return errTimeout
}

func (svc *services) startLlama() error {
	modelPath := svc.config.ModelPath
	if !filepath.IsAbs(modelPath) {
		wd, _ := os.Getwd(); modelPath = filepath.Join(wd, modelPath)
	}
	if _, err := os.Stat(modelPath); os.IsNotExist(err) {
		return errModelNotFound(modelPath)
	}
	cmd := svc.spawn(svc.config.LlamaPath,
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
	svc.mu.Lock(); svc.llamaCmd = cmd; svc.mu.Unlock()
	return nil
}

func (svc *services) startLlamaReranker() error {
	modelPath := svc.config.RerankerModel
	if !filepath.IsAbs(modelPath) {
		wd, _ := os.Getwd(); modelPath = filepath.Join(wd, modelPath)
	}
	if _, err := os.Stat(modelPath); os.IsNotExist(err) {
		return errModelNotFound(modelPath)
	}
	cmd := svc.spawn(svc.config.LlamaPath,
		"--model", modelPath, "--reranking",
		"--ctx-size", svc.config.CtxSize,
		"--parallel", "1",
		"--cache-ram", "64",
		"--cache-type-k", "q8_0",
		"--cache-type-v", "q8_0",
		"--n-gpu-layers", svc.config.GPULayers,
		"--port", svc.config.LlamaRerankerPort, "--host", svc.config.LlamaHost,
	)
	if cmd == nil {
		return fmt.Errorf("failed to spawn llama.cpp reranker")
	}
	svc.mu.Lock(); svc.llamaRerankerCmd = cmd; svc.mu.Unlock()
	return nil
}

func (svc *services) startHindsight() error {
	// Don't double-spawn — port conflict
	svc.mu.Lock()
	if svc.hindsightCmd != nil && svc.hindsightCmd.Process != nil {
		if svc.check(healthURL(svc.config.HindsightPort)) == nil {
			svc.mu.Unlock()
			return nil // Already running and healthy
		}
	}
	svc.mu.Unlock()
	hindsightPath := svc.config.HindsightPath
	if _, err := exec.LookPath(hindsightPath); err != nil {
		// Build platform-aware fallback candidates
		venvBin := filepath.Join(".venv", "bin", "hindsight-api")
		if runtime.GOOS == "windows" {
			venvBin = filepath.Join(".venv", "Scripts", "hindsight-api.exe")
		}
		for _, p := range []string{
			venvBin,
			"/Library/Frameworks/Python.framework/Versions/3.12/bin/hindsight-api",
			"/usr/local/bin/hindsight-api",
			filepath.Join(os.Getenv("HOME"), ".local/bin/hindsight-api"),
		} {
			info, err := os.Stat(p)
			if err != nil {
				continue
			}
			if !info.Mode().IsRegular() {
				continue
			}
			if info.Size() == 0 {
				continue
			}
			if info.Mode()&0111 == 0 {
				continue
			}
			hindsightPath = p
			break
		}
		if hindsightPath == svc.config.HindsightPath { return errBinaryNotFound }
	}
	env := os.Environ()
	// Block torch/sklearn from being importable — these 400MB+ libraries
	// get lazy-loaded by Hindsight's query_analyzer and local-ml backends
	// even when provider=cohere. Hindsight gracefully falls back via
	// ImportError when these packages aren't available.
	env = append(env,
		"TORCH_UNAVAILABLE=1",
		"PYTHON_DISABLE_TORCH=1",
	)
	env = append(env,
		"HINDSIGHT_API_LLM_PROVIDER="+svc.config.LLMProvider,
		"HINDSIGHT_API_LLM_API_KEY="+svc.config.LLMAPIKey,
		"HINDSIGHT_API_LLM_MODEL="+svc.config.LLMModel,
		"HINDSIGHT_API_LLM_BASE_URL="+svc.config.LLMBaseURL,
	)

	// Embedding env vars: branch on cloud vs local
	env = append(env, "HINDSIGHT_API_EMBEDDINGS_PROVIDER="+svc.config.EmbedProvider)
	if svc.config.IsCloudEmbedding() {
		env = append(env,
			"HINDSIGHT_API_EMBEDDINGS_OPENAI_API_KEY="+svc.config.CloudEmbeddingAPIKey,
			"HINDSIGHT_API_EMBEDDINGS_OPENAI_BASE_URL="+svc.config.CloudEmbeddingURL,
			"HINDSIGHT_API_EMBEDDINGS_OPENAI_MODEL="+svc.config.CloudEmbeddingModel,
		)
	} else {
		env = append(env,
			"HINDSIGHT_API_EMBEDDINGS_OPENAI_API_KEY=not-needed",
			"HINDSIGHT_API_EMBEDDINGS_OPENAI_BASE_URL=http://localhost:"+svc.config.LlamaPort+"/v1",
			"HINDSIGHT_API_EMBEDDINGS_OPENAI_MODEL="+svc.config.EmbedModel,
		)
	}

	// Reranker env vars: branch on cloud vs local
	env = append(env, "HINDSIGHT_API_RERANKER_PROVIDER="+svc.config.RerankerProvider)
	if svc.config.IsCloudReranker() {
		env = append(env,
			"HINDSIGHT_API_RERANKER_COHERE_API_KEY="+svc.config.CloudRerankerAPIKey,
			"HINDSIGHT_API_RERANKER_COHERE_BASE_URL="+svc.config.CloudRerankerURL,
			"HINDSIGHT_API_RERANKER_COHERE_MODEL="+svc.config.CloudRerankerModel,
		)
	} else {
		env = append(env,
			"HINDSIGHT_API_RERANKER_COHERE_API_KEY=not-needed",
			"HINDSIGHT_API_RERANKER_COHERE_BASE_URL=http://localhost:"+svc.config.LlamaRerankerPort+"/v1/rerank",
			"HINDSIGHT_API_RERANKER_COHERE_MODEL="+filepath.Base(svc.config.RerankerModel),
		)
	}

	env = append(env, "HINDSIGHT_API_PORT="+svc.config.HindsightPort)
	cmd := exec.Command(hindsightPath, "--port", svc.config.HindsightPort)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Env = env
	wd, _ := os.Getwd()
	f, _ := os.OpenFile(filepath.Join(wd, "logs", "hindsight-crash.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0666)
	cmd.Stdout, cmd.Stderr = f, f
	if err := cmd.Start(); err != nil { return err }
	svc.mu.Lock(); svc.hindsightCmd = cmd; svc.mu.Unlock()
	svc.log.Info("Hindsight started", "pid", cmd.Process.Pid)
	return nil
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
		defer func() { if r := recover(); r != nil { svc.log.Error("stopProcess panic", "name", name, "panic", fmt.Sprintf("%v", r)); done <- errProcessPanic } }()
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
			l, r, h := svc.allHealthy()
			return fmt.Errorf("services not healthy after %v: llama=%v reranker=%v hindsight=%v", timeout, l, r, h)
		case <-ticker.C:
			l, r, h := svc.allHealthy()
			if l && r && h { return nil }
		}
	}
}

func httpGet(client *http.Client, url string, timeout time.Duration) (*http.Response, error) {
	c := &http.Client{Timeout: timeout, Transport: client.Transport}
	return c.Get(url)
}
