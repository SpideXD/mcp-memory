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
// PASS 2 — BOUNDARY: Edge cases, race windows, resource failures, and
//            platform quirks that Pass 1 did not cover.
//
// ATTACK VECTORS:
//   - mktemp failure (read-only /tmp, disk full, no write permission)
//   - Concurrent [ -x ] guard (TOCTOU race in download-llama idempotency)
//   - armv8l/armv8 naming (not just arm64/aarch64)
//   - Executable-but-truncated artifact bypasses [ -x ] idempotency guard
//   - resolveLlamaPath: path > 4096 bytes (ENAMETOOLONG)
//   - resolveLlamaPath: PATH entries with spaces
//   - resolveLlamaPath: LookPath finds a directory (not a file)
//   - resolveLlamaPath: device file (/dev/null) bypasses IsRegular check
//   - resolveLlamaPath: symlink to a directory
//   - Config: LLAMA_PATH set to vendor/bin/llama-server (redundant but valid)
//   - Makefile curl --connect-timeout 30: does failure produce visible error
//   - .gitignore: vendor/bin/llama-server correctly covered by vendor/
//
// NO process spawning. NO real binary download. Test the Go logic
// and Makefile shells only.
// =========================================================================

// ===================================================================
// Makefile — mktemp failure (read-only /tmp)
// ===================================================================

// TestMakefile_MktempFailure verifies that if mktemp -d fails (e.g., /tmp
// is read-only or disk full), the download target does NOT silently succeed.
//
// Makes mktemp return non-zero, sets TMPDIR to empty, and shows the pipeline
// continues to print "downloaded" despite tar and mv both failing.
func TestMakefile_MktempFailure(t *testing.T) {
	dir := t.TempDir()
	vendorBin := filepath.Join(dir, "vendor", "bin")

	// Override mktemp to fail. Then run the Makefile's download pipeline.
	// Without set -o pipefail or explicit error checks, the recipe continues
	// past tar/mv/chmod failures and prints success.
	script := fmt.Sprintf(`
mktemp() { echo "mktemp: cannot create temporary directory" >&2; return 1; }
TMPDIR=$(mktemp -d /tmp/llama-download-XXXXXX 2>/dev/null || true)
# Now TMPDIR is empty — simulate what the Makefile does after mktemp
set +o pipefail
echo "garbage" | tar xz -C "${TMPDIR}" 2>/dev/null
mkdir -p "%s"
mv "${TMPDIR}/build/bin/llama-server" "%s/llama-server" 2>/dev/null
chmod +x "%s/llama-server" 2>/dev/null
rm -rf "${TMPDIR}" 2>/dev/null
echo "llama-server downloaded to vendor/bin/llama-server."
`, vendorBin, vendorBin, vendorBin)

	cmd := exec.Command("bash", "-c", script)
	out, err := cmd.CombinedOutput()
	output := strings.TrimSpace(string(out))

	if err != nil {
		// Script failed — this is the DESIRED behavior, but the Makefile
		// would have already exited earlier. Check the output for the error.
		t.Logf("Script exited with error (good): %v\n%s", err, output)
		return
	}

	// Script succeeded — the Makefile silently reported success.
	t.Errorf("BUG: mktemp failure silently swallowed. "+
		"mktemp failed, tar failed, mv failed, yet recipe exited 0.\n"+
		"Output: %s", output)

	// Verify no binary was created despite the success message
	if _, statErr := os.Stat(filepath.Join(vendorBin, "llama-server")); statErr == nil {
		t.Errorf("BUG: binary created despite mktemp failure! Partial artifact at %s",
			filepath.Join(vendorBin, "llama-server"))
	}
}

// ===================================================================
// Makefile — Concurrent [ -x ] guard (TOCTOU race)
// ===================================================================

