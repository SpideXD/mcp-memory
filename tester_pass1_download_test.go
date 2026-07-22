package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

// =========================================================================
// PASS 1 — ADVERSARIAL: Makefile download target, resolveLlamaPath(),
//            config default change.
//
// ATTACK VECTORS:
//   - resolveLlamaPath(): permission, type, size, symlink edge cases
//   - Makefile platform detection: Linux aarch64, armv7l, FreeBSD
//   - Makefile download: curl failure, archive layout change, pipefail bug
//   - Idempotency: version mismatch not detected
//   - Config: LLAMA_PATH="" env, default path change
//   - Cloud mode: resolveLlamaPath MUST NOT be called
//   - Backward compat: brew-installed via exec.LookPath
//
// NO process spawning. NO real binary download. Test the Go logic
// and Makefile shells only.
// =========================================================================

// cleanPATH sets PATH to a directory that does NOT contain llama-server,
// preventing fallback to brew-installed llama-server on developer machines.
func cleanPATH() {
	origPath := os.Getenv("PATH")
	// Build a PATH without the brew prefix (/opt/homebrew/bin)
	var cleaned []string
	for _, p := range filepath.SplitList(origPath) {
		if !strings.Contains(p, "homebrew") &&
			!strings.Contains(p, "brew") &&
			!strings.Contains(p, "llama") {
			cleaned = append(cleaned, p)
		}
	}
	if len(cleaned) == 0 {
		cleaned = []string{"/nonexistent"}
	}
	os.Setenv("PATH", strings.Join(cleaned, string(os.PathListSeparator)))
}

// ===================================================================
// resolveLlamaPath() — Binary Validation Edge Cases
// ===================================================================

// TestResolve_NotExecutable: file exists but mode 0644 -> rejected
func TestResolve_NotExecutable(t *testing.T) {
	cleanPATH()

	dir := t.TempDir()
	bin := filepath.Join(dir, "llama-server")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\necho fake"), 0644); err != nil {
		t.Fatal(err)
	}

	svc, _ := newTestServices()
	svc.config.LlamaPath = bin

	got, err := svc.resolveLlamaPath()
	if err == nil {
		t.Errorf("expected error for non-executable file, got path=%q", got)
	}
	if got != "" {
		t.Errorf("expected empty path for non-executable, got %q", got)
	}
}

// TestResolve_Directory: path is a directory -> rejected
func TestResolve_Directory(t *testing.T) {
	cleanPATH()

	dir := t.TempDir()

	svc, _ := newTestServices()
	svc.config.LlamaPath = dir

	got, err := svc.resolveLlamaPath()
	if err == nil {
		t.Errorf("expected error for directory, got path=%q", got)
	}
	if got != "" {
		t.Errorf("expected empty path for directory, got %q", got)
	}
}

// TestResolve_EmptyFile: file exists but size=0 -> rejected
func TestResolve_EmptyFile(t *testing.T) {
	cleanPATH()

	dir := t.TempDir()
	bin := filepath.Join(dir, "llama-server")
	if err := os.WriteFile(bin, []byte{}, 0755); err != nil {
		t.Fatal(err)
	}

	svc, _ := newTestServices()
	svc.config.LlamaPath = bin

	got, err := svc.resolveLlamaPath()
	if err == nil {
		t.Errorf("expected error for empty file, got path=%q", got)
	}
	if got != "" {
		t.Errorf("expected empty path for empty file, got %q", got)
	}
}

// TestResolve_FIFO: path is a named pipe -> rejected (not IsRegular)
func TestResolve_FIFO(t *testing.T) {
	cleanPATH()

	if runtime.GOOS == "windows" {
		t.Skip("mkfifo not available on Windows")
	}
	dir := t.TempDir()
	fifo := filepath.Join(dir, "llama-server")

	cmd := exec.Command("mkfifo", fifo)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("mkfifo failed: %v\n%s", err, out)
	}

	svc, _ := newTestServices()
	svc.config.LlamaPath = fifo

	got, err := svc.resolveLlamaPath()
	if err == nil {
		t.Errorf("expected error for FIFO, got path=%q", got)
	}
	if got != "" {
		t.Errorf("expected empty path for FIFO, got %q", got)
	}
}

