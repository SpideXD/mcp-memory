package main

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// ─── Process lifecycle suite ─────────────────────────────────────────────────
//
// These tests launch the real mcp-memory binary as a subprocess and drive it
// with signals. Nothing here needs Cognee or an LLM: the Cognee child is a stub
// that serves /health, and llama is skipped by putting an https URL in
// LLAMA_MODEL_PATH (cloud-embedding mode). That makes the whole signal-handling
// and subprocess-teardown path testable in the hermetic suite.
//
// This is the layer that shipped with zero coverage and where both lifecycle
// defects lived: main() returning before Stop() could finish, and an orphan
// probe that never fired.

// stubSource is a minimal stand-in for cognee-http-server. It reads its port
// from HTTP_API_PORT the way the real binary does, and can be told to ignore
// SIGTERM so the SIGKILL escalation path is reachable.
const stubSource = `package main

import (
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	if os.Getenv("STUB_IGNORE_SIGTERM") == "1" {
		signal.Ignore(syscall.SIGTERM, syscall.SIGINT)
	}
	port := os.Getenv("HTTP_API_PORT")
	if port == "" {
		port = "8003"
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, ` + "`" + `{"status":"ready","health":"healthy","version":"stub"}` + "`" + `)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, ` + "`" + `{"status":"completed"}` + "`" + `)
	})
	http.ListenAndServe("127.0.0.1:"+port, mux)
}
`

var (
	buildOnce   sync.Once
	serverBin   string
	stubBin     string
	buildErrMsg string
)

// buildBinaries compiles mcp-memory and the Cognee stub once per test run.
func buildBinaries(t *testing.T) (server, stub string) {
	t.Helper()
	buildOnce.Do(func() {
		dir, err := os.MkdirTemp("", "mcp-lifecycle-bin")
		if err != nil {
			buildErrMsg = fmt.Sprintf("temp dir: %v", err)
			return
		}
		repo, err := os.Getwd()
		if err != nil {
			buildErrMsg = fmt.Sprintf("getwd: %v", err)
			return
		}

		serverBin = filepath.Join(dir, "mcp-memory")
		cmd := exec.Command("go", "build", "-o", serverBin, ".")
		cmd.Dir = repo
		if out, err := cmd.CombinedOutput(); err != nil {
			buildErrMsg = fmt.Sprintf("build mcp-memory: %v\n%s", err, out)
			return
		}

		stubDir := filepath.Join(dir, "stub")
		if err := os.MkdirAll(stubDir, 0o755); err != nil {
			buildErrMsg = fmt.Sprintf("stub dir: %v", err)
			return
		}
		if err := os.WriteFile(filepath.Join(stubDir, "main.go"), []byte(stubSource), 0o644); err != nil {
			buildErrMsg = fmt.Sprintf("write stub: %v", err)
			return
		}
		if err := os.WriteFile(filepath.Join(stubDir, "go.mod"),
			[]byte("module cogneestub\n\ngo 1.24\n"), 0o644); err != nil {
			buildErrMsg = fmt.Sprintf("write stub go.mod: %v", err)
			return
		}
		stubBin = filepath.Join(dir, "cognee-http-server")
		cmd = exec.Command("go", "build", "-o", stubBin, ".")
		cmd.Dir = stubDir
		if out, err := cmd.CombinedOutput(); err != nil {
			buildErrMsg = fmt.Sprintf("build stub: %v\n%s", err, out)
			return
		}
	})
	if buildErrMsg != "" {
		t.Fatalf("binary build failed: %s", buildErrMsg)
	}
	return serverBin, stubBin
}

// freePort reserves an ephemeral port and releases it for the child to bind.
func freePort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve port: %v", err)
	}
	defer l.Close()
	return fmt.Sprint(l.Addr().(*net.TCPAddr).Port)
}

// liveServer is a running mcp-memory subprocess under test.
type liveServer struct {
	t        *testing.T
	cmd      *exec.Cmd
	workDir  string
	httpPort string
	cogPort  string
	waited   chan error
}

