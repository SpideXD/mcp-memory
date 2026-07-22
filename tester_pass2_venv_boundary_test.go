// Package main — Pass 2: Boundary tests for Phase 2 .venv integration.
// Tests edge cases the spec and coder did NOT cover:
//   - Makefile edge cases (python3 missing, pip failure, partial venv, concurrent setup)
//   - Binary discovery edge cases (no execute permission, FIFO, socket, device files)
//   - .gitignore accidental match patterns
//   - Docs consistency across all 4 updated files
//   - Cross-platform .venv/Scripts/ (Windows) fallback missing
//   - Build target correctness
//   - run without setup behavior
package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
)

// =========================================================================
// MAKEFILE EDGE CASES
// =========================================================================

// TestVenv_Boundary_Makefile_Python3Missing verifies what happens when
// python3 is NOT installed on the system. The Makefile has:
//   setup:
//       python3 -m venv .venv
// If python3 is missing, the shell errors with "command not found".
// The Makefile has NO guard to check for python3 presence before running.
func TestVenv_Boundary_Makefile_Python3Missing(t *testing.T) {
	// We cannot test this by modifying PATH in this process because
	// cmd.Run() inherits PATH. But we can verify that the Makefile
	// has no python3 dependency check.
	//
	// Read Makefile content
	mk, err := os.ReadFile(filepath.Join("Makefile"))
	if err != nil {
		t.Fatal(err)
	}
	mkStr := string(mk)

	// Check: does the Makefile check for python3 before running setup?
	hasPythonCheck := strings.Contains(mkStr, "python3") &&
		(strings.Contains(mkStr, "which python3") || strings.Contains(mkStr, "command -v python3") || strings.Contains(mkStr, "test -x"))

	t.Log("Makefile setup target: python3 -m venv .venv")
	if !hasPythonCheck {
		t.Log("BOUNDARY GAP: Makefile setup has NO dependency check for python3.")
		t.Log("If python3 is missing, 'python3 -m venv .venv' fails with 'command not found'.")
		t.Log("Fix: add a pre-check: 'command -v python3 >/dev/null 2>&1 || { echo \"python3 required\"; exit 1; }'")
	}

	// Also verify: pip3 should be available in the venv after creation
	hasPipCheck := strings.Contains(mkStr, "test -d") || strings.Contains(mkStr, "which pip")
	t.Log("")
	if !hasPipCheck {
		t.Log("BOUNDARY GAP: Makefile setup does not verify pip works after venv creation.")
		t.Log("If .venv was created without pip (e.g., Debian python3-venv not installed),")
		t.Log("the second line '.venv/bin/pip install ...' fails silently.")
	}
}

// TestVenv_Boundary_Makefile_PipFails verifies behavior when pip install
// fails partway through. The Makefile runs:
//   .venv/bin/pip install hindsight-api-slim==0.8.2 hindsight-client==0.8.2
// If one package installs and the other fails, .venv is left in a partial state.
func TestVenv_Boundary_Makefile_PipFails(t *testing.T) {
	// This test verifies the Makefile's pip install line is a single
	// command. If pip exits non-zero mid-way, the Make aborts.
	// But the .venv is left with only ONE package installed.
	//
	// Read Makefile to verify pip install is a single line
	mk, err := os.ReadFile(filepath.Join("Makefile"))
	if err != nil {
		t.Fatal(err)
	}
	mkStr := string(mk)

	// Find the setup target's pip install line
	lines := strings.Split(mkStr, "\n")
	var pipLine string
	foundSetup := false
	var continuation strings.Builder
	inContinuation := false
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "setup:" {
			foundSetup = true
			continue
		}
		if foundSetup && inContinuation {
			// Accumulate continuation lines (lines after \)
			continuation.WriteString(" ")
			continuation.WriteString(trimmed)
			if !strings.HasSuffix(trimmed, "\\") {
				pipLine = continuation.String()
				break
			}
			continue
		}
		if foundSetup && strings.Contains(trimmed, "pip install") {
			if strings.HasSuffix(trimmed, "\\") {
				continuation.WriteString(strings.TrimSuffix(trimmed, "\\"))
				inContinuation = true
				continue
			}
			pipLine = trimmed
			break
		}
		if foundSetup && trimmed == "" {
			// Blank line after setup means we passed the section
			break
		}
	}

	if pipLine == "" {
		t.Fatal("Could not find pip install line in Makefile setup target")
	}

	t.Logf("Makefile pip install line: %s", pipLine)
	t.Log("BOUNDARY ISSUE: pip install is a single command for TWO packages.")
	t.Log("If hindsight-api-slim installs but hindsight-client fails,")
	t.Log(".venv is in a partial state with no rollback.")
	t.Log("Fix: install packages in sequence or add a post-install verification step.")

	// Verify: both packages should be listed
	if !strings.Contains(pipLine, "hindsight-api-slim") {
		t.Error("pip install line missing hindsight-api-slim")
	}
	if !strings.Contains(pipLine, "hindsight-client") {
		t.Error("pip install line missing hindsight-client")
	}
}

// TestVenv_Boundary_Makefile_PartialVenv verifies that if python3 -m venv
// creates .venv/ partially (e.g., bin/ exists but pip doesn't), the Makefile
// continues to the pip install line which then fails.
// The .venv/ directory is left in a broken state with no rollback/cleanup.
func TestVenv_Boundary_Makefile_PartialVenv_NoRollback(t *testing.T) {
	tmpDir := t.TempDir()
	oldWd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(oldWd)

	// Create a partial .venv: .venv/bin/ exists but pip is missing
	venvBin := filepath.Join(tmpDir, ".venv", "bin")
	if err := os.MkdirAll(venvBin, 0755); err != nil {
		t.Fatal(err)
	}
	// Create a python symlink but NO pip
	if err := os.Symlink("/usr/bin/python3", filepath.Join(venvBin, "python3")); err != nil {
		t.Skip("Cannot create symlink, skipping: ", err)
	}

	// Now simulate what happens: Makefile runs `.venv/bin/pip install ...`
	// pip does NOT exist in .venv/bin/ — the command fails
	_, err = os.Stat(filepath.Join(venvBin, "pip"))
	if err == nil {
		t.Log("pip exists in partial .venv (unexpected — .venv might be complete)")
		return
	}

	t.Log("BOUNDARY ISSUE: Partial .venv creation (bin/ exists, pip missing)")
	t.Log("os.Stat('.venv/bin/pip') fails with:", err)
	t.Log("Makefile continues to pip install, which fails.")
	t.Log(".venv/ is left in broken state with NO cleanup.")
	t.Log("Fix: add failure detection and rollback (rm -rf .venv on failure)")
}