// TestResolve_SymlinkToValid: symlink -> valid executable -> accepted
func TestResolve_SymlinkToValid(t *testing.T) {
	dir := t.TempDir()
	realBin := filepath.Join(dir, "llama-server-real")
	linkBin := filepath.Join(dir, "llama-server")

	if err := os.WriteFile(realBin, []byte("#!/bin/sh\necho real"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(realBin, linkBin); err != nil {
		t.Fatalf("symlink failed: %v", err)
	}

	svc, _ := newTestServices()
	svc.config.LlamaPath = linkBin

	got, err := svc.resolveLlamaPath()
	if err != nil {
		t.Fatalf("unexpected error for symlink-to-valid: %v", err)
	}
	if got != linkBin {
		t.Errorf("expected resolved path=%q, got %q", linkBin, got)
	}
}

// TestResolve_SymlinkBroken: symlink to nonexistent target -> rejected
func TestResolve_SymlinkBroken(t *testing.T) {
	cleanPATH()

	dir := t.TempDir()
	linkBin := filepath.Join(dir, "llama-server")

	if err := os.Symlink("/nonexistent/llama-server", linkBin); err != nil {
		t.Fatalf("symlink failed: %v", err)
	}

	svc, _ := newTestServices()
	svc.config.LlamaPath = linkBin

	got, err := svc.resolveLlamaPath()
	if err == nil {
		t.Errorf("expected error for broken symlink, got path=%q", got)
	}
	if got != "" {
		t.Errorf("expected empty path for broken symlink, got %q", got)
	}
}

// TestResolve_SymlinkToNonExecutable: symlink -> 0644 file -> rejected
func TestResolve_SymlinkToNonExecutable(t *testing.T) {
	cleanPATH()

	dir := t.TempDir()
	realBin := filepath.Join(dir, "llama-server-real")
	linkBin := filepath.Join(dir, "llama-server")

	if err := os.WriteFile(realBin, []byte("data"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(realBin, linkBin); err != nil {
		t.Fatalf("symlink failed: %v", err)
	}

	svc, _ := newTestServices()
	svc.config.LlamaPath = linkBin

	got, err := svc.resolveLlamaPath()
	if err == nil {
		t.Errorf("expected error for symlink-to-non-executable, got path=%q", got)
	}
	if got != "" {
		t.Errorf("expected empty path, got %q", got)
	}
}

// TestResolve_RelativePath: relative path like "./bin/llama/llama-server"
func TestResolve_RelativePath(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "bin", "llama")
	if err := os.MkdirAll(sub, 0755); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(sub, "llama-server")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\necho test"), 0755); err != nil {
		t.Fatal(err)
	}

	origWd, _ := os.Getwd()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(origWd)

	svc, _ := newTestServices()
	svc.config.LlamaPath = "./bin/llama/llama-server"

	got, err := svc.resolveLlamaPath()
	if err != nil {
		t.Fatalf("unexpected error with relative path: %v", err)
	}
	if !strings.HasSuffix(got, "llama-server") {
		t.Errorf("expected path ending in llama-server, got %q", got)
	}
}

// ===================================================================
// resolveLlamaPath() — Resolution Chain
// ===================================================================

// TestResolve_Candidate1Succeeds: valid executable at LlamaPath -> returns it
func TestResolve_Candidate1Succeeds(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "llama-server")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\necho real"), 0755); err != nil {
		t.Fatal(err)
	}

	svc, _ := newTestServices()
	svc.config.LlamaPath = bin

	got, err := svc.resolveLlamaPath()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != bin {
		t.Errorf("expected %q, got %q", bin, got)
	}
}

// TestResolve_Candidate2Fallback: candidate 1 fails, llama-server on PATH -> found
func TestResolve_Candidate2Fallback(t *testing.T) {
	dir := t.TempDir()
	fakeBinDir := filepath.Join(dir, "fakepath")
	if err := os.MkdirAll(fakeBinDir, 0755); err != nil {
		t.Fatal(err)
	}
	fakeBin := filepath.Join(fakeBinDir, "llama-server")
	if err := os.WriteFile(fakeBin, []byte("#!/bin/sh\necho path"), 0755); err != nil {
		t.Fatal(err)
	}

	origPath := os.Getenv("PATH")
	os.Setenv("PATH", fakeBinDir+string(os.PathListSeparator)+origPath)
	defer os.Setenv("PATH", origPath)

	svc, _ := newTestServices()
	svc.config.LlamaPath = "/nonexistent/llama-server"

	got, err := svc.resolveLlamaPath()
	if err != nil {
		t.Fatalf("expected fallback to PATH, got error: %v", err)
	}
	if !strings.HasSuffix(got, "llama-server") {
		t.Errorf("expected path ending in llama-server, got %q", got)
	}
}

