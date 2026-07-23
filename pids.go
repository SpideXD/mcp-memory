package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// pidFile tracks child process PIDs for orphan cleanup after crash.
// Written to {workingDir}/.mcp-pids.json
func (svc *services) savePids() {
	svc.mu.Lock()
	defer svc.mu.Unlock()

	pids := map[string]int{}

	if svc.llamaCmd != nil && svc.llamaCmd.Process != nil {
		pids["llama"] = svc.llamaCmd.Process.Pid
	}
	if svc.llamaRerankerCmd != nil && svc.llamaRerankerCmd.Process != nil {
		pids["llama_reranker"] = svc.llamaRerankerCmd.Process.Pid
	}
	if svc.hindsightCmd != nil && svc.hindsightCmd.Process != nil {
		pids["hindsight"] = svc.hindsightCmd.Process.Pid
	}
	if svc.cogneeCmd != nil && svc.cogneeCmd.Process != nil {
		pids["cognee"] = svc.cogneeCmd.Process.Pid
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

// cleanupOrphans reads the PID file from a previous crash and kills
// any orphaned child processes that survived.
func cleanupOrphans() {
	wd, _ := os.Getwd()
	path := filepath.Join(wd, "logs/.mcp-pids.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return // No PID file — fresh start
	}

	var pids map[string]int
	if err := json.Unmarshal(data, &pids); err != nil {
		os.Remove(path)
		return
	}

	for name, pid := range pids {
		if pid <= 0 {
			continue
		}
		// Check if process is still alive
		proc, err := os.FindProcess(pid)
		if err != nil {
			continue
		}
		// Signal(0) checks if process exists without sending a signal
		if err := proc.Signal(os.Signal(nil)); err == nil {
			fmt.Fprintf(os.Stderr, "mcp-memory: killing orphaned %s (PID: %d)\n", name, pid)
			proc.Signal(os.Interrupt)
			// Wait for graceful exit with timeout before spawning services
			done := make(chan struct{})
			go func() { proc.Wait(); close(done) }()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				proc.Kill()
			}
		}
	}

	os.Remove(path)
}
