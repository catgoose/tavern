package tavern

import (
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCollectRuntimeStats_Fields(t *testing.T) {
	start := time.Now().Add(-5 * time.Second)
	s := CollectRuntimeStats(start)

	assert.Equal(t, runtime.Version(), s.GoVersion)
	assert.Equal(t, runtime.GOOS, s.OS)
	assert.Equal(t, runtime.GOARCH, s.Arch)
	assert.Equal(t, runtime.NumCPU(), s.NumCPU)
	assert.Greater(t, s.Goroutines, 0)
	assert.Greater(t, s.NumThread, 0)

	// Uptime should reflect ~5 seconds.
	assert.NotEmpty(t, s.Uptime)

	// Timestamp formatted as HH:MM:SS.
	assert.Regexp(t, `^\d{2}:\d{2}:\d{2}$`, s.Timestamp)

	// Memory fields should be non-negative.
	assert.GreaterOrEqual(t, s.HeapAllocMB, 0.0)
	assert.GreaterOrEqual(t, s.HeapSysMB, 0.0)
	assert.GreaterOrEqual(t, s.HeapIdleMB, 0.0)
	assert.GreaterOrEqual(t, s.HeapReleasedMB, 0.0)
	assert.GreaterOrEqual(t, s.StackInUseMB, 0.0)
	assert.GreaterOrEqual(t, s.SysMB, 0.0)
	assert.GreaterOrEqual(t, s.TotalAllocMB, 0.0)
	assert.GreaterOrEqual(t, s.NextGCMB, 0.0)

	// Allocator counters: Mallocs >= Frees, LiveObjects = Mallocs - Frees.
	require.GreaterOrEqual(t, s.Mallocs, s.Frees)
	assert.Equal(t, s.Mallocs-s.Frees, s.LiveObjects)
}

func TestFormatDuration_Seconds(t *testing.T) {
	assert.Equal(t, "30s", formatDuration(30*time.Second))
	assert.Equal(t, "0s", formatDuration(0))
	assert.Equal(t, "59s", formatDuration(59*time.Second))
}

func TestFormatDuration_Minutes(t *testing.T) {
	assert.Equal(t, "1m 0s", formatDuration(time.Minute))
	assert.Equal(t, "2m 30s", formatDuration(2*time.Minute+30*time.Second))
	assert.Equal(t, "59m 59s", formatDuration(59*time.Minute+59*time.Second))
}

func TestFormatDuration_Hours(t *testing.T) {
	assert.Equal(t, "1h 0m 0s", formatDuration(time.Hour))
	assert.Equal(t, "1h 30m 45s", formatDuration(time.Hour+30*time.Minute+45*time.Second))
	assert.Equal(t, "25h 0m 0s", formatDuration(25*time.Hour))
}