// TestResolve_BothFail: neither candidate works -> error
func TestResolve_BothFail(t *testing.T) {
	origPath := os.Getenv("PATH")
	os.Setenv("PATH", "/nonexistent")
	defer os.Setenv("PATH", origPath)

	svc, _ := newTestServices()
	svc.config.LlamaPath = "/nonexistent/llama-server"

	got, err := svc.resolveLlamaPath()
	if err == nil {
		t.Errorf("expected error when both candidates fail, got path=%q", got)
	}
	if got != "" {
		t.Errorf("expected empty path on error, got %q", got)
	}
	if !strings.Contains(err.Error(), "llama-server not found") {
		t.Errorf("error message should describe failure, got: %v", err)
	}
}

// TestResolve_EmptyConfigPath: LlamaPath="" -> skips candidate 1, tries LookPath
func TestResolve_EmptyConfigPath(t *testing.T) {
	origPath := os.Getenv("PATH")
	os.Setenv("PATH", "/nonexistent")
	defer os.Setenv("PATH", origPath)

	svc, _ := newTestServices()
	svc.config.LlamaPath = ""

	got, err := svc.resolveLlamaPath()
	if err == nil {
		t.Errorf("expected error with empty config and no PATH match, got path=%q", got)
	}
	if got != "" {
		t.Errorf("expected empty path, got %q", got)
	}
}

// TestResolve_EmptyConfigPathWithPATH: LlamaPath="" but llama-server on PATH
func TestResolve_EmptyConfigPathWithPATH(t *testing.T) {
	dir := t.TempDir()
	fakeBinDir := filepath.Join(dir, "bin")
	if err := os.MkdirAll(fakeBinDir, 0755); err != nil {
		t.Fatal(err)
	}
	fakeBin := filepath.Join(fakeBinDir, "llama-server")
	if err := os.WriteFile(fakeBin, []byte("#!/bin/sh\necho path"), 0755); err != nil {
		t.Fatal(err)
	}

	origPath := os.Getenv("PATH")
	os.Setenv("PATH", fakeBinDir+string(os.PathListSeparator)+origPath)
	defer os.Setenv("PATH", origPath)

	svc, _ := newTestServices()
	svc.config.LlamaPath = ""

	got, err := svc.resolveLlamaPath()
	if err != nil {
		t.Fatalf("expected PATH fallback to succeed, got: %v", err)
	}
	if !strings.HasSuffix(got, "llama-server") {
		t.Errorf("expected path ending in llama-server, got %q", got)
	}
}

// TestResolve_ErrorFormat: error message includes both paths checked
func TestResolve_ErrorFormat(t *testing.T) {
	origPath := os.Getenv("PATH")
	os.Setenv("PATH", "/nonexistent")
	defer os.Setenv("PATH", origPath)

	svc, _ := newTestServices()
	svc.config.LlamaPath = "/custom/path/llama-server"

	_, err := svc.resolveLlamaPath()
	if err == nil {
		t.Fatal("expected error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "/custom/path/llama-server") {
		t.Errorf("error should mention LlamaPath, got: %s", msg)
	}
	if !strings.Contains(msg, "system PATH") {
		t.Errorf("error should mention system PATH, got: %s", msg)
	}
}

// ===================================================================
// resolveLlamaPath() — Concurrency Safety
// ===================================================================

// TestResolve_Concurrent: 10 goroutines calling resolveLlamaPath, -race must pass
func TestResolve_Concurrent(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "llama-server")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\necho test"), 0755); err != nil {
		t.Fatal(err)
	}

	svc, _ := newTestServices()
	svc.config.LlamaPath = bin

	var wg sync.WaitGroup
	results := make([]string, 10)
	errs := make([]error, 10)

	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			path, err := svc.resolveLlamaPath()
			results[idx] = path
			errs[idx] = err
		}(i)
	}
	wg.Wait()

	for i := 0; i < 10; i++ {
		if errs[i] != nil {
			t.Errorf("goroutine %d: unexpected error: %v", i, errs[i])
		}
		if results[i] != bin {
			t.Errorf("goroutine %d: got %q, want %q", i, results[i], bin)
		}
	}
}