type launchOpts struct {
	ignoreSigterm bool   // stub refuses SIGTERM, forcing SIGKILL escalation
	httpPort      string // pre-chosen port, to provoke a bind conflict
}

// launchServer starts mcp-memory in an isolated working directory with a stub
// Cognee and cloud-embedding mode (so llama is never spawned).
func launchServer(t *testing.T, opts launchOpts) *liveServer {
	t.Helper()
	server, stub := buildBinaries(t)

	workDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(workDir, "logs"), 0o755); err != nil {
		t.Fatalf("mkdir logs: %v", err)
	}

	httpPort := opts.httpPort
	if httpPort == "" {
		httpPort = freePort(t)
	}
	cogPort := freePort(t)

	cmd := exec.Command(server)
	cmd.Dir = workDir
	cmd.Env = append(os.Environ(),
		"BACKEND=cognee-rust",
		"COGNEE_BINARY="+stub,
		"COGNEE_PORT="+cogPort,
		"MCP_PORT="+httpPort,
		"MCP_HOST=127.0.0.1",
		// https path => IsCloudEmbedding() => llama.cpp is skipped entirely
		"LLAMA_MODEL_PATH=https://embeddings.invalid/v1",
		"COGNEE_DATA_DIR="+filepath.Join(workDir, "cognee-data"),
		"QUEUE_DB_PATH="+filepath.Join(workDir, "queue.db"),
		"SERVICE_START_TIMEOUT=20s",
		"SERVICE_STOP_TIMEOUT=2s",
		"SHUTDOWN_TIMEOUT=5s",
		"ALERT_URL=",
		"ERROR_WEBHOOK_URL=",
		"AUTO_REFLECT_AFTER_N=0",
		"AUTO_IMPROVE_AFTER_N=0",
		"COGNEE_LLM_API_KEY=test-key-for-preflight",
	)
	if opts.ignoreSigterm {
		cmd.Env = append(cmd.Env, "STUB_IGNORE_SIGTERM=1")
	}

	logFile, err := os.Create(filepath.Join(workDir, "stdio.log"))
	if err != nil {
		t.Fatalf("create stdio log: %v", err)
	}
	cmd.Stdout, cmd.Stderr = logFile, logFile
	// Own process group, so a signal to the test binary never leaks to it.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	if err := cmd.Start(); err != nil {
		t.Fatalf("start server: %v", err)
	}

	ls := &liveServer{
		t: t, cmd: cmd, workDir: workDir,
		httpPort: httpPort, cogPort: cogPort,
		waited: make(chan error, 1),
	}
	go func() { ls.waited <- cmd.Wait(); logFile.Close() }()

	t.Cleanup(func() {
		if ls.running() {
			syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			<-ls.waited
		}
		ls.killStubs()
	})
	return ls
}

// killStubs is a backstop so a failing test never leaves a stub behind.
func (ls *liveServer) killStubs() {
	if pid := ls.cogneePID(); pid > 0 {
		if proc, err := os.FindProcess(pid); err == nil && processAlive(proc) {
			_ = syscall.Kill(-pid, syscall.SIGKILL)
			_ = proc.Kill()
		}
	}
}

func (ls *liveServer) running() bool {
	select {
	case err := <-ls.waited:
		ls.waited <- err // put it back for later readers
		return false
	default:
		return true
	}
}

// waitHealthy blocks until /health reports running.
func (ls *liveServer) waitHealthy(timeout time.Duration) {
	ls.t.Helper()
	deadline := time.Now().Add(timeout)
	client := &http.Client{Timeout: 2 * time.Second}
	url := "http://127.0.0.1:" + ls.httpPort + "/health"
	for time.Now().Before(deadline) {
		if resp, err := client.Get(url); err == nil {
			var h struct {
				Status string `json:"status"`
			}
			json.NewDecoder(resp.Body).Decode(&h)
			resp.Body.Close()
			if h.Status == "running" {
				return
			}
		}
		if !ls.running() {
			ls.t.Fatalf("server exited during startup\n%s", ls.stdio())
		}
		time.Sleep(100 * time.Millisecond)
	}
	ls.t.Fatalf("server not healthy within %v\n%s", timeout, ls.stdio())
}