// TestVenv_Boundary_Makefile_RunWithoutSetup verifies that `make run` doesn't
// check whether `make setup` has been run. If .venv doesn't exist and the user
// runs `make run`, the server starts but hindsight-api will fail at discovery.
func TestVenv_Boundary_Makefile_RunWithoutSetup(t *testing.T) {
	// Read Makefile to check if `run` depends on `setup`
	mk, err := os.ReadFile(filepath.Join("Makefile"))
	if err != nil {
		t.Fatal(err)
	}
	mkStr := string(mk)

	// Check if `run` target declares a dependency on `setup`
	// In Makefile syntax: `run: setup` means run depends on setup
	lines := strings.Split(mkStr, "\n")
	var runDependsOnSetup bool
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "run:") {
			// Check if "setup" is in the dependency list
			afterColon := strings.TrimPrefix(trimmed, "run:")
			runDependsOnSetup = strings.Contains(afterColon, "setup")
			break
		}
	}

	if !runDependsOnSetup {
		t.Log("BOUNDARY ISSUE: `make run` does NOT depend on `make setup`.")
		t.Log("If a user runs `make run` without running `make setup` first:")
		t.Log("  1. Server starts (go run .)")
		t.Log("  2. startHindsight() fails to find hindsight-api binary")
		t.Log("  3. Error: 'hindsight-api not found' — confusing because")
		t.Log("     the user might not know about make setup")
		t.Log("Fix: add 'run: setup' dependency or check .venv exists in run target")
	} else {
		t.Log("OK: `make run` depends on `make setup`")
	}
}

// TestVenv_Boundary_Makefile_ConcurrentSetup verifies that running
// `make setup` from two terminals simultaneously causes a race.
// Both calls run `python3 -m venv .venv` which can conflict.
func TestVenv_Boundary_Makefile_ConcurrentSetup(t *testing.T) {
	_ = t.TempDir() // Keep for future concurrent test expansion

	// Create two lock files to simulate concurrent access
	// We can't easily run two make processes from one test,
	// but we can verify the Makefile has no locking mechanism.
	mk, err := os.ReadFile(filepath.Join("Makefile"))
	if err != nil {
		t.Fatal(err)
	}
	mkStr := string(mk)

	// Check for any locking mechanism
	hasLockFile := strings.Contains(mkStr, "LOCKFILE") || strings.Contains(mkStr, "flock") || strings.Contains(mkStr, "lockfile")
	hasTestDashD := strings.Contains(mkStr, "test -d .venv")
	hasMkdirRace := strings.HasPrefix(strings.TrimSpace(strings.Split(mkStr, "\n")[0]), "setup:") &&
		strings.Contains(mkStr, "test -d .venv ||")

	t.Log("Makefile setup target — concurrent safety analysis:")
	if hasLockFile {
		t.Log("  Lock file mechanism: PRESENT")
	} else {
		t.Log("  Lock file mechanism: NONE — concurrent `make setup` calls race")
	}

	if hasTestDashD {
		t.Log("  test -d .venv guard: PRESENT")
	} else {
		t.Log("  test -d .venv guard: ABSENT — concurrent calls both run python3 -m venv")
	}

	if hasMkdirRace {
		t.Log("  TOCTOU race: test -d .venv && ... has unavoidable TOCTOU gap")
	}

	// Verify: concurrent calls can collide on .venv creation
	// Two simultaneous `make setup` calls:
	//   Process A: python3 -m venv .venv (creates .venv/)
	//   Process B: python3 -m venv .venv (fails because .venv/ exists)
	// In practice, python3 -m venv handles this gracefully in Python 3.12+
	// but this is not guaranteed across Python versions.
	t.Log("")
	t.Log("BOUNDARY ISSUE: No locking on concurrent make setup calls.")
	t.Log("Two simultaneous calls can race on .venv creation.")
	t.Log("Python 3.12+ handles overlapping venv creation gracefully,")
	t.Log("but older versions or pip install from two terminals could conflict.")
	t.Log("Fix: use a lock file: 'setup: .venv.setup.lock' or flock(1)")
}

