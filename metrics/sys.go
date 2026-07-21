package metrics

import (
	"runtime"
)

// CaptureSysStats returns a snapshot of Go runtime memory and goroutine stats.
func CaptureSysStats() map[string]interface{} {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return map[string]interface{}{
		"sys_alloc_mb":       int64(m.Alloc / 1024 / 1024),
		"sys_total_alloc_mb": int64(m.TotalAlloc / 1024 / 1024),
		"sys_sys_mb":         int64(m.Sys / 1024 / 1024),
		"sys_num_gc":         int64(m.NumGC),
		"sys_gc_pause_ns":    int64(m.PauseNs[(m.NumGC+255)%256]),
		"sys_goroutines":     int64(runtime.NumGoroutine()),
	}
}