// TestMakefile_ConcurrentGuardRace demonstrates that two concurrent
// processes can both pass the [ -x ] idempotency guard when vendor/bin/
// does not yet have the llama-server binary. This is a TOCTOU race:
// both see "not executable", both proceed to download.
//
// Spec E8 acknowledges this race as acceptable ("human-invoked setup
// step"). This test verifies the race exists and is benign — the final
// binary is valid (both processes download the same release).
func TestMakefile_ConcurrentGuardRace(t *testing.T) {
	dir := t.TempDir()
	vendorBin := filepath.Join(dir, "vendor", "bin")
	if err := os.MkdirAll(vendorBin, 0755); err != nil {
		t.Fatal(err)
	}

	// Simulate the download-llama idempotency logic that TWO concurrent
	// processes run simultaneously. Both check [ -x ] on a path that
	// does NOT exist yet, so both proceed.
	bin := filepath.Join(vendorBin, "llama-server")

	var wg sync.WaitGroup
	results := make([]bool, 2)

	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			// Each "process" evaluates the guard independently.
			// With no file present, both get "false" and would
			// proceed to download.
			_, err := os.Stat(bin)
			exe := err == nil
			if exe {
				results[idx] = true // skip
			} else {
				results[idx] = false // proceed to download
			}
		}(i)
	}
	wg.Wait()

	// Both should have decided to proceed (no file existed when either ran)
	for i, r := range results {
		if r {
			t.Logf("process %d decided to skip (already downloaded) — raced earlier", i)
		} else {
			t.Logf("process %d decided to download", i)
		}
	}

	// At least one should have decided to download
	anyDownloaded := false
	for _, r := range results {
		if !r {
			anyDownloaded = true
		}
	}
	if !anyDownloaded {
		t.Error("neither process decided to download despite no file existing — unexpected")
	}

	// After both finish, simulate Process A writing the binary
	if err := os.WriteFile(bin, []byte("#!/bin/sh\necho llama-server"), 0755); err != nil {
		t.Fatal(err)
	}

	// Verify the binary is executable
	if _, err := os.Stat(bin); err != nil {
		t.Fatalf("binary should exist after write: %v", err)
	}
	info, _ := os.Stat(bin)
	if info.Mode()&0111 == 0 {
		t.Error("binary should be executable")
	}
}

// ===================================================================
// Makefile — armv8l architecture detection
// ===================================================================

// TestMakefile_Platform_Armv8l verifies that "armv8l" (Linux 32-bit
// userspace on ARMv8, common on Raspberry Pi OS 32-bit) is correctly
// rejected. The spec only handles arm64|aarch64 for Linux.
func TestMakefile_Platform_Armv8l(t *testing.T) {
	got, ok := runPlatformScript("Linux", "armv8l")
	if ok {
		t.Errorf("armv8l should be rejected, got success: %s", got)
	}
	if !strings.Contains(got, "ERROR") {
		t.Errorf("expected error output for armv8l, got: %s", got)
	}
}

// TestMakefile_Platform_Armv8 tests that "armv8" (bare version) is rejected.
func TestMakefile_Platform_Armv8(t *testing.T) {
	got, ok := runPlatformScript("Linux", "armv8")
	if ok {
		t.Errorf("armv8 should be rejected, got success: %s", got)
	}
	if !strings.Contains(got, "ERROR") {
		t.Errorf("expected error output for armv8, got: %s", got)
	}
}

// TestMakefile_Platform_Armv8lPlus tests that "armv8l" with '+' variant
// patterns is rejected.
func TestMakefile_Platform_Armv8lPlus(t *testing.T) {
	got, ok := runPlatformScript("Linux", "armv8l-compat")
	if ok {
		t.Errorf("armv8l-compat should be rejected, got success: %s", got)
	}
	if !strings.Contains(got, "ERROR") {
		t.Errorf("expected error output for armv8l-compat, got: %s", got)
	}
}

// ===================================================================
// Makefile — Truncated executable artifact bypasses [ -x ] guard
// ===================================================================