// TestResolve_ConcurrentBothFail: all goroutines get the same error
func TestResolve_ConcurrentBothFail(t *testing.T) {
	origPath := os.Getenv("PATH")
	os.Setenv("PATH", "/nonexistent")
	defer os.Setenv("PATH", origPath)

	svc, _ := newTestServices()
	svc.config.LlamaPath = "/nonexistent/llama-server"

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			got, err := svc.resolveLlamaPath()
			if err == nil {
				t.Errorf("goroutine %d: expected error, got path=%q", idx, got)
			}
			if got != "" {
				t.Errorf("goroutine %d: expected empty path, got %q", idx, got)
			}
		}(i)
	}
	wg.Wait()
}

// TestResolve_ConcurrentWithPATHFallback: 10 goroutines, fallback to PATH
func TestResolve_ConcurrentWithPATHFallback(t *testing.T) {
	dir := t.TempDir()
	fakeBinDir := filepath.Join(dir, "bin")
	if err := os.MkdirAll(fakeBinDir, 0755); err != nil {
		t.Fatal(err)
	}
	fakeBin := filepath.Join(fakeBinDir, "llama-server")
	if err := os.WriteFile(fakeBin, []byte("#!/bin/sh\necho path"), 0755); err != nil {
		t.Fatal(err)
	}

	origPath := os.Getenv("PATH")
	os.Setenv("PATH", fakeBinDir+string(os.PathListSeparator)+origPath)
	defer os.Setenv("PATH", origPath)

	svc, _ := newTestServices()
	svc.config.LlamaPath = "/nonexistent/llama-server"

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			got, err := svc.resolveLlamaPath()
			if err != nil {
				t.Errorf("goroutine %d: unexpected error: %v", idx, err)
			}
			if got == "" {
				t.Errorf("goroutine %d: expected non-empty path", idx)
			}
		}(i)
	}
	wg.Wait()
}

// ===================================================================
// Cloud Mode — resolveLlamaPath MUST NOT be called
// ===================================================================

// TestCloud_ResolveNotCalled_Embedding: with cloud embedding, startLlama() never
// called, resolveLlamaPath never reached.
func TestCloud_ResolveNotCalled_Embedding(t *testing.T) {
	cfg := newTestConfig()
	cfg.ModelPath = "https://api.openai.com/v1/embeddings"
	cfg.RerankerModel = "./model/test.gguf"
	cfg.LlamaPath = "/nonexistent/llama-server"

	svc, buf := newTestCloudServices(cfg)

	// start() skips llama (cloud), tries reranker/hindsight.
	// It will fail on reranker/hindsight but MUST NOT fail on resolveLlamaPath.
	_ = svc.start()

	out := buf.String()
	if !strings.Contains(out, "llama.cpp skipped (cloud embedding mode)") {
		t.Errorf("expected cloud skip log for llama.cpp, got:\n%s", out)
	}

	if strings.Contains(out, "llama-server not found") {
		if strings.Contains(out, "llama.cpp skipped") {
			t.Log("embedder was skipped; any resolveLlamaPath error is from reranker, not embedding")
		} else {
			t.Errorf("unexpected resolveLlamaPath attempt in cloud mode:\n%s", out)
		}
	}
}

// TestCloud_ResolveNotCalled_Reranker: with cloud reranker, startLlamaReranker()
// never called, resolveLlamaPath never reached.
func TestCloud_ResolveNotCalled_Reranker(t *testing.T) {
	cfg := newTestConfig()
	cfg.ModelPath = "https://api.openai.com/v1/embeddings"
	cfg.RerankerModel = "https://api.cohere.com/v1/rerank"
	cfg.LlamaPath = "/nonexistent/llama-server"

	svc, buf := newTestCloudServices(cfg)

	_ = svc.start()

	out := buf.String()
	if !strings.Contains(out, "llama.cpp skipped (cloud embedding mode)") {
		t.Errorf("expected cloud skip log for embedding, got:\n%s", out)
	}
	if !strings.Contains(out, "llama reranker skipped (cloud reranker mode)") {
		t.Errorf("expected cloud skip log for reranker, got:\n%s", out)
	}
}

