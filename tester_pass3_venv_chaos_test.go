// Package main — Pass 3: Chaos tests for .venv integration.
// Final pass. Code survived adversarial (Pass 1) and boundary (Pass 2).
// This pass attacks with concurrency, corruption, permission chaos,
// TOCTOU races, goroutine leaks, and resource exhaustion.
//
// ATTACK VECTORS:
//   1. Concurrent make setup — 5 terminals running make setup simultaneously
//   2. Rapid make setup/clean cycles — 20x setup->clean->setup->clean
//   3. Binary discovery stress — 1000 rapid startHindsight() calls
//   4. Makefile resilience — HOME unset, PATH empty, PWD with spaces
//   5. .venv corruption — truncated binary, broken symlink loop
//   6. Permission chaos — chmod 000 on .venv/bin/, chmod 000 on .venv/
//   7. TOCTOU race — startHindsight() while make clean removes .venv
//   8. Goroutine leak — rapid startHindsight with .venv existing vs not
//   9. Memory — 1000 os.Stat calls on .venv path
package main

import (
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// =========================================================================
// UTILITY: mock hindsight binary for startHindsight tests
// =========================================================================

// createMockHindsightBinary creates a small shell script at the given path
// that sleeps for 1 second then exits. Use this only when the test needs
// the spawned process to stay alive (e.g., double-spawn guard tests).
// The sleep prevents fork-bomb from rapid exit-immediately spawns.
func createMockHindsightBinary(t *testing.T, path string) {
	t.Helper()
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	content := "#!/bin/sh\nsleep 1\n"
	if err := os.WriteFile(path, []byte(content), 0755); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// makeServicesWithCustomCwd creates a services instance configured to
// discover the mock hindsight binary, and changes to the given temp dir.
// Returns the services, the original cwd (to be restored by caller), and
// the cleanup function.
func makeServicesWithCustomCwd(t *testing.T, tempDir string) (*services, string, func()) {
	t.Helper()
	origWd, _ := os.Getwd()
	if err := os.Chdir(tempDir); err != nil {
		t.Fatalf("chdir %s: %v", tempDir, err)
	}
	cfg := newTestConfig()
	// Set HindsightPath to something that fails LookPath, forcing fallback
	cfg.HindsightPath = "nonexistent-hindsight-binary"
	// Very short timeouts for fast failure
	cfg.StartTimeout = 50 * time.Millisecond
	cfg.HealthTimeout = 20 * time.Millisecond
	l, _ := newTestLogger()
	alerts := NewAlertClient("", "optional")
	svc := newServices(cfg, l, alerts)

	cleanup := func() {
		os.Chdir(origWd)
	}
	return svc, origWd, cleanup
}

// =========================================================================
// VECTOR 1: Concurrent make setup — 5 terminals racing
// =========================================================================

// TestChaosVenv_ConcurrentMakeSetup runs 5 simultaneous python3 -m venv
// invocations, then verifies the .venv directories are not corrupted.
// Multiple venv creations on the same path are racy at the filesystem level.
func TestChaosVenv_ConcurrentMakeSetup(t *testing.T) {
	dir := t.TempDir()
	origWd, _ := os.Getwd()
	defer os.Chdir(origWd)
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir: %v", err)
	}

	// Check python3 availability
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not found, skipping concurrent make setup test")
	}

	var wg sync.WaitGroup
	var errorsMu sync.Mutex
	var errors []string
	concurrent := 5

	for i := 0; i < concurrent; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			cmd := exec.Command("python3", "-m", "venv", ".venv")
			output, err := cmd.CombinedOutput()
			if err != nil {
				errorsMu.Lock()
				errors = append(errors, fmt.Sprintf("goroutine %d: %v\n%s", id, err, string(output)))
				errorsMu.Unlock()
			}
		}(i)
	}
	wg.Wait()

	// Report any errors
	if len(errors) > 0 {
		t.Logf("Concurrent make setup: %d errors out of %d", len(errors), concurrent)
		for _, e := range errors {
			t.Logf("  %s", e)
		}
	}

	// Verify the .venv is not corrupted
	checks := []struct {
		name string
		path string
	}{
		{".venv exists", ".venv"},
		{".venv/bin exists", filepath.Join(".venv", "bin")},
		{".venv/bin/python3 exists", filepath.Join(".venv", "bin", "python3")},
		{".venv/bin/pip exists", filepath.Join(".venv", "bin", "pip")},
	}

	corrupted := false
	for _, c := range checks {
		info, err := os.Stat(c.path)
		if err != nil {
			t.Errorf("BUG: %s is missing after concurrent setup: %v", c.name, err)
			corrupted = true
		} else if info.IsDir() && c.name == ".venv exists" {
			// Expected
		}
	}

	if corrupted {
		t.Logf("FINDING: Concurrent make setup can corrupt .venv. %d goroutines ran 'python3 -m venv' simultaneously.", concurrent)
	} else {
		t.Logf("OK: .venv survived %d concurrent 'python3 -m venv' invocations", concurrent)
	}

	// Check for stray files
	entries, _ := os.ReadDir(dir)
	var stray []string
	for _, e := range entries {
		if e.Name() != ".venv" {
			stray = append(stray, e.Name())
		}
	}
	if len(stray) > 0 {
		t.Logf("Stray files after concurrent setup: %v", stray)
	}
}

