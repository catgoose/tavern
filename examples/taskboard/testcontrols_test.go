package main

import (
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTestControlsAreOffByDefault(t *testing.T) {
	tb := newTestBoard(t, DefaultConfig())
	for _, path := range []string{"/test/lab/hold?conn=a&on=1", "/test/lab/burst?hold=1", "/test/pressure/repair?hold=1"} {
		assert.Equal(t, http.StatusNotFound, tb.post(t, path, nil), path)
	}
	assert.Nil(t, tb.app.lab.holds)
	assert.Nil(t, tb.app.lab.holdBurst)
	assert.Nil(t, tb.app.delivery.pressure.holdRepair)
}

// TestTestControlsHoldABurst holds a burst past its end: a second burst is
// refused until the hold is released.
func TestTestControlsHoldABurst(t *testing.T) {
	cfg := DefaultConfig()
	cfg.TestControls = true
	tb := newTestBoard(t, cfg)
	burst := func() int {
		status, _ := tb.command(t, "/showcase/lab/burst?as=alice", "application/json", `{"count":"2"}`)
		return status
	}
	require.Equal(t, http.StatusNoContent, tb.post(t, "/test/lab/burst?hold=1", nil))
	require.Equal(t, http.StatusNoContent, burst())
	time.Sleep(3*labBurstEvery + 100*time.Millisecond) // past its two votes
	assert.Equal(t, http.StatusConflict, burst(), "the held burst is still running")
	require.Equal(t, http.StatusNoContent, tb.post(t, "/test/lab/burst?hold=0", nil))
	require.Eventually(t, func() bool { return burst() == http.StatusNoContent }, time.Second, 20*time.Millisecond, "released, it ends")
	tb.app.Shutdown() // a burst held at shutdown is released too
}