// cogneePID reads the child PID that the server recorded.
func (ls *liveServer) cogneePID() int {
	data, err := os.ReadFile(filepath.Join(ls.workDir, "logs/.mcp-pids.json"))
	if err != nil {
		return 0
	}
	entries, ok := parsePidFile(data)
	if !ok {
		return 0
	}
	return entries["cognee"].PID
}

func (ls *liveServer) pidFileExists() bool {
	_, err := os.Stat(filepath.Join(ls.workDir, "logs/.mcp-pids.json"))
	return err == nil
}

func (ls *liveServer) stdio() string {
	b, _ := os.ReadFile(filepath.Join(ls.workDir, "stdio.log"))
	return "--- stdio ---\n" + string(b)
}

// structuredLog returns the server's JSON log messages in order.
func (ls *liveServer) structuredLog() []string {
	b, err := os.ReadFile(filepath.Join(ls.workDir, "logs/memory.log"))
	if err != nil {
		return nil
	}
	var msgs []string
	for _, line := range strings.Split(string(b), "\n") {
		if line == "" {
			continue
		}
		var e struct {
			Msg string `json:"msg"`
		}
		if json.Unmarshal([]byte(line), &e) == nil && e.Msg != "" {
			msgs = append(msgs, e.Msg)
		}
	}
	return msgs
}

func (ls *liveServer) loggedMessage(want string) bool {
	for _, m := range ls.structuredLog() {
		if m == want {
			return true
		}
	}
	return false
}

// signalAndWait sends sig to the server and waits for it to exit.
func (ls *liveServer) signalAndWait(sig syscall.Signal, timeout time.Duration) error {
	ls.t.Helper()
	if err := ls.cmd.Process.Signal(sig); err != nil {
		ls.t.Fatalf("signal %v: %v", sig, err)
	}
	select {
	case err := <-ls.waited:
		ls.waited <- err
		return err
	case <-time.After(timeout):
		ls.t.Fatalf("server did not exit within %v of %v\n%s", timeout, sig, ls.stdio())
		return nil
	}
}