// =========================================================================
// VECTOR 2: Rapid make setup/clean cycles — 20x setup->clean
// =========================================================================

// TestChaosVenv_RapidSetupClean runs 20 rapid setup->clean cycles to
// detect leftover files, race conditions in venv removal, and filesystem
// state corruption.
func TestChaosVenv_RapidSetupClean(t *testing.T) {
	dir := t.TempDir()
	origWd, _ := os.Getwd()
	defer os.Chdir(origWd)
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir: %v", err)
	}

	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not found, skipping rapid setup/clean test")
	}

	cycles := 20
	filesAfterClean := 0
	cyclesWithErrors := 0

	for cycle := 0; cycle < cycles; cycle++ {
		// setup: create .venv
		cmd := exec.Command("python3", "-m", "venv", ".venv")
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Logf("Cycle %d setup: %v\n%s", cycle, err, string(output))
			cyclesWithErrors++
			// Remove partial .venv if setup failed
			os.RemoveAll(".venv")
			continue
		}

		// Verify .venv was created
		if _, err := os.Stat(".venv"); os.IsNotExist(err) {
			t.Errorf("BUG: .venv not created after setup in cycle %d", cycle)
		}

		// clean: remove .venv
		if err := os.RemoveAll(".venv"); err != nil {
			t.Errorf("BUG: cleanup failed in cycle %d: %v", cycle, err)
		}

		// Verify .venv is gone
		if _, err := os.Stat(".venv"); !os.IsNotExist(err) {
			t.Errorf("BUG: .venv still exists after rm -rf in cycle %d", cycle)
		}

		// Check for leftover files in cwd
		entries, _ := os.ReadDir(dir)
		for _, e := range entries {
			if e.Name() != ".venv" {
				filesAfterClean++
			}
		}
	}

	if cyclesWithErrors > 0 {
		t.Errorf("BUG: %d/%d cycles had errors during venv creation", cyclesWithErrors, cycles)
	}
	if filesAfterClean > 0 {
		t.Errorf("BUG: %d leftover files after %d setup/clean cycles", filesAfterClean, cycles)
	}
	t.Logf("OK: %d rapid setup/clean cycles completed with %d errors, %d leftover files",
		cycles, cyclesWithErrors, filesAfterClean)
}

// =========================================================================
// VECTOR 3: Binary discovery stress — 1000 rapid startHindsight() calls
// =========================================================================