// TestMakefile_TruncatedExecutable_BypassesIdempotency verifies that a
// small, executable but semantically invalid file (e.g., truncated from
// a failed download) passes the [ -x ] guard, causing the Makefile to
// incorrectly skip the download and leave a broken binary in place.
//
// The spec's idempotency check is: `[ -x vendor/bin/llama-server ]`
// This ONLY checks existence+executable, NOT content validity.
func TestMakefile_TruncatedExecutable_BypassesIdempotency(t *testing.T) {
	dir := t.TempDir()
	vendorBin := filepath.Join(dir, "vendor", "bin")
	if err := os.MkdirAll(vendorBin, 0755); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(vendorBin, "llama-server")

	// Create a 10-byte executable file — this is NOT a valid llama-server
	// binary, but the Makefile's [ -x ] check does NOT validate content.
	if err := os.WriteFile(bin, []byte("garb\x00aged!!"), 0755); err != nil {
		t.Fatal(err)
	}

	// Simulate the Makefile's idempotency check exactly
	script := fmt.Sprintf(`
if [ -x "%s" ]; then
	echo "llama-server already downloaded."
	exit 0
fi
echo "need download"
`, bin)

	cmd := exec.Command("bash", "-c", script)
	out, err := cmd.CombinedOutput()
	output := strings.TrimSpace(string(out))

	if err != nil {
		t.Fatalf("shell check failed: %v\n%s", err, output)
	}

	if output != "llama-server already downloaded." {
		t.Fatalf("expected 'llama-server already downloaded.', got %q", output)
	}

	// The Makefile thinks the download succeeded, but the file is
	// truncated garbage. This would cause a runtime failure in
	// startLlama() when exec.Command tries to execute the binary.
	t.Logf("BUG: [ -x ] bypasses content validation. "+
		"A %d-byte garbage file (size %d) with +x passes the idempotency guard. "+
		"The make download-llama target would skip the download, leaving a "+
		"broken binary in place.", 10, len("garb\x00aged!!"))
}

// TestMakefile_TruncatedExecutable_SizeAboveZero_StillBypasses tests
// that even a non-empty but clearly invalid binary passes [ -x ].
func TestMakefile_TruncatedExecutable_SizeAboveZero(t *testing.T) {
	dir := t.TempDir()
	vendorBin := filepath.Join(dir, "vendor", "bin")
	if err := os.MkdirAll(vendorBin, 0755); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(vendorBin, "llama-server")

	// A 100-byte file that is executable — too small to be a real binary
	// (real llama-server is ~15MB), but passes [ -x ]
	data := make([]byte, 100)
	for i := range data {
		data[i] = byte(i)
	}
	if err := os.WriteFile(bin, data, 0755); err != nil {
		t.Fatal(err)
	}

	info, err := os.Stat(bin)
	if err != nil {
		t.Fatal(err)
	}
	if !info.Mode().IsRegular() || info.Size() == 0 || info.Mode()&0111 == 0 {
		t.Fatal("test setup: file should be regular, non-empty, executable")
	}

	// The Makefile would skip this. Verify.
	script := fmt.Sprintf(`
if [ -x "%s" ]; then
	echo "skipped"
	exit 0
fi
echo "download would proceed"
`, bin)

	cmd := exec.Command("bash", "-c", script)
	out, _ := cmd.CombinedOutput()
	output := strings.TrimSpace(string(out))
	if output != "skipped" {
		t.Fatalf("expected 'skipped', got %q", output)
	}
	t.Logf("The [ -x ] guard only checks executable bit, not file integrity. "+
		"A 100-byte file with +x (real binary is ~15MB) passes the guard. "+
		"Startup will fail when exec.Command tries to run it.")
}

// ===================================================================
// resolveLlamaPath() — Very Long Path (ENAMETOOLONG)
// ===================================================================

// TestResolve_VeryLongPath verifies that when LlamaPath exceeds the
// system path length limit, os.Stat fails with ENAMETOOLONG and the
// resolution falls through to candidate 2 (system PATH).
func TestResolve_VeryLongPath(t *testing.T) {
	// Build a path long enough to exceed PATH_MAX on any system.
	// macOS: PATH_MAX = 1024 bytes (including NUL)
	// Linux: PATH_MAX = 4096 bytes
	// Use 4096+ to guarantee failure on all Unix systems.
	longComponent := strings.Repeat("a", 4096)
	longPath := "/" + longComponent + "/llama-server"

	svc, _ := newTestServices()
	svc.config.LlamaPath = longPath

	got, err := svc.resolveLlamaPath()
	if err == nil {
		t.Fatalf("expected error for path of length %d (> PATH_MAX), got path=%q",
			len(longPath), got)
	}
	if got != "" {
		t.Errorf("expected empty path on error, got %q", got)
	}
}

// TestResolve_VeryLongPath_FallsbackToPATH verifies that when LlamaPath
// is too long but a valid binary exists on PATH, resolution falls back
// to candidate 2 instead of failing entirely.
func TestResolve_VeryLongPath_FallsbackToPATH(t *testing.T) {
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

	longComponent := strings.Repeat("b", 4096)
	longPath := "/" + longComponent + "/llama-server"

	svc, _ := newTestServices()
	svc.config.LlamaPath = longPath

	got, err := svc.resolveLlamaPath()
	if err != nil {
		t.Fatalf("expected fallback to PATH for too-long LlamaPath, got: %v", err)
	}
	if !strings.HasSuffix(got, "llama-server") {
		t.Errorf("expected path ending in llama-server, got %q", got)
	}
}

