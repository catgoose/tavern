package tavern

import (
	"fmt"
	"runtime"
	"runtime/pprof"
	"time"
)

// SystemStats holds Go runtime metrics for the SSE system dashboard.
type SystemStats struct {
	Timestamp       string
	GoVersion       string
	OS              string
	Arch            string
	Uptime          string
	HeapIdleMB      float64
	TotalAllocMB    float64
	Goroutines      int
	HeapAllocMB     float64
	HeapSysMB       float64
	NumCPU          int
	HeapReleasedMB  float64
	StackInUseMB    float64
	SysMB           float64
	NumThread       int
	LiveObjects     uint64
	LastPauseMicros uint64
	NextGCMB        float64
	HeapObjects     uint64
	Mallocs         uint64
	Frees           uint64
	GCCycles        uint32
}

// CollectRuntimeStats samples the current Go runtime metrics.
// start is the time the process was started (used to compute Uptime).
func CollectRuntimeStats(start time.Time) SystemStats {
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)

	numThread := 0
	if p := pprof.Lookup("threadcreate"); p != nil {
		numThread = p.Count()
	}

	return SystemStats{
		// Runtime
		Uptime:     formatDuration(time.Since(start)),
		GoVersion:  runtime.Version(),
		OS:         runtime.GOOS,
		Arch:       runtime.GOARCH,
		NumCPU:     runtime.NumCPU(),
		Goroutines: runtime.NumGoroutine(),
		NumThread:  numThread,
		Timestamp:  time.Now().Format("15:04:05"),

		// Memory
		HeapAllocMB:    float64(ms.HeapAlloc) / 1024 / 1024,
		HeapSysMB:      float64(ms.HeapSys) / 1024 / 1024,
		HeapIdleMB:     float64(ms.HeapIdle) / 1024 / 1024,
		HeapReleasedMB: float64(ms.HeapReleased) / 1024 / 1024,
		StackInUseMB:   float64(ms.StackInuse) / 1024 / 1024,
		SysMB:          float64(ms.Sys) / 1024 / 1024,
		TotalAllocMB:   float64(ms.TotalAlloc) / 1024 / 1024,

		// GC
		GCCycles:        ms.NumGC,
		LastPauseMicros: ms.PauseNs[(ms.NumGC+255)%256] / 1000,
		NextGCMB:        float64(ms.NextGC) / 1024 / 1024,

		// Allocator
		HeapObjects: ms.HeapObjects,
		Mallocs:     ms.Mallocs,
		Frees:       ms.Frees,
		LiveObjects: ms.Mallocs - ms.Frees,
	}
}

func formatDuration(d time.Duration) string {
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	s := int(d.Seconds()) % 60
	if h > 0 {
		return fmt.Sprintf("%dh %dm %ds", h, m, s)
	}
	if m > 0 {
		return fmt.Sprintf("%dm %ds", m, s)
	}
	return fmt.Sprintf("%ds", s)
}