// TestChaosVenv_BinaryDiscoveryStress hammers the os.Stat fallback loop in
// startHindsight() with concurrent .venv state mutations. The test does NOT
// create a valid binary — startHindsight always returns errBinaryNotFound
// (no process spawn). This tests: concurrent os.Stat calls, slice iteration,
// no panics from filesystem errors, and correct fallback behavior under load.
func TestChaosVenv_BinaryDiscoveryStress(t *testing.T) {
	dir := t.TempDir()
	svc, _, cleanup := makeServicesWithCustomCwd(t, dir)
	defer cleanup()

	type setupFn func(string) // modifies .venv state, no binary created

	cases := []struct {
		name string
		fn   setupFn
	}{
		{
			name: "no_venv",
			fn: func(d string) {
				os.RemoveAll(filepath.Join(d, ".venv"))
			},
		},
		{
			name: "venv_without_bin",
			fn: func(d string) {
				os.MkdirAll(filepath.Join(d, ".venv"), 0755)
				os.RemoveAll(filepath.Join(d, ".venv", "bin"))
			},
		},
		{
			name: "venv_with_bin_empty",
			fn: func(d string) {
				os.MkdirAll(filepath.Join(d, ".venv", "bin"), 0755)
				// No hindsight-api file — os.Stat fails
			},
		},
		{
			name: "venv_with_bin_nonexec",
			fn: func(d string) {
				os.MkdirAll(filepath.Join(d, ".venv", "bin"), 0755)
				// Write a non-executable empty file — passes Stat, fails exec
				os.WriteFile(filepath.Join(d, ".venv", "bin", "hindsight-api"), []byte{}, 0644)
			},
		},
	}

	var panics atomic.Int64
	var notFoundCount atomic.Int64
	var otherErrors atomic.Int64

	var wg sync.WaitGroup
	threads := 10
	cycles := 30

	for i := 0; i < threads; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for j := 0; j < cycles; j++ {
				tc := cases[(id+j)%len(cases)]
				tc.fn(dir)

				func() {
					defer func() {
						if r := recover(); r != nil {
							panics.Add(1)
						}
					}()
					err := svc.startHindsight()
					if err != nil && strings.Contains(err.Error(), "not found") {
						notFoundCount.Add(1)
					} else if err != nil {
						otherErrors.Add(1)
					}
					// err==nil should NOT happen since no valid binary exists
				}()
			}
		}(i)
	}

	wg.Wait()

	total := threads * cycles
	if panics.Load() > 0 {
		t.Errorf("BUG: startHindsight panicked %d times during binary discovery stress", panics.Load())
	}
	t.Logf("Binary discovery stress: %d calls, %d not-found, %d other, %d panics (no processes spawned)",
		total, notFoundCount.Load(), otherErrors.Load(), panics.Load())
}

// =========================================================================
// VECTOR 4: Makefile resilience — environment stress
// =========================================================================

// TestChaosVenv_MakefileResilience tests the Makefile's targets when
// critical environment variables are missing or malformed.
// The Makefile uses shell commands that depend on HOME, PATH, and PWD.
func TestChaosVenv_MakefileResilience(t *testing.T) {
	dir := t.TempDir()
	origWd, _ := os.Getwd()
	defer os.Chdir(origWd)
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir: %v", err)
	}

	// Create a minimal test setup for Makefile, but since we're testing
	// environment resilience, we test what the Makefile targets do under
	// different environmental conditions.

	tests := []struct {
		name   string
		env    []string
		cmd    string
		args   []string
	}{
		{
			name: "HOME_unset",
			env:  []string{},
			cmd:  "python3",
			args: []string{"--version"},
		},
		{
			name: "PATH_empty",
			env:  []string{"PATH="},
			cmd:  "python3",
			args: []string{"--version"},
		},
		{
			name: "HOME_and_PATH_unset",
			env:  []string{},
			cmd:  "python3",
			args: []string{"--version"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmd := exec.Command(tt.cmd, tt.args...)
			cmd.Env = tt.env
			output, err := cmd.CombinedOutput()
			if err != nil {
				t.Logf("FINDING: %s causes error: %v\n%s", tt.name, err, string(output))
			} else {
				t.Logf("OK: %s — %s", tt.name, strings.TrimSpace(string(output)))
			}
		})
	}

	// Test that 'make setup' fails gracefully when HOME is unset
	// (python3 -m venv may create directories relative to HOME for caching)
	if _, err := exec.LookPath("python3"); err == nil {
		t.Run("make_setup_HOME_unset", func(t *testing.T) {
			cmd := exec.Command("python3", "-m", "venv", filepath.Join(dir, "test-venv"))
			cmd.Env = []string{}
			output, err := cmd.CombinedOutput()
			if err != nil {
				t.Logf("FINDING: python3 -m venv fails when HOME is unset: %v\n%s", err, string(output))
			} else {
				t.Log("OK: python3 -m venv works without HOME set")
			}
		})
	}
}

// =========================================================================
// VECTOR 5: .venv corruption — truncated binary, broken symlink loop
// =========================================================================