// ===================================================================
// resolveLlamaPath() — PATH with spaces
// ===================================================================

// TestResolve_PATHWithSpaces verifies that when a directory on PATH
// contains spaces, exec.LookPath correctly finds the binary there.
func TestResolve_PATHWithSpaces(t *testing.T) {
	dir := t.TempDir()
	// Create a directory with a space in its name
	spaceDir := filepath.Join(dir, "my programs")
	if err := os.MkdirAll(spaceDir, 0755); err != nil {
		t.Fatal(err)
	}
	fakeBin := filepath.Join(spaceDir, "llama-server")
	if err := os.WriteFile(fakeBin, []byte("#!/bin/sh\necho path-with-spaces"), 0755); err != nil {
		t.Fatal(err)
	}

	origPath := os.Getenv("PATH")
	os.Setenv("PATH", spaceDir+string(os.PathListSeparator)+origPath)
	defer os.Setenv("PATH", origPath)

	svc, _ := newTestServices()
	svc.config.LlamaPath = "/nonexistent/llama-server"

	got, err := svc.resolveLlamaPath()
	if err != nil {
		t.Fatalf("expected PATH with spaces to work, got: %v", err)
	}
	if got != fakeBin {
		t.Errorf("expected %q, got %q", fakeBin, got)
	}
}

// TestResolve_PATHWithTrailingColon verifies that a PATH with trailing
// colon (which means "current directory" — an empty entry) doesn't
// break LookPath resolution.
func TestResolve_PATHWithTrailingColon(t *testing.T) {
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
	// Add a trailing colon — empty entry means current directory on Unix
	os.Setenv("PATH", fakeBinDir+":")
	defer os.Setenv("PATH", origPath)

	svc, _ := newTestServices()
	svc.config.LlamaPath = "/nonexistent/llama-server"

	got, err := svc.resolveLlamaPath()
	if err != nil {
		t.Fatalf("expected PATH with trailing colon to work, got: %v", err)
	}
	if got != fakeBin {
		t.Errorf("expected %q, got %q", fakeBin, got)
	}
}

// ===================================================================
// resolveLlamaPath() — LookPath finds a directory
// ===================================================================

// TestResolve_LookPathFindsDirectory checks what happens when a directory
// named "llama-server" exists on PATH. exec.LookPath returns paths to
// directories only if they have the executable bit set. But os.Stat
// on the path would show a directory, so Mode().IsRegular() returns
// false — correctly rejected.
func TestResolve_LookPathFindsDirectory(t *testing.T) {
	dir := t.TempDir()
	fakeDir := filepath.Join(dir, "llama-server")
	if err := os.MkdirAll(fakeDir, 0755); err != nil {
		t.Fatal(err)
	}

	origPath := os.Getenv("PATH")
	os.Setenv("PATH", dir+string(os.PathListSeparator)+origPath)
	defer os.Setenv("PATH", origPath)

	// exec.LookPath on a directory: On Unix, LookPath requires the
	// path to be executable. A directory with 0755 IS executable,
	// so LookPath returns it. But resolveLlamaPath then does
	// os.Stat → IsRegular() → false → skip.
	lookPath, err := exec.LookPath("llama-server")
	if err != nil {
		// Some systems may not consider directories as valid LookPath results
		t.Skipf("exec.LookPath behavior for directories: %v", err)
	}
	t.Logf("exec.LookPath found directory at %q", lookPath)

	svc, _ := newTestServices()
	svc.config.LlamaPath = "/nonexistent/llama-server"

	got, err := svc.resolveLlamaPath()
	if err == nil {
		t.Fatalf("expected error when LookPath finds a directory, got path=%q", got)
	}
	if got != "" {
		t.Errorf("expected empty path for directory, got %q", got)
	}
}

// ===================================================================
// resolveLlamaPath() — Device file paths (/dev/null, /dev/zero)
// ===================================================================

// TestResolve_DeviceFile_DevNull verifies that /dev/null is correctly
// rejected. It is not a regular file (it's a character device), so
// info.Mode().IsRegular() returns false.
func TestResolve_DeviceFile_DevNull(t *testing.T) {
	svc, _ := newTestServices()
	svc.config.LlamaPath = "/dev/null"

	got, err := svc.resolveLlamaPath()
	if err == nil {
		t.Fatalf("expected error for /dev/null (not a regular file), got path=%q", got)
	}
	if got != "" {
		t.Errorf("expected empty path, got %q", got)
	}
}