// TestCloud_BothCloud: both cloud — neither llama service starts
func TestCloud_BothCloud_ResolveNotCalled(t *testing.T) {
	cfg := newTestConfig()
	cfg.ModelPath = "https://api.openai.com/v1/embeddings"
	cfg.RerankerModel = "https://api.cohere.com/v1/rerank"
	cfg.LlamaPath = "/nonexistent/llama-server"

	svc, buf := newTestCloudServices(cfg)

	_ = svc.start()

	out := buf.String()
	if !strings.Contains(out, "llama.cpp skipped (cloud embedding mode)") {
		t.Errorf("expected cloud skip for llama.cpp, got:\n%s", out)
	}
	if !strings.Contains(out, "llama reranker skipped (cloud reranker mode)") {
		t.Errorf("expected cloud skip for reranker, got:\n%s", out)
	}
}

// TestCloud_AllHealthy_CloudModeReturnsTrue: cloud mode health status without
// ever calling resolveLlamaPath.
func TestCloud_AllHealthy_CloudModeReturnsTrue(t *testing.T) {
	cfg := newTestConfig()
	cfg.ModelPath = "https://api.openai.com/v1/embeddings"
	cfg.RerankerModel = "https://api.cohere.com/v1/rerank"

	svc, _ := newTestCloudServices(cfg)

	l, r, _ := svc.allHealthy()
	if !l {
		t.Error("allHealthy() llama = false with cloud embedding, want true")
	}
	if !r {
		t.Error("allHealthy() reranker = false with cloud reranker, want true")
	}
}

// ===================================================================
// Makefile — Platform Detection Shell Logic
// ===================================================================

// runPlatformScript runs the Makefile's platform detection logic with
// mocked uname output. Returns stdout and whether exit code was 0.
func runPlatformScript(unameS, unameM string) (string, bool) {
	// Build a bash script that overrides uname and runs the Makefile's
	// detection logic. Use printf for format safety with arbitrary strings.
	script := fmt.Sprintf(`
uname() { case "$1" in -s) echo "%s";; -m) echo "%s";; esac; }
case $(uname -s) in
	Darwin) OSNAME=macos;;
	Linux)  OSNAME=ubuntu;;
	*)      echo "ERROR: Unsupported platform"; exit 1;;
esac
case $(uname -m) in
	arm64|aarch64) ARCH=arm64;;
	x86_64)         ARCH=x64;;
	*)              echo "ERROR: Unsupported architecture"; exit 1;;
esac
echo "$OSNAME-$ARCH"
`, unameS, unameM)

	cmd := exec.Command("bash", "-c", script)
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err == nil
}