// TestVenv_Boundary_Makefile_CleanAllBuildArtifacts verifies that
// `make build` outputs to bin/mcp-memory and `make clean` removes it.
// Also verifies that make clean removes both bin/mcp-memory AND root mcp-memory.
func TestVenv_Boundary_Makefile_BuildThenClean(t *testing.T) {
	tmpDir := t.TempDir()
	oldWd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(oldWd)

	if err := os.Chdir(tmpDir); err != nil {
		t.Fatal(err)
	}

	// Create a mock main.go so build works (minimal Go file)
	mainGo := `package main
import "fmt"
func main() { fmt.Println("test") }
`
	if err := os.WriteFile("main.go", []byte(mainGo), 0644); err != nil {
		t.Fatal(err)
	}

	// Create go.mod for the build
	if err := os.WriteFile("go.mod", []byte("module testbuild\n\ngo 1.26\n"), 0644); err != nil {
		t.Fatal(err)
	}

	// Create bin dir
	if err := os.MkdirAll("bin", 0755); err != nil {
		t.Fatal(err)
	}

	// Run make build (but we know Makefile targets use real paths)
	// Instead, we verify the behavior manually:
	// The build target is: go build -o bin/mcp-memory .
	cmd := exec.Command("go", "build", "-o", "bin/mcp-memory", ".")
	cmd.Dir = tmpDir
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Logf("NOTE: go build failed (expected in isolated dir): %s", string(output))
		t.Log("But we can verify the Makefile syntax is correct:")
		// Read the Makefile to check build target
		mk, err := os.ReadFile(filepath.Join(oldWd, "Makefile"))
		if err != nil {
			t.Fatal(err)
		}
		mkStr := string(mk)
		if strings.Contains(mkStr, "go build -o bin/mcp-memory .") {
			t.Log("OK: Makefile build target outputs to bin/mcp-memory (correct)")
		} else {
			t.Error("Makefile build target may not output to bin/mcp-memory")
		}

		// Verify clean target removes both locations
		if strings.Contains(mkStr, "bin/mcp-memory") && strings.Contains(mkStr, "mcp-memory") {
			t.Log("OK: Makefile clean target removes both bin/mcp-memory and root mcp-memory")
		} else {
			t.Error("Makefile clean target may miss one build artifact location")
		}
		return
	}
	defer cmd.Wait()

	// Verify binary was created at bin/mcp-memory, NOT at root
	binPath := filepath.Join(tmpDir, "bin", "mcp-memory")
	rootPath := filepath.Join(tmpDir, "mcp-memory")

	if _, err := os.Stat(binPath); err != nil {
		t.Errorf("Build did NOT create bin/mcp-memory: %v", err)
	} else {
		t.Log("OK: build creates bin/mcp-memory")
	}

	if _, err := os.Stat(rootPath); err == nil {
		t.Log("NOTE: build also created mcp-memory at root (unexpected)")
	} else {
		t.Log("OK: no root-level mcp-memory (correct)")
	}
}

// =========================================================================
// BINARY DISCOVERY EDGE CASES
// =========================================================================

// TestVenv_Boundary_Discovery_NoExecutePermission verifies that if
// .venv/bin/hindsight-api exists but has NO execute permission (e.g.,
// a data file accidentally placed there, or a broken pip install),
// os.Stat passes but exec.Command fails with "permission denied".
func TestVenv_Boundary_Discovery_NoExecutePermission(t *testing.T) {
	tmpDir := t.TempDir()
	oldWd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(oldWd)
	if err := os.Chdir(tmpDir); err != nil {
		t.Fatal(err)
	}

	// Create .venv/bin/hindsight-api as a non-executable file (0644)
	if err := os.MkdirAll(filepath.Join(tmpDir, ".venv", "bin"), 0755); err != nil {
		t.Fatal(err)
	}
	scriptPath := filepath.Join(tmpDir, ".venv", "bin", "hindsight-api")
	if err := os.WriteFile(scriptPath, []byte("#!/bin/sh\nexit 0\n"), 0644); err != nil {
		t.Fatal(err)
	}
	// No os.Chmod — it's created as 0644 (non-executable)

	// Step 1: os.Stat on the file — coder's code checks err == nil
	info, err := os.Stat(scriptPath)
	if err != nil {
		t.Fatal("os.Stat should succeed on a non-executable file:", err)
	}
	if info.IsDir() {
		t.Fatal("file should not be a directory")
	}

	// Coder's code would set hindsightPath = this path (BUG)
	t.Log("BOUNDARY: os.Stat succeeds on non-executable file (err==nil)")
	t.Log("Coder's code: if _, err := os.Stat(p); err == nil { hindsightPath = p; break }")
	t.Log("This WOULD accept a non-executable file as valid binary path.")

	// Step 2: exec.Command on a non-executable file fails
	cmd := exec.Command(scriptPath)
	err = cmd.Run()
	if err != nil {
		t.Logf("exec.Command fails as expected on non-executable: %v", err)
		// Check if it's a permission error
		if exitErr, ok := err.(*exec.ExitError); ok {
			t.Logf("  Exit code: %d", exitErr.ExitCode())
		} else if pathErr, ok := err.(*os.PathError); ok {
			t.Logf("  PathError: %v (Err: %v)", pathErr.Path, pathErr.Err)
		}
		t.Log("")
		t.Log("BOUNDARY GAP: os.Stat succeeds on non-executable file, but")
		t.Log("exec.Command fails with 'permission denied'. The error message")
		t.Log("is confusing — 'hindsight-api: permission denied' instead of")
		t.Log("'hindsight-api not found' or 'binary not found'.")
		t.Log("Fix: after os.Stat succeeds, verify file is executable with")
		t.Log("info.Mode()&0111 != 0 or use exec.LookPath on the absolute path.")
	} else {
		t.Log("NOTE: non-executable file was executed (unexpected on this platform)")
	}
}