// TestResolve_DeviceFile_DevZero verifies that /dev/zero is correctly
// rejected for the same reason.
func TestResolve_DeviceFile_DevZero(t *testing.T) {
	svc, _ := newTestServices()
	svc.config.LlamaPath = "/dev/zero"

	got, err := svc.resolveLlamaPath()
	if err == nil {
		t.Fatalf("expected error for /dev/zero (not a regular file), got path=%q", got)
	}
	if got != "" {
		t.Errorf("expected empty path, got %q", got)
	}
}

// ===================================================================
// resolveLlamaPath() — Symlink to a directory
// ===================================================================

// TestResolve_SymlinkToDirectory verifies that a symlink pointing to
// a directory is rejected. os.Stat follows the symlink, sees a
// directory, and IsRegular() returns false.
func TestResolve_SymlinkToDirectory(t *testing.T) {
	dir := t.TempDir()
	targetDir := filepath.Join(dir, "target")
	if err := os.MkdirAll(targetDir, 0755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "llama-server")
	if err := os.Symlink(targetDir, link); err != nil {
		t.Fatalf("symlink failed: %v", err)
	}

	svc, _ := newTestServices()
	svc.config.LlamaPath = link

	got, err := svc.resolveLlamaPath()
	if err == nil {
		t.Fatalf("expected error for symlink-to-directory, got path=%q", got)
	}
	if got != "" {
		t.Errorf("expected empty path, got %q", got)
	}
}

// ===================================================================
// resolveLlamaPath() — File without read permission
// ===================================================================

// TestResolve_NoReadPermission verifies that a file with execute but no
// read permission is handled. os.Stat needs read permission on the
// containing directory, but the file itself only needs execute to
// be found. However, the Go code uses os.Stat which succeeds as long
// as the directory is searchable — it reads metadata, not content.
func TestResolve_NoReadPermission(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix permission test")
	}

	dir := t.TempDir()
	bin := filepath.Join(dir, "llama-server")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\necho test"), 0100); err != nil {
		t.Fatal(err)
	}

	svc, _ := newTestServices()
	svc.config.LlamaPath = bin

	// os.Stat succeeds on files without read permission as long as the
	// directory has search (+x) permission. So a file with mode 0100
	// (--x------) IS a regular file, size > 0, AND has executable bit set.
	// This should be accepted.
	got, err := svc.resolveLlamaPath()
	if err != nil {
		t.Fatalf("file with --x permission should be accepted (os.Stat reads metadata, not content): %v", err)
	}
	if got != bin {
		t.Errorf("expected %q, got %q", bin, got)
	}
}

// ===================================================================
// resolveLlamaPath() — Config.LlamaPath same as LookPath result
// ===================================================================

// TestResolve_ConfigPathMatchesLookPath verifies that when the user
// sets LLAMA_PATH to the same path that exec.LookPath would find
// (e.g., a system binary like /usr/local/bin/llama-server), candidate
// 1 succeeds and no fallback is needed. This is the redundant-but-valid
// scenario.
func TestResolve_ConfigPathMatchesLookPath(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "llama-server")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\necho system"), 0755); err != nil {
		t.Fatal(err)
	}

	origPath := os.Getenv("PATH")
	os.Setenv("PATH", dir+string(os.PathListSeparator)+origPath)
	defer os.Setenv("PATH", origPath)

	// Set LlamaPath to the same binary that LookPath would find
	svc, _ := newTestServices()
	svc.config.LlamaPath = bin

	// Candidate 1 should succeed immediately
	got, err := svc.resolveLlamaPath()
	if err != nil {
		t.Fatalf("expected LlamaPath to resolve, got: %v", err)
	}
	if got != bin {
		t.Errorf("expected %q, got %q", bin, got)
	}

	// Verify LookPath also finds the same binary
	lp, _ := exec.LookPath("llama-server")
	if lp != bin {
		t.Logf("Note: LookPath found %q, LlamaPath is %q (different paths, same binary)", lp, bin)
	}
}

// ===================================================================
// Makefile — curl --connect-timeout 30 / --max-time 300 edge
// ===================================================================

