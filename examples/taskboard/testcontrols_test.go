package main

import (
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func controlBoard(t *testing.T) *testBoard {
	t.Helper()
	cfg := DefaultConfig()
	cfg.TestControls = true
	return newTestBoard(t, cfg)
}

// labHolds is whether each open lab stream is held, by stream key.
func labHolds(l *lab) map[string]bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := map[string]bool{}
	for key, c := range l.conns {
		out[key] = c.hold.held()
	}
	return out
}

func TestTestControlsAreOffByDefault(t *testing.T) {
	tb := newTestBoard(t, DefaultConfig())
	for _, path := range []string{"/test/lab/hold?as=bob&conn=a&on=1", "/test/lab/burst?hold=1", "/test/pressure/repair?hold=1"} {
		assert.Equal(t, http.StatusNotFound, tb.post(t, path, nil), path)
	}
	assert.False(t, tb.app.lab.testControls)
	assert.Nil(t, tb.app.lab.holdBurst)
	assert.Nil(t, tb.app.delivery.pressure.holdRepair)
	assert.Nil(t, tb.app.delivery.pressure.queues)
}

// TestTestControlsHoldABurst holds a burst past its end: a second burst is
// refused until the hold is released.
func TestTestControlsHoldABurst(t *testing.T) {
	tb := controlBoard(t)
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
}

// TestTestControlsLabHoldBelongsToOpenStreams holds a window's open
// streams only: not an unknown window, not another persona's, not a stream
// opened later, and not past the stream that owned it.
func TestTestControlsLabHoldBelongsToOpenStreams(t *testing.T) {
	tb := controlBoard(t)
	l := tb.app.lab
	assert.Equal(t, http.StatusNotFound, tb.post(t, "/test/lab/hold?as=bob&conn=unknown&on=1", nil), "no such window")
	assert.Equal(t, http.StatusBadRequest, tb.post(t, "/test/lab/hold?conn=b&on=1", nil))
	assert.Empty(t, labHolds(l), "nothing is kept for an unknown window")

	votes := openSSE(t, tb, "/showcase/lab/votes/events?as=bob&conn=b")
	editor, endEditor := openSSECancel(t, tb, "/showcase/lab/editor/events?as=bob&conn=b")
	next(t, votes)
	next(t, editor)
	assert.Equal(t, http.StatusNotFound, tb.post(t, "/test/lab/hold?as=alice&conn=b&on=1", nil), "another persona's window")

	require.Equal(t, http.StatusNoContent, tb.post(t, "/test/lab/hold?as=bob&conn=b&on=1", nil))
	assert.Equal(t, map[string]bool{"votes/b": true, "editor/b": true}, labHolds(l))
	require.Equal(t, http.StatusNoContent, tb.post(t, "/showcase/lab/tasks", url.Values{"as": {"bob"}, "title": {"Held"}}))
	none(t, votes)
	require.Equal(t, http.StatusNoContent, tb.post(t, "/test/lab/hold?as=bob&conn=b&on=0", nil))
	assert.Equal(t, "lab-update", next(t, votes).name, "released, the held write goes out")

	require.Equal(t, http.StatusNoContent, tb.post(t, "/test/lab/hold?as=bob&conn=b&on=1", nil))
	endEditor()
	require.Eventually(t, func() bool { _, ok := labHolds(l)["editor/b"]; return !ok }, time.Second, 5*time.Millisecond)
	assert.Equal(t, map[string]bool{"votes/b": true}, labHolds(l), "ending one region leaves the other's hold")

	reopened := openSSE(t, tb, "/showcase/lab/editor/events?as=bob&conn=b")
	next(t, reopened)
	assert.Equal(t, map[string]bool{"votes/b": true, "editor/b": false}, labHolds(l), "a new stream on the same window starts unheld")
	require.Equal(t, http.StatusNoContent, tb.post(t, "/test/lab/hold?as=bob&conn=b&on=0", nil))
}

// TestTestControlsLabHoldEndsWithItsRequest cancels a stream whose write is
// held: the handler returns and nothing of the hold is kept.
func TestTestControlsLabHoldEndsWithItsRequest(t *testing.T) {
	tb := controlBoard(t)
	l := tb.app.lab
	votes, cancel := openSSECancel(t, tb, "/showcase/lab/votes/events?as=bob&conn=b")
	next(t, votes)
	require.Equal(t, http.StatusNoContent, tb.post(t, "/test/lab/hold?as=bob&conn=b&on=1", nil))
	require.Equal(t, http.StatusNoContent, tb.post(t, "/showcase/lab/tasks", url.Values{"as": {"bob"}, "title": {"Held"}}))
	none(t, votes)
	cancel()
	require.Eventually(t, func() bool { return len(labHolds(l)) == 0 }, time.Second, 5*time.Millisecond)
	assert.Equal(t, http.StatusNotFound, tb.post(t, "/test/lab/hold?as=bob&conn=b&on=0", nil), "the window is gone")
}