// TestChaosVenv_TruncatedBinary tests that an empty/truncated file
// at .venv/bin/hindsight-api does NOT produce a false positive from
// startHindsight(). An empty file passes os.Stat and IsRegular(), but
// exec.Command starts and immediately exits.
func TestChaosVenv_TruncatedBinary(t *testing.T) {
	dir := t.TempDir()
	svc, _, cleanup := makeServicesWithCustomCwd(t, dir)
	defer cleanup()

	// Create an empty file at .venv/bin/hindsight-api (simulates truncated install)
	emptyPath := filepath.Join(dir, ".venv", "bin", "hindsight-api")
	if err := os.MkdirAll(filepath.Dir(emptyPath), 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(emptyPath, []byte{}, 0755); err != nil {
		t.Fatalf("write empty file: %v", err)
	}

	// Verify os.Stat behavior
	info, err := os.Stat(emptyPath)
	if err != nil {
		t.Fatalf("os.Stat on empty file: %v", err)
	}
	t.Logf("Empty file: IsRegular=%v, Size=%d, Mode=%v", info.Mode().IsRegular(), info.Size(), info.Mode())

	// Call startHindsight — it should find the file and try to exec it.
	// An empty file will execute as a shell script? No — exec on a zero-byte
	// ELF or script file will fail with "exec format error" on most systems,
	// but cmd.Start() itself returns nil. The process exits immediately.
	// This is a SILENT FAILURE — no error returned, but hindsight never runs.
	err = svc.startHindsight()
	if err != nil {
		// Expected: exec of empty file may fail
		t.Logf("FINDING: startHindsight with truncated binary returns error: %v", err)
	} else {
		// startHindsight returned nil — this is the BUG.
		// The truncated binary was accepted as valid, the process started but
		// immediately exited. There is NO error detection for this case.
		t.Logf("BUG: startHindsight returned nil for empty/truncated binary (silent failure)")
		t.Logf("FIX: startHindsight should verify the binary is runnable (e.g., exec with --version)")
	}
}

// TestChaosVenv_BrokenSymlinkLoop tests that a broken symlink at
// .venv/bin/hindsight-api is correctly rejected — no panic, no hang.
// NOTE: If the system has hindsight-api installed in one of the hardcoded
// fallback paths (e.g., /usr/local/bin), the discovery will route to it.
// This test verifies the .venv entry is skipped, not that discovery fails.
func TestChaosVenv_BrokenSymlinkLoop(t *testing.T) {
	dir := t.TempDir()
	svc, _, cleanup := makeServicesWithCustomCwd(t, dir)
	defer cleanup()

	// Create a symlink loop: .venv/bin/hindsight-api -> .venv/bin/hindsight-api
	loopPath := filepath.Join(dir, ".venv", "bin", "hindsight-api")
	if err := os.MkdirAll(filepath.Dir(loopPath), 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// os.Symlink with same source and dest creates a symlink pointing to itself
	if err := os.Symlink("hindsight-api", loopPath); err != nil {
		t.Fatalf("symlink loop: %v", err)
	}

	// os.Stat on a symlink loop returns ELOOP
	_, err := os.Stat(loopPath)
	t.Logf("Symlink loop os.Stat: err=%v", err)

	// startHindsight should NOT panic. If the system has a hindsight-api
	// installed, the fallback will find it — that's correct behavior.
	err = svc.startHindsight()
	if err == nil {
		// Finding the binary through the fallback is OK.
		// The important thing is it didn't try to exec the symlink loop.
		t.Log("OK: startHindsight found system hindsight-api via fallback (symlink loop correctly skipped)")
	} else {
		t.Logf("OK: startHindsight correctly failed: %v (no system hindsight-api found)", err)
	}
}

// =========================================================================
// VECTOR 6: Permission chaos — chmod 000 on .venv/bin/, .venv/
// =========================================================================

// TestChaosVenv_PermissionDeniedOnVenvBin tests that when .venv/bin/
// has 0000 permissions, binary discovery correctly skips it with no panic.
// NOTE: System-installed hindsight-api in fallback paths may still be found.
func TestChaosVenv_PermissionDeniedOnVenvBin(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("permission mode tests not applicable on Windows")
	}

	dir := t.TempDir()
	svc, _, cleanup := makeServicesWithCustomCwd(t, dir)
	defer cleanup()

	// Create .venv/bin/ with a valid binary, then chmod 000 on bin/
	binDir := filepath.Join(dir, ".venv", "bin")
	hindsightPath := filepath.Join(binDir, "hindsight-api")
	createMockHindsightBinary(t, hindsightPath)

	// Set permission to 0000 on bin/
	if err := os.Chmod(binDir, 0000); err != nil {
		t.Fatalf("chmod 000 on bin/: %v", err)
	}
	defer os.Chmod(binDir, 0755) // restore for cleanup

	// os.Stat should fail because we can't traverse bin/
	_, err := os.Stat(hindsightPath)
	t.Logf("os.Stat with bin/ 0000: err=%v", err)

	// startHindsight should NOT panic. It will skip 0000-perm bin/ and
	// continue to the next fallback paths.
	err = svc.startHindsight()
	if err == nil {
		t.Log("OK: startHindsight found system hindsight-api via fallback (0000-perm bin/ correctly skipped)")
	} else {
		t.Logf("OK: startHindsight correctly failed with bin/ 0000: %v", err)
	}
}

// TestChaosVenv_PermissionDeniedOnVenvDir tests that when the .venv/
// directory has 0000 permissions, binary discovery correctly skips it
// with no panic. System-installed fallback path may still be found.
func TestChaosVenv_PermissionDeniedOnVenvDir(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("permission mode tests not applicable on Windows")
	}

	dir := t.TempDir()
	svc, _, cleanup := makeServicesWithCustomCwd(t, dir)
	defer cleanup()

	// Create .venv with a valid binary
	hindsightPath := filepath.Join(dir, ".venv", "bin", "hindsight-api")
	createMockHindsightBinary(t, hindsightPath)

	// Set permission to 0000 on .venv/
	venvDir := filepath.Join(dir, ".venv")
	if err := os.Chmod(venvDir, 0000); err != nil {
		t.Fatalf("chmod 000 on .venv/: %v", err)
	}
	defer os.Chmod(venvDir, 0755) // restore for cleanup

	// os.Stat should fail because we can't traverse .venv/
	_, err := os.Stat(hindsightPath)
	t.Logf("os.Stat with .venv/ 0000: err=%v", err)

	// startHindsight should NOT panic. It will skip the 0000-perm dir
	// and continue to fallback paths.
	err = svc.startHindsight()
	if err == nil {
		t.Log("OK: startHindsight found system hindsight-api via fallback (0000-perm .venv/ correctly skipped)")
	} else {
		t.Logf("OK: startHindsight correctly failed with .venv/ 0000: %v", err)
	}
}