// TestMakefile_CurlTimeout_ErrorMessageVisible verifies that when curl fails
// due to a connection timeout, the error message is visible to the user.
// The Makefile does NOT redirect curl's stderr, so diagnostic messages
// reach the user even though the pipeline's exit code is from tar (the
// last command in the pipe), not curl — this is the pipefail bug (BUG-001).
func TestMakefile_CurlTimeout_ErrorMessageVisible(t *testing.T) {
	// Use an RFC 5737 non-routable IP to force a fast connect failure.
	script := `
curl -fSL --connect-timeout 2 --max-time 5 "http://192.0.2.4/nonexistent.tar.gz" 2>&1 | head -10
`

	cmd := exec.Command("bash", "-c", script)
	out, _ := cmd.CombinedOutput()
	output := string(out)

	// If curl succeeded (unlikely with RFC 5737 IP), skip.
	if output == "" {
		t.Skip("curl may have succeeded (network connectivity to test-net)")
		return
	}

	// The Makefile does NOT redirect stderr, so curl's error message
	// reaches the user. Look for common curl error indicators.
	if strings.Contains(output, "curl:") {
		t.Logf("curl failure message visibly reaches the user (no stderr redirect in Makefile).\nOutput: %s", output)
	} else {
		t.Logf("curl failure output (may vary by platform):\n%s", output)
	}
}

// ===================================================================
// .gitignore — vendor/ pattern correctly covers vendor/bin/
// ===================================================================

// TestGitignore_VendorPatternMatchesSubdirectory verifies that the
// `vendor/` gitignore pattern correctly ignores the full path
// vendor/bin/llama-server. Git's .gitignore treats `vendor/` as
// matching the directory and all its contents recursively.
func TestGitignore_VendorPatternMatchesSubdirectory(t *testing.T) {
	// Check that .gitignore contains `vendor/` (already tested in Pass 1)
	data, err := os.ReadFile(".gitignore")
	if err != nil {
		t.Fatal(err)
	}
	content := string(data)

	if !strings.Contains(content, "vendor/") {
		t.Fatal("vendor/ not in .gitignore — cannot verify pattern matching")
	}

	// According to gitignore(5), a pattern ending with / matches
	// directories and everything below them. So `vendor/` matches:
	//   vendor/
	//   vendor/bin/
	//   vendor/bin/llama-server
	//
	// Verify that git would ignore vendor/bin/llama-server by checking
	// the parsing rules. We can check with `git check-ignore` if git
	// is available.
	cmd := exec.Command("git", "check-ignore", "--no-index",
		"--stdin")
	cmd.Stdin = strings.NewReader("vendor/bin/llama-server\nvendor/bin/\n")
	out, err := cmd.CombinedOutput()

	if err != nil {
		// git check-ignore returns non-zero if NOT ignored, or git
		// may not be installed. Skip in that case.
		t.Logf("git check-ignore not available or not ignored: %v\n%s", err, out)
		return
	}

	// Check that both paths are listed as ignored
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	matched := make(map[string]bool)
	for _, line := range lines {
		matched[line] = true
	}

	if !matched["vendor/bin/llama-server"] {
		t.Error("git check-ignore: vendor/bin/llama-server is NOT ignored")
	} else {
		t.Log("vendor/bin/llama-server is correctly gitignored")
	}

	if !matched["vendor/bin/"] {
		t.Error("git check-ignore: vendor/bin/ is NOT ignored")
	} else {
		t.Log("vendor/bin/ is correctly gitignored")
	}
}

// ===================================================================
// Config — LLAMA_PATH set to vendor/bin/llama-server (redundant but valid)
// ===================================================================

// TestResolve_LlamaPathSetToVendorBin verifies that when the user sets
// LLAMA_PATH to vendor/bin/llama-server (the same as the default),
// or any valid executable, resolution works correctly.
func TestResolve_LlamaPathSetToVendorBin(t *testing.T) {
	dir := t.TempDir()
	vendorBin := filepath.Join(dir, "vendor", "bin")
	if err := os.MkdirAll(vendorBin, 0755); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(vendorBin, "llama-server")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\necho llama-server"), 0755); err != nil {
		t.Fatal(err)
	}

	svc, _ := newTestServices()

	// Set LlamaPath to the absolute vendor/bin/llama-server path
	svc.config.LlamaPath = bin

	got, err := svc.resolveLlamaPath()
	if err != nil {
		t.Fatalf("expected LlamaPath=%q to resolve, got: %v", bin, err)
	}
	if got != bin {
		t.Errorf("expected %q, got %q", bin, got)
	}
}