func processGone(pid int, timeout time.Duration) bool {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return true
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if !processAlive(proc) {
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
	return !processAlive(proc)
}

// ─── Graceful shutdown ───────────────────────────────────────────────────────

// TestLifecycle_SIGTERMStopsChildAndClearsPidFile is the regression guard for
// the shutdown leak. httpSrv.Shutdown closes the listener, so ListenAndServe
// returned ErrServerClosed and main() fell off the end — the process exited
// while the shutdown goroutine was still inside srv.Stop(), leaking the Cognee
// child and leaving .mcp-pids.json behind. Stop() takes seconds, so it lost
// that race nearly every time: 14 shutdown signals in the field log, 3
// completed.
func TestLifecycle_SIGTERMStopsChildAndClearsPidFile(t *testing.T) {
	if testing.Short() {
		t.Skip("builds binaries; skipped under -short")
	}
	ls := launchServer(t, launchOpts{})
	ls.waitHealthy(30 * time.Second)

	childPID := ls.cogneePID()
	if childPID == 0 {
		t.Fatalf("server recorded no Cognee child PID\n%s", ls.stdio())
	}
	proc, _ := os.FindProcess(childPID)
	if !processAlive(proc) {
		t.Fatal("Cognee child was not running before shutdown")
	}

	ls.signalAndWait(syscall.SIGTERM, 30*time.Second)

	if !processGone(childPID, 10*time.Second) {
		t.Errorf("Cognee child %d survived SIGTERM — leaked as an orphan", childPID)
	}
	if ls.pidFileExists() {
		t.Error(".mcp-pids.json was not cleared on graceful shutdown")
	}
	if !ls.loggedMessage("shutdown complete") {
		t.Errorf("shutdown never completed; log was:\n%v\n%s", ls.structuredLog(), ls.stdio())
	}
	for _, want := range []string{"shutdown signal received", "shutting down", "stopping service"} {
		if !ls.loggedMessage(want) {
			t.Errorf("shutdown skipped %q", want)
		}
	}
}

// TestLifecycle_SIGINTStopsChild covers the other signal main() registers.
func TestLifecycle_SIGINTStopsChild(t *testing.T) {
	if testing.Short() {
		t.Skip("builds binaries; skipped under -short")
	}
	ls := launchServer(t, launchOpts{})
	ls.waitHealthy(30 * time.Second)

	childPID := ls.cogneePID()
	if childPID == 0 {
		t.Fatalf("no child PID recorded\n%s", ls.stdio())
	}

	ls.signalAndWait(syscall.SIGINT, 30*time.Second)

	if !processGone(childPID, 10*time.Second) {
		t.Errorf("Cognee child %d survived SIGINT", childPID)
	}
	if !ls.loggedMessage("shutdown complete") {
		t.Error("SIGINT did not complete shutdown")
	}
}

// TestLifecycle_StubIgnoringSIGTERMIsForceKilled exercises the escalation path
// in stopProcess: SIGTERM, wait StopTimeout, then SIGKILL the process group.
func TestLifecycle_StubIgnoringSIGTERMIsForceKilled(t *testing.T) {
	if testing.Short() {
		t.Skip("builds binaries; skipped under -short")
	}
	ls := launchServer(t, launchOpts{ignoreSigterm: true})
	ls.waitHealthy(30 * time.Second)

	childPID := ls.cogneePID()
	if childPID == 0 {
		t.Fatalf("no child PID recorded\n%s", ls.stdio())
	}

	ls.signalAndWait(syscall.SIGTERM, 40*time.Second)

	if !processGone(childPID, 15*time.Second) {
		t.Errorf("child %d ignoring SIGTERM was never SIGKILLed — "+
			"the escalation path does not work", childPID)
	}
	if !ls.loggedMessage("force killing service") {
		t.Errorf("expected a force-kill warning; log was:\n%v", ls.structuredLog())
	}
}

// TestLifecycle_ShutdownCompletesWithOpenSSEConnection is the case most likely
// to hang: httpSrv.Shutdown waits for in-flight requests, and an SSE stream
// never finishes on its own. Shutdown must still terminate within
// SHUTDOWN_TIMEOUT and go on to stop the child.
func TestLifecycle_ShutdownCompletesWithOpenSSEConnection(t *testing.T) {
	if testing.Short() {
		t.Skip("builds binaries; skipped under -short")
	}
	ls := launchServer(t, launchOpts{})
	ls.waitHealthy(30 * time.Second)

	// Hold an SSE stream open for the duration.
	req, err := http.NewRequest("GET",
		"http://127.0.0.1:"+ls.httpPort+"/mcp/sse?bank=lifecycle", nil)
	if err != nil {
		t.Fatalf("build SSE request: %v", err)
	}
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		t.Fatalf("open SSE: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("SSE status %d", resp.StatusCode)
	}

	childPID := ls.cogneePID()
	start := time.Now()
	ls.signalAndWait(syscall.SIGTERM, 40*time.Second)
	elapsed := time.Since(start)

	if elapsed > 30*time.Second {
		t.Errorf("shutdown took %v with an open SSE stream — it should be bounded "+
			"by SHUTDOWN_TIMEOUT", elapsed)
	}
	if childPID > 0 && !processGone(childPID, 10*time.Second) {
		t.Errorf("child %d leaked when shutting down with an open SSE stream", childPID)
	}
	if !ls.loggedMessage("shutdown complete") {
		t.Errorf("shutdown did not complete with an open SSE stream; log:\n%v",
			ls.structuredLog())
	}
}

// ─── Crash path and orphan reaping ───────────────────────────────────────────

// TestLifecycle_CrashLeavesOrphanThenNextStartReapsIt is the end-to-end crash
// story: SIGKILL cannot run cleanup, so the child is orphaned and the PID file
// survives by design. The next start must then reap it. Before the probe fix
// that second half silently did nothing, so orphans accumulated across crashes.
func TestLifecycle_CrashLeavesOrphanThenNextStartReapsIt(t *testing.T) {
	if testing.Short() {
		t.Skip("builds binaries; skipped under -short")
	}
	server, stub := buildBinaries(t)

	workDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(workDir, "logs"), 0o755); err != nil {
		t.Fatalf("mkdir logs: %v", err)
	}
	httpPort, cogPort := freePort(t), freePort(t)

	env := append(os.Environ(),
		"BACKEND=cognee-rust",
		"COGNEE_BINARY="+stub,
		"COGNEE_PORT="+cogPort,
		"MCP_PORT="+httpPort,
		"MCP_HOST=127.0.0.1",
		"LLAMA_MODEL_PATH=https://embeddings.invalid/v1",
		"COGNEE_DATA_DIR="+filepath.Join(workDir, "cognee-data"),
		"QUEUE_DB_PATH="+filepath.Join(workDir, "queue.db"),
		"SERVICE_START_TIMEOUT=20s",
		"SERVICE_STOP_TIMEOUT=2s",
		"SHUTDOWN_TIMEOUT=5s",
		"ALERT_URL=", "ERROR_WEBHOOK_URL=",
		"AUTO_REFLECT_AFTER_N=0", "AUTO_IMPROVE_AFTER_N=0",
		"COGNEE_LLM_API_KEY=test-key-for-preflight",
	)

	run := func(tag string) (*exec.Cmd, chan error) {
		cmd := exec.Command(server)
		cmd.Dir = workDir
		cmd.Env = env
		f, err := os.Create(filepath.Join(workDir, "stdio-"+tag+".log"))
		if err != nil {
			t.Fatalf("create log: %v", err)
		}
		cmd.Stdout, cmd.Stderr = f, f
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		if err := cmd.Start(); err != nil {
			t.Fatalf("start %s: %v", tag, err)
		}
		done := make(chan error, 1)
		go func() { done <- cmd.Wait(); f.Close() }()
		return cmd, done
	}

	readPID := func() int {
		data, err := os.ReadFile(filepath.Join(workDir, "logs/.mcp-pids.json"))
		if err != nil {
			return 0
		}
		entries, ok := parsePidFile(data)
		if !ok {
			return 0
		}
		return entries["cognee"].PID
	}

	waitHealthy := func(done chan error, tag string) {
		client := &http.Client{Timeout: 2 * time.Second}
		deadline := time.Now().Add(30 * time.Second)
		for time.Now().Before(deadline) {
			if resp, err := client.Get("http://127.0.0.1:" + httpPort + "/health"); err == nil {
				var h struct {
					Status string `json:"status"`
				}
				json.NewDecoder(resp.Body).Decode(&h)
				resp.Body.Close()
				if h.Status == "running" {
					return
				}
			}
			select {
			case err := <-done:
				b, _ := os.ReadFile(filepath.Join(workDir, "stdio-"+tag+".log"))
				t.Fatalf("%s exited during startup (%v):\n%s", tag, err, b)
			default:
			}
			time.Sleep(100 * time.Millisecond)
		}
		b, _ := os.ReadFile(filepath.Join(workDir, "stdio-"+tag+".log"))
		t.Fatalf("%s never became healthy:\n%s", tag, b)
	}

	// Run 1: start, then crash it hard.
	cmd1, done1 := run("run1")
	waitHealthy(done1, "run1")
	orphanPID := readPID()
	if orphanPID == 0 {
		t.Fatal("run1 recorded no child PID")
	}
	if err := cmd1.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatalf("SIGKILL run1: %v", err)
	}
	<-done1

	orphan, _ := os.FindProcess(orphanPID)
	t.Cleanup(func() {
		if processAlive(orphan) {
			_ = syscall.Kill(-orphanPID, syscall.SIGKILL)
			_ = orphan.Kill()
		}
	})

	// A hard kill cannot clean up: the child must still be alive and the PID
	// file must remain so the next start can find it.
	if !processAlive(orphan) {
		t.Fatal("child died with the crashed parent; nothing to reap (test setup invalid)")
	}
	if readPID() == 0 {
		t.Fatal("PID file did not survive the crash — orphan recovery has no input")
	}

	// Run 2: startup must reap the orphan.
	cmd2, done2 := run("run2")
	_ = cmd2
	waitHealthy(done2, "run2")

	if !processGone(orphanPID, 15*time.Second) {
		t.Errorf("orphan %d survived the next startup — cleanupOrphans is a no-op", orphanPID)
	}
	stdio2, _ := os.ReadFile(filepath.Join(workDir, "stdio-run2.log"))
	if !strings.Contains(string(stdio2), "killing orphaned") {
		t.Errorf("startup did not report reaping the orphan:\n%s", stdio2)
	}

	// Tidy: the new instance owns a fresh child.
	newPID := readPID()
	if newPID == orphanPID {
		t.Error("run2 reported the reaped orphan as its own child")
	}
	cmd2.Process.Signal(syscall.SIGTERM)
	select {
	case <-done2:
	case <-time.After(30 * time.Second):
		syscall.Kill(-cmd2.Process.Pid, syscall.SIGKILL)
	}
	if newPID > 0 {
		processGone(newPID, 10*time.Second)
	}
}