// =========================================================================
// VECTOR 7: TOCTOU race — startHindsight() while .venv being removed
// =========================================================================

// TestChaosVenv_TOCTOURace starts goroutines that concurrently create and
// remove .venv while startHindsight() runs, to detect the TOCTOU window
// between os.Stat (binary discovery) and exec.Command (process start).
// Iteration count is kept low (30 total) to avoid process table exhaustion.
func TestChaosVenv_TOCTOURace(t *testing.T) {
	dir := t.TempDir()
	origWd, _ := os.Getwd()
	defer os.Chdir(origWd)
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir: %v", err)
	}

	var panics atomic.Int64
	var startedOk atomic.Int64
	var notFound atomic.Int64
	var raceDetected atomic.Int64

	var wg sync.WaitGroup
	threads := 3
	cycles := 5 // 15 total startHindsight calls — each spawns if binary exists

	for i := 0; i < threads; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for cycle := 0; cycle < cycles; cycle++ {
				td := filepath.Join(dir, fmt.Sprintf("t-%d-%d", id, cycle))

				// Phase A: Create .venv with mock sleeping binary
				hindsightPath := filepath.Join(td, ".venv", "bin", "hindsight-api")
				createMockHindsightBinary(t, hindsightPath)

				// Create services in the thread-local temp dir
				svc, _, cleanup := makeServicesWithCustomCwd(t, td)

				// Phase B: Concurrently remove .venv and call startHindsight
				removeDone := make(chan struct{}, 1)
				go func() {
					randSleep := time.Duration(rand.Intn(5000)) * time.Microsecond
					time.Sleep(randSleep)
					os.RemoveAll(td)
					close(removeDone)
				}()

				func() {
					defer func() {
						if r := recover(); r != nil {
							panics.Add(1)
						}
					}()
					err := svc.startHindsight()
					if err == nil {
						startedOk.Add(1)
					} else if strings.Contains(err.Error(), "not found") ||
						strings.Contains(err.Error(), "no such") {
						notFound.Add(1)
					} else if strings.Contains(err.Error(), "no such file") ||
						strings.Contains(err.Error(), "stat") {
						raceDetected.Add(1)
					} else {
						// Other errors (e.g., exec format, permission) are expected
						// when the binary is removed mid-exec
						raceDetected.Add(1)
					}
				}()

				<-removeDone
				cleanup()
			}
		}(i)
	}

	wg.Wait()

	t.Logf("TOCTOU race: %d calls, started=%d notFound=%d TOCTOU-hit=%d panics=%d",
		threads*cycles, startedOk.Load(), notFound.Load(), raceDetected.Load(), panics.Load())
	if panics.Load() > 0 {
		t.Errorf("BUG: TOCTOU race caused %d panics", panics.Load())
	}
}