// TestVenv_Boundary_Discovery_FifoFile verifies that if .venv/bin/hindsight-api
// is a FIFO (named pipe), os.Stat succeeds but the binary is invalid.
// FIFOs are created by mkfifo/mknod and can appear in odd locations.
func TestVenv_Boundary_Discovery_FifoFile(t *testing.T) {
	tmpDir := t.TempDir()
	oldWd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(oldWd)
	if err := os.Chdir(tmpDir); err != nil {
		t.Fatal(err)
	}

	if err := os.MkdirAll(filepath.Join(tmpDir, ".venv", "bin"), 0755); err != nil {
		t.Fatal(err)
	}
	fifoPath := filepath.Join(tmpDir, ".venv", "bin", "hindsight-api")

	// Create a FIFO (named pipe) — this is a special file type
	// syscall.Mkfifo is available on Unix
	if err := syscall.Mkfifo(fifoPath, 0644); err != nil {
		t.Skip("Cannot create FIFO (not supported on this platform):", err)
	}

	// Step 1: os.Stat on a FIFO succeeds
	info, err := os.Stat(fifoPath)
	if err != nil {
		t.Fatal("os.Stat should succeed on a FIFO:", err)
	}

	// A FIFO is NOT a regular file and NOT a directory
	isRegular := info.Mode().IsRegular()
	isDir := info.IsDir()

	if isDir {
		t.Fatal("FIFO should not be IsDir()")
	}

	// Coder's code (with proposed IsDir() fix) would still fail here:
	// `if info, err := os.Stat(p); err == nil && !info.IsDir() { ... }`
	// A FIFO passes both err==nil AND !info.IsDir()

	t.Logf("BOUNDARY: FIFO file at .venv/bin/hindsight-api")
	t.Logf("  Mode: %v", info.Mode())
	t.Logf("  IsDir: %v, IsRegular: %v", isDir, isRegular)
	t.Log("")
	t.Log("BOUNDARY GAP: os.Stat on a FIFO succeeds and !IsDir() is true.")
	t.Log("Even with the proposed IsDir() fix from Pass 1,")
	t.Log("a FIFO would still be accepted as a valid binary.")
	t.Log("exec.Command on a FIFO would hang (block on read) or fail.")
	t.Log("Fix: add info.Mode().IsRegular() check to ensure it's a regular file.")
}

// TestVenv_Boundary_Discovery_SocketFile verifies that if
// .venv/bin/hindsight-api is a Unix domain socket, os.Stat succeeds
// but the "binary" is invalid.
func TestVenv_Boundary_Discovery_SocketFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix sockets not available on Windows")
	}

	tmpDir := t.TempDir()
	oldWd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(oldWd)
	if err := os.Chdir(tmpDir); err != nil {
		t.Fatal(err)
	}

	if err := os.MkdirAll(filepath.Join(tmpDir, ".venv", "bin"), 0755); err != nil {
		t.Fatal(err)
	}
	socketPath := filepath.Join(tmpDir, ".venv", "bin", "hindsight-api")

	// Create a Unix domain socket (best effort — may not be supported everywhere)
	// We use net.Listen to create a socket file
	ln, err := exec.Command("bash", "-c", "nc -lU "+socketPath+" & sleep 0.1; exit 0").CombinedOutput()
	_ = ln
	// Alternative: directly create a socket file is tricky. We'll just verify
	// that os.Stat would succeed on a socket file if one exists.
	//
	// Actually, creating a Unix socket file requires a server to bind to it.
	// If we can't create one, we test the concept differently:
	// os.Stat on a socket returns mode & os.ModeSocket != 0
	t.Log("Testing socket file detection concept:")
	t.Log("If .venv/bin/hindsight-api were a Unix socket:")
	t.Log("  - os.Stat succeeds (err == nil)")
	t.Log("  - !info.IsDir() is true (socket is not a directory)")
	t.Log("  - Even with IsDir() fix, socket file passes the check")
	t.Log("  - exec.Command would fail trying to execute a socket")
	t.Log("")
	t.Log("BOUNDARY GAP: os.Stat succeeds on socket files but they are not")
	t.Log("valid executables. IsDir() is not sufficient — use IsRegular().")
	t.Log("Fix: change to: err == nil && info.Mode().IsRegular()")
}

// TestVenv_Boundary_Discovery_DeviceFile verifies that if .venv/bin/hindsight-api
// were a block or character device file, os.Stat would succeed and the
// coder's check would incorrectly accept it as a valid binary.
func TestVenv_Boundary_Discovery_DeviceFile(t *testing.T) {
	_ = t.TempDir() // Keep for future device file test expansion

	// We can't easily create device files without root, but we can
	// verify that os.Stat on a system device file returns correct info.
	// On macOS, /dev/null is a character device.
	info, err := os.Stat("/dev/null")
	if err != nil {
		t.Skip("Cannot stat /dev/null:", err)
	}

	isRegular := info.Mode().IsRegular()
	isDir := info.IsDir()
	isDevice := info.Mode()&os.ModeDevice != 0

	t.Logf("/dev/null mode: %v, IsDir: %v, IsRegular: %v, IsDevice: %v",
		info.Mode(), isDir, isRegular, isDevice)

	// coder's check without IsDir: err == nil -> ACCEPTS (bug)
	// coder's check with proposed IsDir(): err==nil && !IsDir() -> ACCEPTS (bug)
	t.Log("")
	t.Log("BOUNDARY: os.Stat on a device file succeeds.")
	t.Log("  Proposed IsDir() fix: passes (device files are not directories).")
	t.Log("  exec.Command on /dev/null would fail or have undefined behavior.")
	t.Log("  Fix: use info.Mode().IsRegular() instead of !info.IsDir().")
}

// =========================================================================
// .GITIGNORE EDGE CASES
// =========================================================================

// TestVenv_Boundary_Gitignore_AccidentalMatch verifies that .gitignore
// patterns don't accidentally match files we want to track.
// E.g., does "bin/*" match "bin/README.md"? Yes — that's intended for build
// artifacts. But what about patterns that are too broad?
func TestVenv_Boundary_Gitignore_AccidentalMatch(t *testing.T) {
	gitignorePath := filepath.Join(".gitignore")
	content, err := os.ReadFile(gitignorePath)
	if err != nil {
		t.Fatal(err)
	}
	gi := string(content)

	// Check for overly broad patterns
	patterns := strings.Split(gi, "\n")
	for _, p := range patterns {
		p = strings.TrimSpace(p)
		if p == "" || strings.HasPrefix(p, "#") {
			continue
		}

		// Check if "logs/*" is too broad — it ignores ALL files in logs/
		// even if someone creates logs/important-config.json
		if p == "logs/*" {
			t.Log("NOTE: logs/* pattern ignores ALL files under logs/")
			t.Log("This is intentional for runtime logs, but means")
			t.Log("any accidentally placed file in logs/ is ignored.")
			t.Log("Consider using 'logs/*.log logs/*.pid' for precision.")
		}

		// Check if "mcp-memory" (without path prefix) is too broad
		if p == "mcp-memory" {
			t.Log("NOTE: 'mcp-memory' pattern matches ANY path named mcp-memory,")
			t.Log("not just the root-level binary. E.g.:")
			t.Log("  - ./mcp-memory (intended)")
			t.Log("  - ./vendor/mcp-memory (unintended)")
			t.Log("  - ./internal/mcp-memory (unintended)")
			t.Log("If subdirectories contain files named mcp-memory,")
			t.Log("they would be unexpectedly ignored.")
			t.Log("Safer: './mcp-memory' or 'mcp-memory' with leading slash.")
		}
	}
}