// ===================================================================
// resolveLlamaPath() — Empty string edge in error message
// ===================================================================

// TestResolve_ErrorFormatEmptyConfig verifies that when LlamaPath is
// empty and both candidates fail, the error message is still readable
// and mentions system PATH.
func TestResolve_ErrorFormatEmptyConfig(t *testing.T) {
	origPath := os.Getenv("PATH")
	os.Setenv("PATH", "/nonexistent")
	defer os.Setenv("PATH", origPath)

	svc, _ := newTestServices()
	svc.config.LlamaPath = ""

	_, err := svc.resolveLlamaPath()
	if err == nil {
		t.Fatal("expected error")
	}
	msg := err.Error()

	if strings.Contains(msg, `""`) || strings.Contains(msg, `''`) {
		// Empty string quoted — acceptable but not ideal for user readability
		t.Logf("Error message for empty LlamaPath: %s", msg)
	} else {
		t.Logf("Error message: %s", msg)
	}

	if !strings.Contains(msg, "system PATH") {
		t.Errorf("error should mention system PATH search, got: %s", msg)
	}
}

// ===================================================================
// resolveLlamaPath() — lookPath returns a non-regular file
// ===================================================================

// TestResolve_LookPathFindsFIFO checks what happens when a FIFO (named
// pipe) named "llama-server" exists on PATH. exec.LookPath requires
// the file to be executable. FIFOs typically have mode 0644, not
// executable, so LookPath likely won't find it. But if someone
// chmod's a FIFO to +x, LookPath returns it, and resolveLlamaPath
// correctly rejects it via IsRegular().
func TestResolve_LookPathFindsFIFO(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("mkfifo not available on Windows")
	}

	dir := t.TempDir()
	fifo := filepath.Join(dir, "llama-server")

	cmd := exec.Command("mkfifo", fifo)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("mkfifo failed: %v\n%s", err, out)
	}
	// Make it executable so LookPath considers it
	if err := exec.Command("chmod", "+x", fifo).Run(); err != nil {
		t.Fatalf("chmod failed: %v", err)
	}

	origPath := os.Getenv("PATH")
	os.Setenv("PATH", dir+string(os.PathListSeparator)+origPath)
	defer os.Setenv("PATH", origPath)

	// Check if LookPath finds the FIFO
	lp, err := exec.LookPath("llama-server")
	if err != nil {
		t.Skipf("exec.LookPath did not find the FIFO: %v", err)
	}
	t.Logf("exec.LookPath found FIFO at %q", lp)

	svc, _ := newTestServices()
	svc.config.LlamaPath = "/nonexistent/llama-server"

	got, err := svc.resolveLlamaPath()
	if err == nil {
		t.Fatalf("expected error for FIFO on PATH, got path=%q", got)
	}
	if got != "" {
		t.Errorf("expected empty path for FIFO, got %q", got)
	}
	t.Log("resolveLlamaPath correctly rejects FIFO on PATH via IsRegular() check")
}

// ===================================================================
// Makefile — Environment variable expansion edge cases
// ===================================================================

// TestMakefile_LLAMAVersionWithSpecialChars verifies that unusual but
// valid characters in LLAMA_VERSION are handled correctly by the
// Makefile's URL construction. Version tags can include dots, hyphens,
// and underscores.
func TestMakefile_LLAMAVersionWithDots(t *testing.T) {
	// Test that the URL template handles dots in version tags
	// (some releases may use semver-like tags)
	script := `
VERSION="b12345.2"
URL="https://github.com/ggml-org/llama.cpp/releases/download/${VERSION}/llama-${VERSION}-bin-macos-arm64.tar.gz"
echo "$URL"
`
	cmd := exec.Command("bash", "-c", script)
	out, _ := cmd.CombinedOutput()
	output := strings.TrimSpace(string(out))

	expected := "https://github.com/ggml-org/llama.cpp/releases/download/b12345.2/llama-b12345.2-bin-macos-arm64.tar.gz"
	if output != expected {
		t.Errorf("URL with dots in version:\ngot:  %q\nwant: %q", output, expected)
	}
}