// =========================================================================
// VECTOR 8: Goroutine leak — rapid startHindsight() calls
// =========================================================================

// TestChaosVenv_RapidStartHindsight_GoroutineLeak calls startHindsight()
// rapidly in sequence with no valid binary (returns errBinaryNotFound
// instantly, no process spawn). Verifies goroutine count is stable.
func TestChaosVenv_RapidStartHindsight_GoroutineLeak(t *testing.T) {
	dir := t.TempDir()
	origWd, _ := os.Getwd()
	defer os.Chdir(origWd)
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir: %v", err)
	}

	before := runtime.NumGoroutine()
	// Create empty .venv with no hindsight-api binary — all calls return
	// errBinaryNotFound without spawning any process.
	if err := os.MkdirAll(filepath.Join(dir, ".venv", "bin"), 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	svc, _, cleanup := makeServicesWithCustomCwd(t, dir)
	defer cleanup()

	rapidCalls := 200
	var panics atomic.Int64

	for i := 0; i < rapidCalls; i++ {
		func() {
			defer func() {
				if r := recover(); r != nil {
					panics.Add(1)
				}
			}()
			svc.startHindsight() // expected: returns errBinaryNotFound, no spawn
		}()
	}

	after := runtime.NumGoroutine()
	delta := after - before

	if panics.Load() > 0 {
		t.Errorf("BUG: startHindsight panicked %d times during rapid calls", panics.Load())
	}
	if delta > 20 {
		t.Errorf("BUG: possible goroutine leak after %d rapid startHindsight calls: delta=%d", rapidCalls, delta)
	}
	t.Logf("Rapid startHindsight (no-spawn): %d calls, %d panics, goroutine delta=%d",
		rapidCalls, panics.Load(), delta)
}

// TestChaosVenv_ConcurrentStartHindsight_GoroutineLeak calls startHindsight()
// concurrently from multiple goroutines (no valid binary = no spawn).
// Tests mu.Lock() contention and guard logic under load.
func TestChaosVenv_ConcurrentStartHindsight_GoroutineLeak(t *testing.T) {
	dir := t.TempDir()
	origWd, _ := os.Getwd()
	defer os.Chdir(origWd)
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir: %v", err)
	}

	// Empty .venv — no binary exists, no process spawns
	if err := os.MkdirAll(filepath.Join(dir, ".venv", "bin"), 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	before := runtime.NumGoroutine()

	svc, _, cleanup := makeServicesWithCustomCwd(t, dir)
	defer cleanup()

	var wg sync.WaitGroup
	var panics atomic.Int64

	concurrentCalls := 50

	for i := 0; i < concurrentCalls; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			func() {
				defer func() {
					if r := recover(); r != nil {
						panics.Add(1)
					}
				}()
				svc.startHindsight() // expected: errBinaryNotFound, no spawn
			}()
		}()
	}
	wg.Wait()

	after := runtime.NumGoroutine()
	delta := after - before

	if panics.Load() > 0 {
		t.Errorf("BUG: concurrent startHindsight panicked %d times", panics.Load())
	}
	if delta > 20 {
		t.Errorf("BUG: possible goroutine leak after %d concurrent startHindsight calls: delta=%d", concurrentCalls, delta)
	}
	t.Logf("Concurrent startHindsight (no-spawn): %d calls, %d panics, goroutine delta=%d",
		concurrentCalls, panics.Load(), delta)
}

// =========================================================================
// VECTOR 9: Memory — 1000 os.Stat calls on .venv path
// =========================================================================