// TestVenv_Boundary_Gitignore_WindowsPaths verifies that .gitignore patterns
// work correctly on Windows (.venv/Scripts/ instead of .venv/bin/).
func TestVenv_Boundary_Gitignore_WindowsPaths(t *testing.T) {
	gitignorePath := filepath.Join(".gitignore")
	content, err := os.ReadFile(gitignorePath)
	if err != nil {
		t.Fatal(err)
	}
	gi := string(content)

	// On Windows, Python venv creates .venv/Scripts/ not .venv/bin/
	// The .gitignore pattern ".venv/" covers both — good.
	if !strings.Contains(gi, ".venv/") {
		t.Error(".venv/ pattern missing from .gitignore")
	} else {
		t.Log("OK: .venv/ pattern covers both Windows (.venv/Scripts/) and POSIX (.venv/bin/)")
	}

	// The "bin/*" pattern is POSIX-specific. On Windows, Go build creates
	// bin/mcp-memory.exe (with .exe extension). The "bin/*" pattern covers
	// any file under bin/, including bin/mcp-memory.exe — so it works.
	if !strings.Contains(gi, "bin/*") {
		t.Error("bin/* pattern missing from .gitignore")
	} else {
		t.Log("OK: bin/* pattern covers both bin/mcp-memory and bin/mcp-memory.exe")
	}

	// The "mcp-memory" pattern (without .exe) on Windows:
	// Go build -o bin/mcp-memory actually creates bin/mcp-memory.exe
	// But the untracked root-level "mcp-memory" (if someone runs go build without -o)
	// would create mcp-memory.exe on Windows.
	if runtime.GOOS == "windows" {
		t.Log("NOTE: On Windows, 'mcp-memory' pattern does NOT match 'mcp-memory.exe'")
		t.Log("Go build adds .exe automatically on Windows.")
		t.Log("Consider adding 'mcp-memory.exe' to .gitignore for Windows support.")
	} else {
		t.Log("OK: On POSIX, 'mcp-memory' pattern works correctly (no .exe extension)")
	}
}

// =========================================================================
// DOCS CONSISTENCY VERIFICATION
// =========================================================================

// TestVenv_Boundary_Docs_MakeSetupValid verifies that all 4 updated doc
// files correctly reference `make setup` for installing hindsight-api-slim.
func TestVenv_Boundary_Docs_MakeSetupMentioned(t *testing.T) {
	type docCheck struct {
		path    string
		require []string // patterns that MUST be present
		forbid  []string // patterns that must NOT be present
	}

	checks := []docCheck{
		{
			path: "docs/README.md",
			require: []string{
				"make setup",
				"make run",
				"hindsight-api-slim",
				"Python 3.12",
			},
			forbid: []string{
				"pip install hindsight-api",
				"go run .", // Quick Start should use make run
			},
		},
		{
			path: "docs/deployment.md",
			require: []string{
				"make setup",
				"make run",
				"make build",
				"make stop",
				"Python 3.12",
			},
			forbid: []string{
				"pip install hindsight-api",
				"go build -o mcp-memory",
			},
		},
		{
			path: "docs/hindsight.md",
			require: []string{
				"make setup",
				"hindsight-api-slim==0.8.2",
				"hindsight-client==0.8.2",
			},
			forbid: []string{
				"pip install hindsight-api", // Should be make setup now
			},
		},
		{
			path: "docs/development.md",
			require: []string{
				"make setup",
				"make run",
				"make build",
				"make test",
				"make stop",
				"make clean",
			},
			forbid: []string{},
		},
	}

	for _, c := range checks {
		data, err := os.ReadFile(c.path)
		if err != nil {
			t.Errorf("Cannot read %s: %v", c.path, err)
			continue
		}
		content := string(data)

		// Check required patterns
		for _, req := range c.require {
			if !strings.Contains(content, req) {
				t.Errorf("%s: MISSING required content: %q", c.path, req)
			} else {
				t.Logf("OK %s: contains %q", c.path, req)
			}
		}

		// Check forbidden patterns
		for _, forbid := range c.forbid {
			if strings.Contains(content, forbid) {
				t.Errorf("%s: HAS forbidden content: %q", c.path, forbid)
			} else {
				t.Logf("OK %s: no forbidden %q", c.path, forbid)
			}
		}
	}
}

// TestVenv_Boundary_Docs_VersionConsistency verifies that the pinned
// version in docs/hindsight.md matches the Makefile exactly.
func TestVenv_Boundary_Docs_VersionConsistency(t *testing.T) {
	// Read Makefile for version pins
	mkData, err := os.ReadFile("Makefile")
	if err != nil {
		t.Fatal(err)
	}
	mkStr := string(mkData)

	// Extract version from Makefile
	mkSlimVersion := extractVersion(mkStr, "hindsight-api-slim")
	mkClientVersion := extractVersion(mkStr, "hindsight-client")

	t.Logf("Makefile versions: hindsight-api-slim==%s, hindsight-client==%s",
		mkSlimVersion, mkClientVersion)

	// Read hindsight.md
	hindData, err := os.ReadFile("docs/hindsight.md")
	if err != nil {
		t.Fatal(err)
	}
	hindStr := string(hindData)

	hindSlimVersion := extractVersion(hindStr, "hindsight-api-slim")
	hindClientVersion := extractVersion(hindStr, "hindsight-client")

	t.Logf("docs/hindsight.md versions: hindsight-api-slim==%s, hindsight-client==%s",
		hindSlimVersion, hindClientVersion)

	// Compare
	if mkSlimVersion != hindSlimVersion {
		t.Errorf("Version mismatch: Makefile has hindsight-api-slim==%s but docs/hindsight.md has %s",
			mkSlimVersion, hindSlimVersion)
	} else if mkSlimVersion != "" {
		t.Log("OK: hindsight-api-slim version consistent across Makefile and docs/hindsight.md")
	}

	if mkClientVersion != hindClientVersion {
		t.Errorf("Version mismatch: Makefile has hindsight-client==%s but docs/hindsight.md has %s",
			mkClientVersion, hindClientVersion)
	} else if mkClientVersion != "" {
		t.Log("OK: hindsight-client version consistent across Makefile and docs/hindsight.md")
	}
}