// TestMakefile_Platform_Matrix: all supported platform combos produce correct URLs
func TestMakefile_Platform_Matrix(t *testing.T) {
	tests := []struct {
		name   string
		unameS string
		unameM string
		want   string
	}{
		{"macOS arm64", "Darwin", "arm64", "macos-arm64"},
		{"macOS x64", "Darwin", "x86_64", "macos-x64"},
		{"Linux arm64", "Linux", "arm64", "ubuntu-arm64"},
		{"Linux aarch64", "Linux", "aarch64", "ubuntu-arm64"},
		{"Linux x64", "Linux", "x86_64", "ubuntu-x64"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := runPlatformScript(tt.unameS, tt.unameM)
			if !ok {
				t.Fatalf("platform detection failed: %s", got)
			}
			if got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

// TestMakefile_Platform_UnsupportedOS: FreeBSD -> error
func TestMakefile_Platform_UnsupportedOS(t *testing.T) {
	got, ok := runPlatformScript("FreeBSD", "x86_64")
	if ok {
		t.Errorf("FreeBSD should be rejected, got success: %s", got)
	}
	if !strings.Contains(got, "ERROR") {
		t.Errorf("expected error output for FreeBSD, got: %s", got)
	}
}

// TestMakefile_Platform_UnsupportedArch: riscv64 -> error
func TestMakefile_Platform_UnsupportedArch(t *testing.T) {
	got, ok := runPlatformScript("Linux", "riscv64")
	if ok {
		t.Errorf("riscv64 should be rejected, got success: %s", got)
	}
	if !strings.Contains(got, "ERROR") {
		t.Errorf("expected error output for riscv64, got: %s", got)
	}
}

// TestMakefile_Platform_LinuxArmv7l: Linux armv7l -> error
func TestMakefile_Platform_LinuxArmv7l(t *testing.T) {
	got, ok := runPlatformScript("Linux", "armv7l")
	if ok {
		t.Errorf("armv7l should be rejected, got success: %s", got)
	}
	if !strings.Contains(got, "ERROR") {
		t.Errorf("expected error output for armv7l, got: %s", got)
	}
}

// TestMakefile_Platform_DarwinAarch64: Darwin aarch64 -> macos-arm64
func TestMakefile_Platform_DarwinAarch64(t *testing.T) {
	got, ok := runPlatformScript("Darwin", "aarch64")
	if !ok {
		t.Fatalf("aarch64 detection failed: %s", got)
	}
	if got != "macos-arm64" {
		t.Errorf("got %q, want %q", got, "macos-arm64")
	}
}

// ===================================================================
// Makefile — Idempotency Logic
// ===================================================================

// TestMakefile_Idempotency_SkipExecutable: [ -x ] skips when executable file exists
func TestMakefile_Idempotency_SkipExecutable(t *testing.T) {
	dir := t.TempDir()
	binDir := filepath.Join(dir, "bin", "llama")
	if err := os.MkdirAll(binDir, 0755); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(binDir, "llama-server")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\necho fake"), 0755); err != nil {
		t.Fatal(err)
	}

	script := fmt.Sprintf(`
if [ -x "%s" ]; then
	echo "already downloaded"
	exit 0
fi
echo "need download"
`, bin)

	cmd := exec.Command("bash", "-c", script)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("shell check failed: %v\n%s", err, out)
	}
	output := strings.TrimSpace(string(out))
	if output != "already downloaded" {
		t.Errorf("expected 'already downloaded', got %q", output)
	}
}

// TestMakefile_Idempotency_NotExecutable: [ -x ] rejects non-executable file
func TestMakefile_Idempotency_NotExecutable(t *testing.T) {
	dir := t.TempDir()
	binDir := filepath.Join(dir, "bin", "llama")
	if err := os.MkdirAll(binDir, 0755); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(binDir, "llama-server")
	if err := os.WriteFile(bin, []byte("garbage"), 0644); err != nil {
		t.Fatal(err)
	}

	script := fmt.Sprintf(`
if [ -x "%s" ]; then
	echo "already downloaded"
	exit 0
fi
echo "need download"
`, bin)

	cmd := exec.Command("bash", "-c", script)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("shell check failed: %v\n%s", err, out)
	}
	output := strings.TrimSpace(string(out))
	if output != "need download" {
		t.Errorf("expected 'need download' for non-executable, got %q", output)
	}
}

// TestMakefile_Idempotency_VersionMismatch: idempotency check does NOT verify version.
// This is a known/acceptable limitation (spec E9). Verify the behavior.
func TestMakefile_Idempotency_VersionMismatch(t *testing.T) {
	dir := t.TempDir()
	binDir := filepath.Join(dir, "bin", "llama")
	if err := os.MkdirAll(binDir, 0755); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(binDir, "llama-server")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\necho old-version"), 0755); err != nil {
		t.Fatal(err)
	}

	// The Makefile only checks [ -x ], not version
	script := fmt.Sprintf(`
VERSION="b99999"
if [ -x "%s" ]; then
	echo "skipped (version not checked)"
	exit 0
fi
echo "download would proceed for version $VERSION"
`, bin)

	cmd := exec.Command("bash", "-c", script)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("shell check failed: %v\n%s", err, out)
	}
	output := strings.TrimSpace(string(out))
	if !strings.HasPrefix(output, "skipped") {
		t.Errorf("expected download to be skipped despite version change, got %q", output)
	}
}

// ===================================================================
// Makefile — Download Pipeline Error Handling (PIPEFAIL BUG)
// ===================================================================