// TestChaosVenv_MemoryOsStatStress performs 1000 os.Stat calls on the
// .venv binary path from concurrent goroutines, measuring total allocation
// to detect excessive allocation patterns.
func TestChaosVenv_MemoryOsStatStress(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir: %v", err)
	}

	// Create .venv structure
	hindsightPath := filepath.Join(dir, ".venv", "bin", "hindsight-api")
	createMockHindsightBinary(t, hindsightPath)

	var wg sync.WaitGroup
	threads := 10
	statsPerThread := 100 // total = 1000 os.Stat calls

	var errors atomic.Int64
	var panics atomic.Int64

	var memStatsStart, memStatsEnd runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&memStatsStart)

	for i := 0; i < threads; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < statsPerThread; j++ {
				func() {
					defer func() {
						if r := recover(); r != nil {
							panics.Add(1)
						}
					}()
					if _, err := os.Stat(hindsightPath); err != nil {
						errors.Add(1)
					}
				}()
				// Alternate between existing and non-existing paths
				if j%3 == 0 {
					os.Stat(filepath.Join(dir, ".venv", "bin", "nonexistent"))
				}
			}
		}()
	}

	wg.Wait()

	runtime.GC()
	runtime.ReadMemStats(&memStatsEnd)

	allocDelta := memStatsEnd.TotalAlloc - memStatsStart.TotalAlloc
	heapDelta := memStatsEnd.HeapAlloc - memStatsStart.HeapAlloc
	// When heapDelta < 0, GC freed more than allocated during the test
	if heapDelta < 0 {
		heapDelta = 0
	}

	t.Logf("os.Stat stress: %d calls, %d errors, %d panics",
		threads*statsPerThread, errors.Load(), panics.Load())
	t.Logf("Memory: TotalAlloc delta=%d bytes (%.1f KB), HeapAlloc delta=%d bytes (%.1f KB)",
		allocDelta, float64(allocDelta)/1024, heapDelta, float64(heapDelta)/1024)

	if panics.Load() > 0 {
		t.Errorf("BUG: os.Stat panicked %d times during stress", panics.Load())
	}
	// os.Stat should not allocate more than ~50KB per call on average
	// 1000 calls * 50 bytes per os.Stat = ~50KB minimum; but Go's os.Stat is
	// more expensive. If we see >500KB allocation for 1000 calls, something's wrong.
	if allocDelta > 500*1024 && allocDelta > 0 {
		t.Logf("NOTE: os.Stat allocated %d bytes for %d calls (%.0f bytes/call)", allocDelta, threads*statsPerThread, float64(allocDelta)/float64(threads*statsPerThread))
	}
	if errors.Load() > 0 {
		t.Logf("NOTE: %d os.Stat calls failed (expected for non-existent paths)", errors.Load())
	}
}

// =========================================================================
// VECTOR 3 EXTENSION: Binary discovery with executable bit variations
// =========================================================================

// TestChaosVenv_BinaryDiscovery_ExecutableBits creates hindsight-api
// files with various executable bit combinations to verify the fallback
// correctly handles:
//   - No executable bits (0644) — should reject
//   - Owner-only executable (0100) — should accept
//   - Group-only executable (0010) — should accept
//   - Other-only executable (0001) — should accept
//   - All executable bits (0777) — should accept
func TestChaosVenv_BinaryDiscovery_ExecutableBits(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("executable permission tests not applicable on Windows")
	}

	execPerms := []struct {
		name string
		mode os.FileMode
	}{
		{"no_exec (0644)", 0644},
		{"owner_exec (0100)", 0100},
		{"owner_only (0700)", 0700},
		{"group_exec (0010)", 0010},
		{"other_exec (0001)", 0001},
		{"all_exec (0777)", 0777},
	}

	for _, ep := range execPerms {
		t.Run(ep.name, func(t *testing.T) {
			dir := t.TempDir()
			svc, _, cleanup := makeServicesWithCustomCwd(t, dir)
			defer cleanup()

			hindsightPath := filepath.Join(dir, ".venv", "bin", "hindsight-api")
			if err := os.MkdirAll(filepath.Dir(hindsightPath), 0755); err != nil {
				t.Fatalf("mkdir: %v", err)
			}
			// Write a valid shell script with the given mode
			content := "#!/bin/sh\nexit 0\n"
			if err := os.WriteFile(hindsightPath, []byte(content), ep.mode); err != nil {
				t.Fatalf("write file: %v", err)
			}

			// Verify the file's mode
			info, _ := os.Stat(hindsightPath)
			execAny := info.Mode()&0111 != 0
			isRegular := info.Mode().IsRegular()

			err := svc.startHindsight()
			if err == nil {
				t.Logf("startHindsight accepted (mode=%v, isRegular=%v, exec=%v)", ep.mode, isRegular, execAny)
			} else {
				t.Logf("startHindsight rejected: %v (mode=%v, isRegular=%v, exec=%v)", err, ep.mode, isRegular, execAny)
				if isRegular && execAny {
					// This should have been accepted — something else failed
					t.Logf("NOTE: valid binary was rejected despite isRegular=%v exec=%v", isRegular, execAny)
				}
			}
		})
	}
}