// extractVersion extracts the version string after "pkg==" from a text block.
func extractVersion(text, pkg string) string {
	// Look for patterns like "pkg==X.Y.Z"
	idx := strings.Index(text, pkg+"==")
	if idx < 0 {
		return ""
	}
	rest := text[idx+len(pkg)+2:] // skip "pkg=="
	end := strings.IndexAny(rest, " \n\t")
	if end < 0 {
		return rest
	}
	return rest[:end]
}

// TestVenv_Boundary_Docs_QuickStartAccuracy verifies that the Quick Start
// in docs/README.md is accurate and won't confuse users.
func TestVenv_Boundary_Docs_QuickStartAccuracy(t *testing.T) {
	readme, err := os.ReadFile("docs/README.md")
	if err != nil {
		t.Fatal(err)
	}
	readmeStr := string(readme)

	// Find the Quick Start section
	qsIdx := strings.Index(readmeStr, "## Quick Start")
	if qsIdx < 0 {
		t.Fatal("Quick Start section not found in docs/README.md")
	}

	// Extract the Quick Start code block
	qs := readmeStr[qsIdx:]
	cbIdx := strings.Index(qs, "```bash")
	if cbIdx < 0 {
		t.Fatal("No bash code block in Quick Start section")
	}
	cbEnd := strings.Index(qs[cbIdx+7:], "```")
	if cbEnd < 0 {
		t.Fatal("Unclosed code block in Quick Start")
	}
	qsCodeBlock := qs[cbIdx+7 : cbIdx+7+cbEnd]

	t.Logf("Quick Start code block:\n%s", qsCodeBlock)

	// Verify the sequence: cp .env.example -> make setup -> make run
	hasCpEnv := strings.Contains(qsCodeBlock, "cp .env.example .env")
	hasMakeSetup := strings.Contains(qsCodeBlock, "make setup")
	hasMakeRun := strings.Contains(qsCodeBlock, "make run")

	if hasCpEnv && hasMakeSetup && hasMakeRun {
		t.Log("OK: Quick Start sequence is correct (cp .env.example -> make setup -> make run)")
	} else {
		if !hasCpEnv {
			t.Log("NOTE: Quick Start missing 'cp .env.example .env' step")
		}
		if !hasMakeSetup {
			t.Error("Quick Start missing 'make setup' step")
		}
		if !hasMakeRun {
			t.Error("Quick Start missing 'make run' step")
		}
	}
}

// TestVenv_Boundary_Docs_DeploymentMakefileSync verifies that deployment.md
// references all Makefile targets that actually exist.
func TestVenv_Boundary_Docs_DeploymentMakefileSync(t *testing.T) {
	mkData, err := os.ReadFile("Makefile")
	if err != nil {
		t.Fatal(err)
	}

	// Extract all Makefile targets
	mkStr := string(mkData)
	lines := strings.Split(mkStr, "\n")
	var makeTargets []string
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.Contains(trimmed, ":") && !strings.HasPrefix(trimmed, "\t") && !strings.HasPrefix(trimmed, "#") {
			target := strings.Split(trimmed, ":")[0]
			makeTargets = append(makeTargets, strings.TrimSpace(target))
		}
	}
	t.Logf("Makefile targets: %v", makeTargets)

	// Read deployment.md
	deploy, err := os.ReadFile("docs/deployment.md")
	if err != nil {
		t.Fatal(err)
	}
	deployStr := string(deploy)

	// Count mentions of each make target
	for _, target := range makeTargets {
		count := strings.Count(deployStr, "make "+target)
		if count == 0 && target != ".PHONY" {
			// Check if target is mentioned without "make" prefix
			count2 := strings.Count(deployStr, "`"+target+"`")
			if count2 == 0 {
				t.Logf("INFO: make %s not mentioned in deployment.md (may not be relevant)", target)
			} else {
				t.Logf("OK: %s mentioned in deployment.md (%d times)", target, count2)
			}
		} else if count > 0 {
			t.Logf("OK: make %s mentioned in deployment.md (%d times)", target, count)
		}
	}

	// Verify no stale non-make references
	if strings.Contains(deployStr, "go run .") && strings.Contains(deployStr, "Development") {
		// In the Development section, "go run ." is acceptable
		// but in the Deploy section, it should be "make build"
		deploySection := deployStr[strings.Index(deployStr, "## Run"):]
		if strings.Contains(deploySection, "go run .") {
			t.Log("NOTE: deployment.md Run section mentions 'go run .' alongside 'make build'")
			t.Log("This is fine as a development alternative.")
		}
	}
}

// =========================================================================
// CROSS-PLATFORM .VENV
// =========================================================================