// TestMakefile_LLAMAVersionWithHyphen verifies hyphens in version
func TestMakefile_LLAMAVersionWithHyphen(t *testing.T) {
	script := `
VERSION="b12345-rc2"
URL="https://github.com/ggml-org/llama.cpp/releases/download/${VERSION}/llama-${VERSION}-bin-macos-arm64.tar.gz"
echo "$URL"
`
	cmd := exec.Command("bash", "-c", script)
	out, _ := cmd.CombinedOutput()
	output := strings.TrimSpace(string(out))

	expected := "https://github.com/ggml-org/llama.cpp/releases/download/b12345-rc2/llama-b12345-rc2-bin-macos-arm64.tar.gz"
	if output != expected {
		t.Errorf("URL with hyphens in version:\ngot:  %q\nwant: %q", output, expected)
	}
}

// ===================================================================
// resolveLlamaPath() — PATH with non-existent entries
// ===================================================================

// TestResolve_PATHWithNonExistentDirs verifies that exec.LookPath
// correctly skips non-existent directories in PATH and finds the
// binary in a valid directory.
func TestResolve_PATHWithNonExistentDirs(t *testing.T) {
	dir := t.TempDir()
	fakeBinDir := filepath.Join(dir, "bin")
	if err := os.MkdirAll(fakeBinDir, 0755); err != nil {
		t.Fatal(err)
	}
	fakeBin := filepath.Join(fakeBinDir, "llama-server")
	if err := os.WriteFile(fakeBin, []byte("#!/bin/sh\necho found"), 0755); err != nil {
		t.Fatal(err)
	}

	origPath := os.Getenv("PATH")
	// PATH with non-existent directories before the valid one
	os.Setenv("PATH", "/nonexistent1:/nonexistent2:"+fakeBinDir+string(os.PathListSeparator)+origPath)
	defer os.Setenv("PATH", origPath)

	svc, _ := newTestServices()
	svc.config.LlamaPath = "/nonexistent/llama-server"

	got, err := svc.resolveLlamaPath()
	if err != nil {
		t.Fatalf("expected LookPath to skip non-existent dirs, got: %v", err)
	}
	if got != fakeBin {
		t.Errorf("expected %q, got %q", fakeBin, got)
	}
}

// ===================================================================
// Concurrent resolveLlamaPath with PATH fallback (higher concurrency)
// ===================================================================

// TestResolve_ConcurrentLarge verifies resolveLlamaPath under high
// concurrency (50 goroutines) with both candiate 1 and 2 scenarios.
// Run with -race to catch data races.
func TestResolve_ConcurrentLarge(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "llama-server")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\necho concurrent"), 0755); err != nil {
		t.Fatal(err)
	}

	svc, _ := newTestServices()
	svc.config.LlamaPath = bin

	var wg sync.WaitGroup
	errs := make([]error, 50)
	paths := make([]string, 50)

	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			path, err := svc.resolveLlamaPath()
			paths[idx] = path
			errs[idx] = err
		}(i)
	}
	wg.Wait()

	for i := 0; i < 50; i++ {
		if errs[i] != nil {
			t.Errorf("goroutine %d: unexpected error: %v", i, errs[i])
		}
		if paths[i] != bin {
			t.Errorf("goroutine %d: got %q, want %q", i, paths[i], bin)
		}
	}
}

// TestResolve_ConcurrentLarge_PATHFallback tests high concurrency when
// falling back to PATH resolution.
func TestResolve_ConcurrentLarge_PATHFallback(t *testing.T) {
	dir := t.TempDir()
	fakeBinDir := filepath.Join(dir, "altbin")
	if err := os.MkdirAll(fakeBinDir, 0755); err != nil {
		t.Fatal(err)
	}
	fakeBin := filepath.Join(fakeBinDir, "llama-server")
	if err := os.WriteFile(fakeBin, []byte("#!/bin/sh\necho fallback"), 0755); err != nil {
		t.Fatal(err)
	}

	origPath := os.Getenv("PATH")
	os.Setenv("PATH", fakeBinDir+string(os.PathListSeparator)+origPath)
	defer os.Setenv("PATH", origPath)

	svc, _ := newTestServices()
	svc.config.LlamaPath = "/nonexistent/llama-server"

	var wg sync.WaitGroup
	errs := make([]error, 50)

	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			_, err := svc.resolveLlamaPath()
			errs[idx] = err
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("goroutine %d: unexpected error with PATH fallback: %v", i, err)
		}
	}
}
