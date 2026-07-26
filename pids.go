package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// pidEntry records a child process so a later run can identify it. The command
// name is stored alongside the PID because PIDs are recycled: after a crash and
// a reboot-free interval, the recorded PID may belong to something else
// entirely, and signalling it would kill an unrelated process.
type pidEntry struct {
	PID  int    `json:"pid"`
	Name string `json:"name"`
}

// processAlive reports whether proc names a live process, using a zero-signal
// probe. It must be syscall.Signal(0): os.Process.Signal type-asserts its
// argument to syscall.Signal, so a nil os.Signal always fails with
// "os: unsupported signal type" whether or not the process exists — which is
// what made the original orphan cleanup a silent no-op.
func processAlive(proc *os.Process) bool {
	return proc.Signal(syscall.Signal(0)) == nil
}

// processName returns the executable name for pid, or "" if it cannot be
// determined. `ps -p <pid> -o comm=` works on both darwin and linux.
func processName(pid int) string {
	out, err := exec.Command("ps", "-p", fmt.Sprint(pid), "-o", "comm=").Output()
	if err != nil {
		return ""
	}
	return filepath.Base(strings.TrimSpace(string(out)))
}

// pidFile tracks child process PIDs for orphan cleanup after crash.
// Written to {workingDir}/logs/.mcp-pids.json
func (svc *services) savePids() {
	svc.mu.Lock()
	defer svc.mu.Unlock()

	pids := map[string]pidEntry{}

	if svc.llamaCmd != nil && svc.llamaCmd.Process != nil {
		pid := svc.llamaCmd.Process.Pid
		pids["llama"] = pidEntry{PID: pid, Name: processName(pid)}
	}

	if svc.cogneeCmd != nil && svc.cogneeCmd.Process != nil {
		pid := svc.cogneeCmd.Process.Pid
		pids["cognee"] = pidEntry{PID: pid, Name: processName(pid)}
	}

	if len(pids) == 0 {
		return
	}

	path := filepath.Join(svc.workingDir(), "logs/.mcp-pids.json")
	tmpPath := path + ".tmp"
	data, _ := json.Marshal(pids)
	os.WriteFile(tmpPath, data, 0644)
	os.Rename(tmpPath, path) // Atomic — no corrupted file on crash
}

func (svc *services) clearPids() {
	os.Remove(filepath.Join(svc.workingDir(), "logs/.mcp-pids.json"))
}

func (svc *services) workingDir() string {
	wd, _ := os.Getwd()
	return wd
}

// parsePidFile decodes the PID file, accepting both the current
// {"llama":{"pid":N,"name":"..."}} form and the legacy {"llama":N} form.
// Legacy entries carry no name, so they cannot be identity-checked.
func parsePidFile(data []byte) (map[string]pidEntry, bool) {
	var entries map[string]pidEntry
	if err := json.Unmarshal(data, &entries); err == nil {
		valid := false
		for _, e := range entries {
			if e.PID > 0 {
				valid = true
				break
			}
		}
		if valid {
			return entries, true
		}
	}

	var legacy map[string]int
	if err := json.Unmarshal(data, &legacy); err == nil {
		out := make(map[string]pidEntry, len(legacy))
		for k, pid := range legacy {
			out[k] = pidEntry{PID: pid}
		}
		return out, true
	}
	return nil, false
}

// cleanupOrphans reads the PID file left by a previous crash and kills any
// child process that survived it.
//
// A recorded PID is only signalled when the live process still carries the same
// executable name. Without that check a recycled PID would send SIGINT, then
// SIGKILL, to an unrelated process — the PID file outlives the crash precisely
// so it can be read long afterwards, which is when recycling is most likely.
// Entries with no recorded name (legacy file format) are skipped rather than
// killed on faith.
func cleanupOrphans() {
	wd, _ := os.Getwd()
	path := filepath.Join(wd, "logs/.mcp-pids.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return // No PID file — fresh start
	}
	defer os.Remove(path)

	entries, ok := parsePidFile(data)
	if !ok {
		return
	}

	for label, entry := range entries {
		if entry.PID <= 0 {
			continue
		}
		proc, err := os.FindProcess(entry.PID)
		if err != nil {
			continue
		}
		if !processAlive(proc) {
			continue
		}

		// Identity check: the PID must still be the process we started.
		actual := processName(entry.PID)
		if entry.Name == "" {
			fmt.Fprintf(os.Stderr,
				"mcp-memory: PID file has no recorded name for %s (PID %d, now %q) — skipping\n",
				label, entry.PID, actual)
			continue
		}
		if actual != entry.Name {
			fmt.Fprintf(os.Stderr,
				"mcp-memory: PID %d was %s, is now %q — recycled, not killing\n",
				entry.PID, entry.Name, actual)
			continue
		}

		fmt.Fprintf(os.Stderr, "mcp-memory: killing orphaned %s %q (PID %d)\n",
			label, entry.Name, entry.PID)
		proc.Signal(os.Interrupt)

		// Poll for exit. proc.Wait() cannot be used: on Unix only a parent may
		// wait on a process, and an orphan's parent is init — Wait returns an
		// error immediately and we would move on without confirming death.
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if !processAlive(proc) {
				break
			}
			time.Sleep(50 * time.Millisecond)
		}
		if processAlive(proc) {
			proc.Kill()
		}
	}
}