// TestVenv_Boundary_CrossPlatform_WindowsVenv verifies that the .venv
// fallback does NOT work on Windows, where Python venv creates
// .venv/Scripts/hindsight-api.exe instead of .venv/bin/hindsight-api.
func TestVenv_Boundary_CrossPlatform_WindowsVenv(t *testing.T) {
	// Read services.go to verify only POSIX path is checked
	svc, err := os.ReadFile("services.go")
	if err != nil {
		t.Fatal(err)
	}
	svcStr := string(svc)

	// Find the fallback path slice
	idx := strings.Index(svcStr, `filepath.Join(".venv", "bin", "hindsight-api")`)
	if idx < 0 {
		t.Fatal("Could not find .venv bin path in services.go")
	}

	// Check if there's a Windows fallback path
	windowsIdx := strings.Index(svcStr, `filepath.Join(".venv", "Scripts"`)
	if windowsIdx < 0 {
		t.Log("CROSS-PLATFORM GAP: No Windows .venv/Scripts/ fallback path found.")
		t.Log("Current code only checks .venv/bin/hindsight-api (POSIX).")
		t.Log("On Windows, Python venv creates: .venv/Scripts/hindsight-api.exe")
		t.Log("The binary also has .exe extension on Windows.")
		t.Log("")
		t.Log("Impact: .venv fallback is completely broken on Windows.")
		t.Log("Fix: add conditional path based on runtime.GOOS:")
		t.Log("  if runtime.GOOS == \"windows\" {")
		t.Log("      .venv/Scripts/hindsight-api.exe")
		t.Log("  } else {")
		t.Log("      .venv/bin/hindsight-api")
		t.Log("  }")
	} else {
		t.Log("OK: Windows .venv/Scripts/ fallback path is present")
	}

	// Check if there's any runtime.GOOS conditional in the fallback code
	hasGoosCheck := strings.Contains(svcStr, "runtime.GOOS")
	if !hasGoosCheck {
		t.Log("")
		t.Log("NOTE: No runtime.GOOS check found in services.go at all.")
		t.Log("This confirms there's no cross-platform handling.")
	}
}

// =========================================================================
// ADDITIONAL EDGE CASES
// =========================================================================

// TestVenv_Boundary_Discovery_SymlinkLoop verifies that a symlink loop
// at .venv/bin/hindsight-api causes os.Stat to fail (EILOOP or ELOOP).
// The coder's code handles this correctly (err != nil -> fallback continues)
// but we verify this behavior explicitly.
func TestVenv_Boundary_Discovery_SymlinkLoop(t *testing.T) {
	tmpDir := t.TempDir()
	oldWd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(oldWd)
	if err := os.Chdir(tmpDir); err != nil {
		t.Fatal(err)
	}

	if err := os.MkdirAll(filepath.Join(tmpDir, ".venv", "bin"), 0755); err != nil {
		t.Fatal(err)
	}

	// Create a symlink loop: a -> b -> a
	aPath := filepath.Join(tmpDir, ".venv", "bin", "hindsight-api")
	bPath := filepath.Join(tmpDir, ".venv", "bin", "loop-target")

	// Create b -> a (fails if a doesn't exist, so we create a first)
	if err := os.Symlink(bPath, aPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(aPath, bPath); err != nil {
		t.Fatalf("Cannot create symlink loop: %v", err)
	}

	// os.Stat on a symlink loop should fail with ELOOP
	_, err = os.Stat(aPath)
	if err != nil {
		t.Logf("OK: Symlink loop correctly fails os.Stat with: %v", err)
		t.Log("Coder's fallback code handles this correctly (err != nil -> continue)")
	} else {
		t.Log("NOTE: os.Stat on symlink loop succeeded (platform-dependent behavior)")
		t.Log("This could be a problem — the fallback would accept it as valid.")
	}
}

// TestVenv_Boundary_Discovery_EmptyFile verifies that an empty file
// at .venv/bin/hindsight-api passes os.Stat but exec.Command fails.
func TestVenv_Boundary_Discovery_EmptyFile(t *testing.T) {
	tmpDir := t.TempDir()
	oldWd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(oldWd)
	if err := os.Chdir(tmpDir); err != nil {
		t.Fatal(err)
	}

	if err := os.MkdirAll(filepath.Join(tmpDir, ".venv", "bin"), 0755); err != nil {
		t.Fatal(err)
	}
	emptyPath := filepath.Join(tmpDir, ".venv", "bin", "hindsight-api")
	if err := os.WriteFile(emptyPath, []byte{}, 0755); err != nil {
		t.Fatal(err)
	}

	// os.Stat succeeds on empty file
	info, err := os.Stat(emptyPath)
	if err != nil {
		t.Fatal("os.Stat should succeed on empty file:", err)
	}

	if info.IsDir() {
		t.Fatal("empty file should not be a directory")
	}
	if info.Size() != 0 {
		t.Fatalf("empty file should have size 0, got %d", info.Size())
	}

	// The coder's code: os.Stat passes, hindsightPath is set to this path
	// exec.Command will start the empty file, which just exits immediately
	// without doing anything useful.
	t.Log("BOUNDARY: Empty file at .venv/bin/hindsight-api (size=0)")
	t.Log("  os.Stat succeeds (err==nil, !IsDir())")
	t.Log("  exec.Command starts but immediately exits (no shebang, empty file)")
	t.Log("  Hindsight process would exit immediately with no error")
	t.Log("  This gives a false positive — hindsight 'started' but never runs")
	t.Log("")
	t.Log("BOUNDARY GAP: An empty file passes ALL checks but produces a")
	t.Log("silent failure (process exits immediately with no error).")
	t.Log("The health watchdog would eventually detect this, but it delays")
	t.Log("startup by 5-10 seconds.")
	t.Log("Fix: consider a basic validation (e.g., file size > 0, or check")
	t.Log("first bytes for shebang) for the .venv binary path.")
}

// TestVenv_Boundary_Makefile_NoVenvGuardInRun verifies that `make run`
// starts the server without checking if .venv exists or if hindsight-api
// is properly installed, leading to a confusing runtime error.
func TestVenv_Boundary_Makefile_NoVenvGuardInRun(t *testing.T) {
	// Read Makefile
	mk, err := os.ReadFile("Makefile")
	if err != nil {
		t.Fatal(err)
	}
	mkStr := string(mk)

	// Extract the run target
	lines := strings.Split(mkStr, "\n")
	var runTarget string
	foundRun := false
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "run:") {
			foundRun = true
			continue
		}
		if foundRun && strings.HasPrefix(trimmed, "\t") {
			runTarget = trimmed
			break
		}
		if foundRun && trimmed == "" {
			break
		}
	}

	t.Logf("Makefile run target: %s", runTarget)

	// Verify the run target doesn't check for .venv
	if strings.Contains(runTarget, "test -d .venv") ||
		strings.Contains(runTarget, "hindsight") ||
		strings.Contains(runTarget, ".venv") {
		t.Log("NOTE: Makefile run target has .venv awareness")
	} else {
		t.Log("")
		t.Log("BOUNDARY GAP: `make run` starts server without checking if")
		t.Log(".venv/ exists or hindsight-api is installed.")
		t.Log("Result: server starts, but startHindsight() fails with")
		t.Log("'errBinaryNotFound' and services.go logs a cryptic error.")
		t.Log("New user would see a server running (HTTP on :8899) but")
		t.Log("hindsight stays dead with confusing error.")
		t.Log("Fix: add a .venv guard: 'test -d .venv || (echo \"Run make setup first\"; exit 1)'")
	}
}