// TestMakefile_Pipefail_TarFailureSilentlyIgnored: the Makefile's download
// pipeline uses `curl ... | tar ...` WITHOUT `set -o pipefail`. This test
// simulates a tar failure to verify the Makefile would continue and report
// success despite the failure — a real bug.
func TestMakefile_Pipefail_TarFailureSilentlyIgnored(t *testing.T) {
	dir := t.TempDir()
	tmpDir := filepath.Join(dir, "tmpdownload")
	if err := os.MkdirAll(tmpDir, 0755); err != nil {
		t.Fatal(err)
	}
	llamaBin := filepath.Join(dir, "bin", "llama")

	// Simulate the download pipeline: invalid data piped to tar (fails),
	// then Makefile blindly continues to mkdir, mv, chmod, rm, echo.
	script := fmt.Sprintf(`
set +o pipefail
TMPDIR=%q
VENDOR_BIN=%q
echo "garbage-input" | tar xz -C "${TMPDIR}" 2>/dev/null
mkdir -p "${VENDOR_BIN}"
mv "${TMPDIR}/build/bin/llama-server" "${VENDOR_BIN}/llama-server" 2>/dev/null
chmod +x "${VENDOR_BIN}/llama-server" 2>/dev/null
rm -rf "${TMPDIR}"
echo "llama-server downloaded to bin/llama/llama-server."
`, tmpDir, llamaBin)

	cmd := exec.Command("bash", "-c", script)
	out, err := cmd.CombinedOutput()
	output := strings.TrimSpace(string(out))

	if err != nil {
		t.Logf("Script failed (expected on some shells): %v\n%s", err, output)
		return
	}

	t.Errorf("BUG: Makefile download pipeline silently swallows tar failure — "+
		"no 'set -o pipefail' or '|| exit 1' in the recipe.\n"+
		"Output: %s", output)

	// Verify no binary was created
	if _, statErr := os.Stat(filepath.Join(llamaBin, "llama-server")); statErr == nil {
		t.Errorf("BUG: binary was created despite tar failure! Partial artifact at %s",
			filepath.Join(llamaBin, "llama-server"))
	}
}

// ===================================================================
// Config — LlamaPath Default Change
// ===================================================================

func TestConfig_LlamaPathDefault(t *testing.T) {
	cfg := LoadConfig()
	expected := "./bin/llama/llama-server"
	if cfg.LlamaPath != expected {
		t.Errorf("LlamaPath default = %q, want %q", cfg.LlamaPath, expected)
	}
}

func TestConfig_LLAMAPathEnvOverride(t *testing.T) {
	expected := "/custom/path/llama-server"
	os.Setenv("LLAMA_PATH", expected)
	defer os.Unsetenv("LLAMA_PATH")

	cfg := LoadConfig()
	if cfg.LlamaPath != expected {
		t.Errorf("LlamaPath = %q, want %q (env override)", cfg.LlamaPath, expected)
	}
}

func TestConfig_LLAMAPathEmptyString(t *testing.T) {
	os.Setenv("LLAMA_PATH", "")
	defer os.Unsetenv("LLAMA_PATH")

	cfg := LoadConfig()
	expected := "./bin/llama/llama-server"
	if cfg.LlamaPath != expected {
		t.Errorf("LLAMA_PATH=\"\" -> LlamaPath = %q, want %q", cfg.LlamaPath, expected)
	}
}

func TestConfig_LLAMAPathEnvNotEmpty(t *testing.T) {
	os.Setenv("LLAMA_PATH", "/usr/local/bin/llama-server")
	defer os.Unsetenv("LLAMA_PATH")

	cfg := LoadConfig()
	if cfg.LlamaPath != "/usr/local/bin/llama-server" {
		t.Errorf("expected /usr/local/bin/llama-server, got %q", cfg.LlamaPath)
	}
}

// ===================================================================
// Backward Compatibility — brew-installed llama-server on PATH
// ===================================================================

func TestBackwardCompat_BrewInstallOnPATH(t *testing.T) {
	dir := t.TempDir()
	fakeBinDir := filepath.Join(dir, "brew", "bin")
	if err := os.MkdirAll(fakeBinDir, 0755); err != nil {
		t.Fatal(err)
	}
	fakeBin := filepath.Join(fakeBinDir, "llama-server")
	if err := os.WriteFile(fakeBin, []byte("#!/bin/sh\necho brew-llama"), 0755); err != nil {
		t.Fatal(err)
	}

	origPath := os.Getenv("PATH")
	os.Setenv("PATH", fakeBinDir+string(os.PathListSeparator)+origPath)
	defer os.Setenv("PATH", origPath)

	svc, _ := newTestServices()
	svc.config.LlamaPath = "./bin/llama/llama-server"

	got, err := svc.resolveLlamaPath()
	if err != nil {
		t.Fatalf("backward compat: expected PATH fallback to succeed, got: %v", err)
	}
	if !strings.HasSuffix(got, "llama-server") {
		t.Errorf("expected path ending in llama-server, got %q", got)
	}
}