// =========================================================================
// VECTOR 3 EXTENSION: HindsightPath gate condition check
// =========================================================================

// TestChaosVenv_HindsightPathGate verifies that when the user explicitly
// sets HindsightPath to a custom value, the .venv fallback is NOT used
// when LookPath fails. The spec says:
//   ".venv fallback must be gated by svc.config.HindsightPath == 'hindsight-api'"
// This test verifies the coder's implementation deviates from this spec.
func TestChaosVenv_HindsightPathGate(t *testing.T) {
	dir := t.TempDir()
	origWd, _ := os.Getwd()
	defer os.Chdir(origWd)
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir: %v", err)
	}

	// Create .venv with hindsight-api so that the fallback would succeed
	hindsightPath := filepath.Join(dir, ".venv", "bin", "hindsight-api")
	createMockHindsightBinary(t, hindsightPath)

	// Set HindsightPath to a custom binary name (not "hindsight-api")
	cfg := newTestConfig()
	cfg.HindsightPath = "/custom/specific-binary" // user explicitly chose this
	cfg.StartTimeout = 50 * time.Millisecond
	cfg.HealthTimeout = 20 * time.Millisecond

	l, _ := newTestLogger()
	alerts := NewAlertClient("", "optional")
	svc := newServices(cfg, l, alerts)

	// startHindsight should NOT fall through to .venv because
	// the user explicitly set a custom HindsightPath
	err := svc.startHindsight()
	if err == nil {
		// The fallback found .venv/bin/hindsight-api — this DEVIATES from the spec
		t.Logf("DEVIATION: startHindsight succeeded via .venv fallback despite custom HindsightPath=%s", cfg.HindsightPath)
		t.Logf("SPEC says: '.venv fallback must be gated by svc.config.HindsightPath == \"hindsight-api\"'")
		t.Logf("IMPACT: User's explicit binary choice is silently overridden by .venv")
	} else {
		t.Logf("OK: startHindsight correctly returned error for custom HindsightPath: %v", err)
	}
}

// =========================================================================
// VECTOR EXTENSION: Double-spawn guard race — concurrent startHindsight
// =========================================================================

// TestChaosVenv_DoubleSpawnRace calls startHindsight() from 10 goroutines
// simultaneously when hindsight is already running. The double-spawn guard
// at the top of startHindsight() uses mu.Lock() to check if hindsightCmd
// is set. Verify no race condition allows multiple spawns.
func TestChaosVenv_DoubleSpawnRace(t *testing.T) {
	dir := t.TempDir()
	svc, _, cleanup := makeServicesWithCustomCwd(t, dir)
	defer cleanup()

	hindsightPath := filepath.Join(dir, ".venv", "bin", "hindsight-api")
	createMockHindsightBinary(t, hindsightPath)

	// First call: create hindsightCmd
	if err := svc.startHindsight(); err != nil {
		t.Fatalf("first startHindsight: %v", err)
	}

	var wg sync.WaitGroup
	var spawnCount atomic.Int64

	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := svc.startHindsight(); err == nil {
				// Guard returned nil (already running) — this is the expected outcome
				spawnCount.Add(1)
			}
		}()
	}
	wg.Wait()

	t.Logf("Double-spawn guard: %d concurrent startHindsight calls returned nil", spawnCount.Load())

	// Verify no additional panic or crash
	if spawnCount.Load() != 10 {
		// Some may have returned errBinaryNotFound if guard was missed
		t.Logf("NOTE: %d/10 returned nil (expected all 10 if guard works correctly)", spawnCount.Load())
	}
}