// TestTestControlsReleaseAtShutdown holds a lab write, a burst and the
// pressure repair, then shuts down: each is released, the held stream
// ends, and no hold can be set again.
func TestTestControlsReleaseAtShutdown(t *testing.T) {
	tb := controlBoard(t)
	l := tb.app.lab
	votes := openSSE(t, tb, "/showcase/lab/votes/events?as=bob&conn=b")
	next(t, votes)
	require.Equal(t, http.StatusNoContent, tb.post(t, "/test/lab/hold?as=bob&conn=b&on=1", nil))
	require.Equal(t, http.StatusNoContent, tb.post(t, "/showcase/lab/tasks", url.Values{"as": {"bob"}, "title": {"Held"}}))
	require.Equal(t, http.StatusNoContent, tb.post(t, "/test/lab/burst?hold=1", nil))
	require.Equal(t, http.StatusNoContent, tb.post(t, "/test/pressure/repair?hold=1", nil))

	tb.app.Shutdown()
	require.Eventually(t, func() bool {
		select {
		case _, ok := <-votes:
			return !ok
		default:
			return false
		}
	}, 2*time.Second, 5*time.Millisecond, "the held stream ends")
	require.Eventually(t, func() bool { return len(labHolds(l)) == 0 }, time.Second, 5*time.Millisecond)
	assert.False(t, l.holdBurst.held())
	assert.False(t, tb.app.delivery.pressure.holdRepair.held())
	assert.Equal(t, http.StatusServiceUnavailable, tb.post(t, "/test/lab/burst?hold=1", nil))
	assert.Equal(t, http.StatusServiceUnavailable, tb.post(t, "/test/pressure/repair?hold=1", nil))
	assert.False(t, l.holdBurst.held())
}

// TestTestHoldReleasesItself: a hold nobody releases ends at its limit,
// and setting it again restarts the limit.
func TestTestHoldReleasesItself(t *testing.T) {
	h := newTestHold(80 * time.Millisecond)
	require.NoError(t, h.set(true))
	time.Sleep(50 * time.Millisecond)
	require.NoError(t, h.set(true))
	time.Sleep(50 * time.Millisecond)
	assert.True(t, h.held(), "the first limit no longer applies")
	require.Eventually(t, func() bool { return !h.held() }, time.Second, 5*time.Millisecond)

	require.NoError(t, h.set(true))
	h.close()
	assert.False(t, h.held())
	assert.ErrorIs(t, h.set(true), errControlsClosed)
}

// TestTestControlsPressureReleaseWaitsForDrain releases the repair only once
// the named window's broker queues are empty, and releases it anyway when
// the window is unknown.
func TestTestControlsPressureReleaseWaitsForDrain(t *testing.T) {
	tb := controlBoard(t)
	l := tb.app.delivery.pressure
	require.Equal(t, http.StatusNoContent, tb.post(t, "/test/pressure/repair?hold=1", nil))
	assert.Equal(t, http.StatusNotFound, tb.post(t, "/test/pressure/repair?hold=0&drained=unknown", nil))
	assert.False(t, l.holdRepair.held(), "released anyway")

	events, cancel := openSSECancel(t, tb, "/showcase/delivery/pressure/events?conn=p")
	next(t, events)
	require.Equal(t, http.StatusNoContent, tb.post(t, "/showcase/delivery/pressure/pause?conn=p&on=1", nil))
	require.Equal(t, http.StatusNoContent, tb.post(t, "/showcase/delivery/pressure/change?n=6", nil))
	queued := func() int {
		l.mu.Lock()
		defer l.mu.Unlock()
		n := 0
		for _, ch := range l.queues["p"].chans {
			n += len(ch)
		}
		return n
	}
	require.Eventually(t, func() bool { return queued() > 0 }, time.Second, 5*time.Millisecond)
	require.Equal(t, http.StatusNoContent, tb.post(t, "/test/pressure/repair?hold=1", nil))
	require.Equal(t, http.StatusNoContent, tb.post(t, "/showcase/delivery/pressure/pause?conn=p&on=0", nil))
	require.Equal(t, http.StatusNoContent, tb.post(t, "/test/pressure/repair?hold=0&drained=p", nil))
	assert.Zero(t, queued())
	assert.False(t, l.holdRepair.held())

	cancel()
	require.Eventually(t, func() bool {
		l.mu.Lock()
		defer l.mu.Unlock()
		return len(l.queues) == 0
	}, time.Second, 5*time.Millisecond, "the window's queues are forgotten when its stream ends")
}