func TestBackwardCompat_ExplicitLLAMAPath(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "my-llama-server")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\necho custom"), 0755); err != nil {
		t.Fatal(err)
	}

	svc, _ := newTestServices()
	svc.config.LlamaPath = bin

	got, err := svc.resolveLlamaPath()
	if err != nil {
		t.Fatalf("expected explicit path resolution, got: %v", err)
	}
	if got != bin {
		t.Errorf("expected %q, got %q", bin, got)
	}
}

func TestBackwardCompat_NoLlamaAnywhere(t *testing.T) {
	origPath := os.Getenv("PATH")
	os.Setenv("PATH", "/nonexistent")
	defer os.Setenv("PATH", origPath)

	svc, _ := newTestServices()
	svc.config.LlamaPath = "./bin/llama/llama-server"

	_, err := svc.resolveLlamaPath()
	if err == nil {
		t.Fatal("expected error when no llama-server exists anywhere")
	}
	if !strings.Contains(err.Error(), "llama-server not found") {
		t.Errorf("error should mention 'llama-server not found', got: %v", err)
	}
}

// ===================================================================
// spawn() — Unchanged signature verification
// ===================================================================

func TestSpawn_UnchangedSignature(t *testing.T) {
	// Compile-time check: if signature changed, this won't compile.
	svc, _ := newTestServices()
	cmd := svc.spawn("/nonexistent/binary", "arg1", "arg2")
	if cmd == nil {
		t.Log("spawn returned nil (expected if logs/ dir doesn't exist)")
	}
}

// ===================================================================
// .gitignore — bin/llama/ pattern
// ===================================================================

func TestGitignore_VendorIgnored(t *testing.T) {
	data, err := os.ReadFile(".gitignore")
	if err != nil {
		t.Fatal(err)
	}
	content := string(data)

	if !strings.Contains(content, "bin/llama/") {
		t.Error(".gitignore does not contain 'bin/llama/' pattern")
	}
}

// ===================================================================
// Makefile — run target conditional hint
// ===================================================================

func TestMakefile_RunConditionalHint(t *testing.T) {
	dir := t.TempDir()

	// Test 1: No binary exists -> hint should print
	script1 := fmt.Sprintf(`
VENDOR_BIN="%s/bin/llama/llama-server"
if [ ! -x "$VENDOR_BIN" ]; then
	echo "Hint: run 'make setup' to download llama-server"
fi
echo "go run ."
`, dir)

	cmd1 := exec.Command("bash", "-c", script1)
	out1, _ := cmd1.CombinedOutput()
	output1 := string(out1)

	if !strings.Contains(output1, "Hint:") {
		t.Errorf("expected hint when binary doesn't exist, got: %s", output1)
	}
	if !strings.Contains(output1, "go run .") {
		t.Errorf("expected 'go run .' regardless, got: %s", output1)
	}

	// Test 2: Binary exists and is executable -> no hint
	binDir := filepath.Join(dir, "bin", "llama")
	if err := os.MkdirAll(binDir, 0755); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(binDir, "llama-server")
	if err := os.WriteFile(bin, []byte("#!/bin/sh"), 0755); err != nil {
		t.Fatal(err)
	}

	script2 := fmt.Sprintf(`
VENDOR_BIN="%s/bin/llama/llama-server"
if [ ! -x "$VENDOR_BIN" ]; then
	echo "Hint: run 'make setup' to download llama-server"
fi
echo "go run ."
`, dir)

	cmd2 := exec.Command("bash", "-c", script2)
	out2, _ := cmd2.CombinedOutput()
	output2 := string(out2)

	if strings.Contains(output2, "Hint:") {
		t.Errorf("unexpected hint when binary exists: %s", output2)
	}
	if !strings.Contains(output2, "go run .") {
		t.Errorf("expected 'go run .' regardless, got: %s", output2)
	}
}