// ─── Startup failure ─────────────────────────────────────────────────────────

// TestLifecycle_BindFailureDoesNotLeakChild covers the other exit path in
// main(): services are started before ListenAndServe, so a port conflict must
// still tear them down instead of exiting and orphaning them.
func TestLifecycle_BindFailureDoesNotLeakChild(t *testing.T) {
	if testing.Short() {
		t.Skip("builds binaries; skipped under -short")
	}
	// Occupy the HTTP port for the whole test so the server cannot bind it.
	blocker, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer blocker.Close()
	taken := fmt.Sprint(blocker.Addr().(*net.TCPAddr).Port)

	ls := launchServer(t, launchOpts{httpPort: taken})

	// Capture the child PID as soon as the server records it. Reading only
	// after exit is not enough: a successful teardown deletes the PID file, so
	// "no PID recorded" and "cleaned up correctly" look identical.
	var childPID int
	grabDeadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(grabDeadline) {
		if pid := ls.cogneePID(); pid > 0 {
			childPID = pid
			break
		}
		if !ls.running() {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	// The server must exit on its own, non-zero.
	var exitErr error
	select {
	case exitErr = <-ls.waited:
		ls.waited <- exitErr
	case <-time.After(60 * time.Second):
		t.Fatalf("server did not exit after failing to bind\n%s", ls.stdio())
	}
	if exitErr == nil {
		t.Error("a bind failure should be a non-zero exit")
	}

	// Independent of the PID file: if the child leaked it still holds cogPort.
	if !portFreed(ls.cogPort, 15*time.Second) {
		t.Errorf("Cognee port %s still bound after the bind failure — the child leaked",
			ls.cogPort)
	}
	if childPID == 0 {
		t.Fatal("never observed a child PID, so the leak check is inconclusive; " +
			"savePids should run inside Start() before ListenAndServe")
	}
	if !processGone(childPID, 15*time.Second) {
		t.Errorf("Cognee child %d leaked after the HTTP bind failure", childPID)
	}
	if ls.pidFileExists() {
		t.Error(".mcp-pids.json left behind after a bind failure")
	}
}

// portFreed reports whether nothing is listening on port within timeout.
func portFreed(port string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", "127.0.0.1:"+port, 200*time.Millisecond)
		if err != nil {
			return true
		}
		conn.Close()
		time.Sleep(100 * time.Millisecond)
	}
	return false
}