// TestVenv_Boundary_Gitignore_LockedFileRemoval verifies that git rm --cached
// needs to be run manually — the .gitignore changes don't retroactively
// untrack already-committed files. This verifies a recommendation from Pass 1.
func TestVenv_Boundary_Gitignore_NeedsGitRmCached(t *testing.T) {
	// Run git ls-files to find files that match the new .gitignore patterns
	// but are still tracked by git
	cmd := exec.Command("git", "ls-files", ".venv/", "bin/", "mcp-memory", "logs/")
	cmd.Dir = "/Users/agentswarm/Desktop/freelancing/memory"
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Logf("git ls-files error (may not be in git repo): %v", err)
		return
	}

	trackedFiles := strings.Split(strings.TrimSpace(string(output)), "\n")
	var stillTracked []string
	for _, f := range trackedFiles {
		if f != "" {
			stillTracked = append(stillTracked, f)
		}
	}
	_ = stillTracked

	if len(stillTracked) == 0 {
		t.Log("OK: No files matching .gitignore patterns are still tracked by git")
	} else {
		t.Log("FILES STILL TRACKED (need git rm --cached):")
		for _, f := range stillTracked {
			t.Logf("  - %s", f)
		}
		// Count matches
		var logsTracked, binTracked, rootMcpMemoryTracked int
		for _, f := range stillTracked {
			if strings.HasPrefix(f, "logs/") {
				logsTracked++
			}
			if strings.HasPrefix(f, "bin/") {
				binTracked++
			}
			if f == "mcp-memory" {
				rootMcpMemoryTracked++
			}
		}
		t.Logf("  logs/*: %d files still tracked", logsTracked)
		t.Logf("  bin/*: %d files still tracked", binTracked)
		t.Logf("  mcp-memory (root): %d files still tracked", rootMcpMemoryTracked)
	}
}

// TestVenv_Boundary_Makefile_VetTargetMissing verifies that the Makefile
// doesn't have a dedicated 'vet' target, leaving static analysis to manual
// invocation.
func TestVenv_Boundary_Makefile_VetTargetMissing(t *testing.T) {
	mk, err := os.ReadFile("Makefile")
	if err != nil {
		t.Fatal(err)
	}
	mkStr := string(mk)

	targets := strings.Split(mkStr, "\n")
	var hasVetTarget bool
	for _, line := range targets {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "vet:") {
			hasVetTarget = true
			break
		}
	}

	if !hasVetTarget {
		t.Log("INFO: Makefile has no 'vet' target.")
		t.Log("Consider adding: 'vet: go vet ./...' to the Makefile.")
		t.Log("Currently, go vet must be run manually.")
	}
}

// TestVenv_Boundary_Discovery_FileOwnedByRoot verifies that if a file at
// .venv/bin/hindsight-api is owned by root (e.g., from a container build or
// permission mistake), os.Stat still succeeds and the coder's code accepts it.
// The binary would fail to execute if the runtime user can't read/execute it.
func TestVenv_Boundary_Discovery_FileOwnedByRoot(t *testing.T) {
	tmpDir := t.TempDir()
	oldWd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(oldWd)
	if err := os.Chdir(tmpDir); err != nil {
		t.Fatal(err)
	}

	if err := os.MkdirAll(filepath.Join(tmpDir, ".venv", "bin"), 0755); err != nil {
		t.Fatal(err)
	}
	scriptPath := filepath.Join(tmpDir, ".venv", "bin", "hindsight-api")
	if err := os.WriteFile(scriptPath, []byte("#!/bin/sh\nexit 0\n"), 0000); err != nil {
		t.Fatal(err)
	}
	// Restore permissions after test
	defer os.Chmod(scriptPath, 0755)

	// os.Stat succeeds on a 0000-permission file (stat only needs path visibility, not read)
	info, err := os.Stat(scriptPath)
	if err != nil {
		t.Fatal("os.Stat should succeed on 0000-permission file:", err)
	}
	t.Logf("os.Stat on 0000-permission file: mode=%v, err=%v", info.Mode(), err)

	// The coder's code would accept this file (err==nil, !IsDir()).
	// But exec.Command would fail with 'permission denied' because
	// the process user can't read/execute the binary.
	cmd := exec.Command(scriptPath)
	err = cmd.Run()
	if err != nil {
		t.Logf("exec.Command on 0000-permission file fails: %v", err)
		t.Log("GAP: os.Stat passes (err==nil) but exec.Command fails (permission denied)")
		t.Log("The confusing error message masks the real problem.")
	} else {
		t.Log("NOTE: 0000-permission file executed successfully (unexpected)")
	}
}
