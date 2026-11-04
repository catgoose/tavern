package main

import (
	"bufio"
	"context"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// recEvent is one SSE event with its id.
type recEvent struct{ id, name, data string }

// recStream opens a scenario's stream for conn, resuming after lastID when
// set, and returns its events.
func recStream(t *testing.T, tb *testBoard, name, conn, lastID string) <-chan recEvent {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	req, err := http.NewRequestWithContext(ctx, "GET", tb.srv.URL+"/showcase/recovery/"+name+"/events?conn="+conn, http.NoBody)
	require.NoError(t, err)
	if lastID != "" {
		req.Header.Set("Last-Event-ID", lastID)
	}
	events := make(chan recEvent, 64)
	go func() {
		defer close(events)
		resp, err := tb.srv.Client().Do(req)
		if err != nil {
			return
		}
		defer resp.Body.Close()
		scanner := bufio.NewScanner(resp.Body)
		var ev recEvent
		var data []string
		for scanner.Scan() {
			line := scanner.Text()
			switch {
			case line == "":
				if ev.name != "" {
					ev.data = strings.Join(data, "\n")
					events <- ev
				}
				ev, data = recEvent{}, nil
			case strings.HasPrefix(line, "id: "):
				ev.id = strings.TrimPrefix(line, "id: ")
			case strings.HasPrefix(line, "event: "):
				ev.name = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				data = append(data, strings.TrimPrefix(line, "data: "))
			}
		}
	}()
	return events
}

func recNext(t *testing.T, events <-chan recEvent) recEvent {
	t.Helper()
	select {
	case ev, ok := <-events:
		require.True(t, ok, "stream ended")
		return ev
	case <-time.After(3 * time.Second):
		t.Fatal("no event within 3s")
		return recEvent{}
	}
}

// recUntilQuiet reads events until none arrives for quiet.
func recUntilQuiet(events <-chan recEvent, quiet time.Duration) []recEvent {
	var out []recEvent
	for {
		select {
		case ev, ok := <-events:
			if !ok {
				return out
			}
			out = append(out, ev)
		case <-time.After(quiet):
			return out
		}
	}
}

func (tb *testBoard) recPost(t *testing.T, path string) int {
	t.Helper()
	return tb.post(t, "/showcase/recovery/"+path, nil)
}

func (tb *testBoard) recValue(name, topic string) int {
	return tb.app.recovery.scenarios[name].current()[topic]
}

func lastValue(events []recEvent, event string) (value, id string) {
	for _, ev := range events {
		if ev.name == event {
			value = ev.data
		}
		if ev.id != "" {
			id = ev.id
		}
	}
	return value, id
}

func names(events []recEvent) []string {
	out := make([]string, len(events))
	for i, ev := range events {
		out[i] = ev.name
	}
	return out
}

func observed(tb *testBoard, name, conn, text string) bool {
	for _, o := range tb.app.recovery.scenarios[name].observations(conn) {
		if strings.Contains(o.Text, text) {
			return true
		}
	}
	return false
}

// awayAndResume opens conn, misses n changes, and resumes with its last ID.
func awayAndResume(t *testing.T, tb *testBoard, name, conn string, n int) []recEvent {
	t.Helper()
	first := recStream(t, tb, name, conn, "")
	snap := recNext(t, first)
	require.NotEmpty(t, snap.id, "the first frame carries the current event ID")
	require.Equal(t, http.StatusNoContent, tb.recPost(t, name+"/away?conn="+conn+"&n="+strconv.Itoa(n)))
	recUntilQuiet(first, 200*time.Millisecond)
	return recUntilQuiet(recStream(t, tb, name, conn, snap.id), 400*time.Millisecond)
}

func TestRecoveryResumeReplaysAShortAbsence(t *testing.T) {
	tb := newTestBoard(t, DefaultConfig())
	got := awayAndResume(t, tb, "resume", "a", 3)
	assert.Equal(t, []string{"value", "value", "value", "tavern-reconnected"}, names(got))
	assert.Equal(t, `{"replayDelivered":3,"replayDropped":0}`, got[3].data)
	value, _ := lastValue(got, "value")
	assert.Equal(t, strconv.Itoa(tb.recValue("resume", "recovery/resume")), value)
	require.Eventually(t, func() bool { return observed(tb, "resume", "a", "3 missed, 3 replayed, 0 dropped") }, time.Second, 5*time.Millisecond)
}

func TestRecoveryLongAbsenceIsAGapWithASnapshot(t *testing.T) {
	tb := newTestBoard(t, DefaultConfig())
	got := awayAndResume(t, tb, "resume", "a", 8)
	assert.Equal(t, []string{"tavern-replay-gap", "value", "tavern-reconnected"}, names(got))
	value, id := lastValue(got, "value")
	assert.Equal(t, strconv.Itoa(tb.recValue("resume", "recovery/resume")), value)
	assert.NotEmpty(t, id, "the snapshot carries the current ID for the next resume")
	require.Eventually(t, func() bool { return observed(tb, "resume", "a", "gap") }, time.Second, 5*time.Millisecond)
}

func TestRecoverySilentGapLeavesTheWindowBehind(t *testing.T) {
	tb := newTestBoard(t, DefaultConfig())
	got := awayAndResume(t, tb, "resume-silent", "a", 8)
	assert.Equal(t, []string{"tavern-reconnected"}, names(got), "no gap event and no snapshot")
	require.Eventually(t, func() bool { return observed(tb, "resume-silent", "a", "gap") }, time.Second, 5*time.Millisecond, "only the server sees the gap")
}

func TestRecoveryMultiResumesEachTopic(t *testing.T) {
	tb := newTestBoard(t, DefaultConfig())
	got := awayAndResume(t, tb, "multi", "a", 4)
	left, _ := lastValue(got, "left")
	right, _ := lastValue(got, "right")
	assert.Equal(t, strconv.Itoa(tb.recValue("multi", "recovery/left")), left)
	assert.Equal(t, strconv.Itoa(tb.recValue("multi", "recovery/right")), right)
	reconnected := 0
	for _, ev := range got {
		if ev.name == "tavern-reconnected" {
			reconnected++
			assert.Equal(t, `{"replayDelivered":4,"replayDropped":0}`, ev.data, "each topic replays the same four IDs")
		}
	}
	assert.Equal(t, 2, reconnected, "one confirmation per topic")
	require.Eventually(t, func() bool {
		return observed(tb, "multi", "", "recovery/left: resumed") && observed(tb, "multi", "", "recovery/right: resumed")
	}, time.Second, 5*time.Millisecond)

	gap := awayAndResume(t, tb, "multi", "b", 8)
	left, _ = lastValue(gap, "left")
	right, _ = lastValue(gap, "right")
	assert.Equal(t, strconv.Itoa(tb.recValue("multi", "recovery/left")), left, "each topic falls back to its own snapshot")
	assert.Equal(t, strconv.Itoa(tb.recValue("multi", "recovery/right")), right)
}

// TestRecoveryTruncatedReplayIsRepaired misses more changes than the queue
// holds. The replay is cut short and its notices are dropped with it; the
// server's republish brings the window to the current value. With repair
// off the window stays behind.
func TestRecoveryTruncatedReplayIsRepaired(t *testing.T) {
	for _, repair := range []bool{true, false} {
		t.Run("repair="+strconv.FormatBool(repair), func(t *testing.T) {
			tb := newTestBoard(t, DefaultConfig())
			s := tb.app.recovery.scenarios["truncation"]
			s.repair.mu.Lock()
			s.repair.base, s.repair.max, s.repair.delay = 20*time.Millisecond, 100*time.Millisecond, 20*time.Millisecond
			s.repair.mu.Unlock()
			if !repair {
				s.repair.close()
			}
			got := awayAndResume(t, tb, "truncation", "a", 10)
			assert.NotContains(t, names(got), "tavern-replay-truncated", "the full queue drops the notice too")
			require.Eventually(t, func() bool { return observed(tb, "truncation", "a", "10 missed, 4 replayed, 6 dropped") }, time.Second, 5*time.Millisecond)
			value, _ := lastValue(got, "value")
			assert.Equal(t, repair, value == strconv.Itoa(tb.recValue("truncation", "recovery/truncated")), "got %q", value)
			if !repair {
				return
			}
			_, id := lastValue(got, "value")
			require.Equal(t, http.StatusNoContent, tb.recPost(t, "truncation/away?conn=a&n=0"))
			confirmed := recUntilQuiet(recStream(t, tb, "truncation", "a", id), 300*time.Millisecond)
			assert.Equal(t, []string{"tavern-reconnected"}, names(confirmed), "a resume with nothing missed is confirmed")
			assert.Equal(t, `{"replayDelivered":0,"replayDropped":0}`, confirmed[0].data)
		})
	}
}

// pressure holds a window back, makes changes until its queue overflows,
// releases it and returns what it then receives.
func pressure(t *testing.T, tb *testBoard, name string) []recEvent {
	t.Helper()
	events := recStream(t, tb, name, "a", "")
	recNext(t, events)
	require.Equal(t, http.StatusNoContent, tb.recPost(t, name+"/pause?conn=a&on=1"))
	for range 2 {
		require.Equal(t, http.StatusNoContent, tb.recPost(t, name+"/change?n=10"))
	}
	require.Equal(t, http.StatusNoContent, tb.recPost(t, name+"/pause?conn=a&on=0"))
	return recUntilQuiet(events, 500*time.Millisecond)
}

func TestRecoveryFullQueues(t *testing.T) {
	t.Run("drop-newest is repaired", func(t *testing.T) {
		tb := newTestBoard(t, DefaultConfig())
		s := tb.app.recovery.scenarios["drop-newest"]
		s.repair.mu.Lock()
		s.repair.base, s.repair.max, s.repair.delay = 20*time.Millisecond, 100*time.Millisecond, 20*time.Millisecond
		s.repair.mu.Unlock()
		value, _ := lastValue(pressure(t, tb, "drop-newest"), "value")
		assert.Equal(t, strconv.Itoa(tb.recValue("drop-newest", "recovery/newest")), value)
	})
	t.Run("drop-newest without repair stays behind", func(t *testing.T) {
		tb := newTestBoard(t, DefaultConfig())
		tb.app.recovery.scenarios["drop-newest"].repair.close()
		value, _ := lastValue(pressure(t, tb, "drop-newest"), "value")
		assert.NotEqual(t, strconv.Itoa(tb.recValue("drop-newest", "recovery/newest")), value)
	})
	t.Run("drop-oldest keeps the newest", func(t *testing.T) {
		tb := newTestBoard(t, DefaultConfig())
		value, _ := lastValue(pressure(t, tb, "drop-oldest"), "value")
		assert.Equal(t, strconv.Itoa(tb.recValue("drop-oldest", "recovery/oldest")), value)
	})
	for _, c := range []struct{ name, reason string }{{"evict", "evicted"}, {"loss-aware", "queue-loss"}} {
		t.Run(c.name+" ends the stream", func(t *testing.T) {
			tb := newTestBoard(t, DefaultConfig())
			got := pressure(t, tb, c.name)
			_ = got
			require.Eventually(t, func() bool { return observed(tb, c.name, "a", "stream ended: "+c.reason) }, 2*time.Second, 5*time.Millisecond)
			topic := tb.app.recovery.scenarios[c.name].topics[0]
			again := recStream(t, tb, c.name, "a", "")
			assert.Equal(t, strconv.Itoa(tb.recValue(c.name, topic)), recNext(t, again).data, "the reconnect starts from the current value")
		})
	}
}

func TestRecoveryReplayStoreFailures(t *testing.T) {
	t.Run("a failed read is a gap with a snapshot", func(t *testing.T) {
		tb := newTestBoard(t, DefaultConfig())
		first := recStream(t, tb, "store", "a", "")
		snap := recNext(t, first)
		require.Equal(t, http.StatusNoContent, tb.recPost(t, "store/fault?read=1"))
		require.Equal(t, http.StatusNoContent, tb.recPost(t, "store/away?conn=a&n=2"))
		got := recUntilQuiet(recStream(t, tb, "store", "a", snap.id), 400*time.Millisecond)
		assert.Equal(t, []string{"tavern-replay-gap", "value", "tavern-reconnected"}, names(got))
		value, _ := lastValue(got, "value")
		assert.Equal(t, strconv.Itoa(tb.recValue("store", "recovery/stored")), value)
		assert.True(t, observed(tb, "store", "", "replay store after-id on recovery/stored failed: replay store unavailable"))
	})
	t.Run("strict ends the replay and continues from a snapshot", func(t *testing.T) {
		tb := newTestBoard(t, DefaultConfig())
		first := recStream(t, tb, "store-strict", "a", "")
		snap := recNext(t, first)
		require.Equal(t, http.StatusNoContent, tb.recPost(t, "store-strict/fault?read=1"))
		require.Equal(t, http.StatusNoContent, tb.recPost(t, "store-strict/away?conn=a&n=2"))
		got := recUntilQuiet(recStream(t, tb, "store-strict", "a", snap.id), 400*time.Millisecond)
		assert.Equal(t, []string{"value"}, names(got), "no replay and no reconnected notice, then the snapshot")
		value, _ := lastValue(got, "value")
		assert.Equal(t, strconv.Itoa(tb.recValue("store-strict", "recovery/strict")), value)
		require.Eventually(t, func() bool { return observed(tb, "store-strict", "a", "replay failed; continuing from a snapshot") }, time.Second, 5*time.Millisecond)
		require.True(t, observed(tb, "store-strict", "", "replay store after-id on recovery/strict failed"))

		require.Equal(t, http.StatusNoContent, tb.recPost(t, "store-strict/fault?read=0"))
		_, id := lastValue(got, "value")
		require.Equal(t, http.StatusNoContent, tb.recPost(t, "store-strict/away?conn=a&n=0"))
		assert.Equal(t, []string{"tavern-reconnected"}, names(recUntilQuiet(recStream(t, tb, "store-strict", "a", id), 300*time.Millisecond)),
			"the window resumes from the snapshot's ID")
		assert.Equal(t, []string{"value", "value", "tavern-reconnected"}, names(awayAndResume(t, tb, "store-strict", "b", 2)),
			"once the store works again a resume replays")
	})
	t.Run("failed appends are lost without a gap", func(t *testing.T) {
		tb := newTestBoard(t, DefaultConfig())
		first := recStream(t, tb, "store", "a", "")
		snap := recNext(t, first)
		require.Equal(t, http.StatusNoContent, tb.recPost(t, "store/fault?append=1"))
		require.Equal(t, http.StatusNoContent, tb.recPost(t, "store/away?conn=a&n=2"))
		assert.True(t, observed(tb, "store", "", "replay store append on recovery/stored failed"))
		require.Equal(t, http.StatusNoContent, tb.recPost(t, "store/fault?append=0"))
		got := recUntilQuiet(recStream(t, tb, "store", "a", snap.id), 400*time.Millisecond)
		assert.Equal(t, []string{"tavern-reconnected"}, names(got),
			"the window's ID is still stored and nothing after it was kept, so the resume looks complete; only the error handler saw the loss")
		require.Equal(t, http.StatusNoContent, tb.recPost(t, "store/change"))
		next := recUntilQuiet(recStream(t, tb, "store", "b", ""), 200*time.Millisecond)
		value, _ := lastValue(next, "value")
		assert.Equal(t, strconv.Itoa(tb.recValue("store", "recovery/stored")), value, "with appends working again, the next change brings windows current")
	})
	t.Run("clearing drops history but keeps the policies", func(t *testing.T) {
		tb := newTestBoard(t, DefaultConfig())
		first := recStream(t, tb, "store", "a", "")
		snap := recNext(t, first)
		require.Equal(t, http.StatusNoContent, tb.recPost(t, "store/clear"))
		require.Equal(t, http.StatusNoContent, tb.recPost(t, "store/away?conn=a&n=2"))
		got := recUntilQuiet(recStream(t, tb, "store", "a", snap.id), 400*time.Millisecond)
		assert.Equal(t, []string{"tavern-replay-gap", "value", "tavern-reconnected"}, names(got),
			"the ID from before the clear is gone; the gap policy, set again after ClearReplay, sends a snapshot")
		value, _ := lastValue(got, "value")
		assert.Equal(t, strconv.Itoa(tb.recValue("store", "recovery/stored")), value)
		assert.Equal(t, []string{"value", "value", "value", "tavern-reconnected"}, names(awayAndResume(t, tb, "store", "b", 3)),
			"history kept after the clear replays")
	})
}

// TestRecoveryControlsActOnOneWindow disconnects and holds back one window
// while another keeps receiving.
func TestRecoveryControlsActOnOneWindow(t *testing.T) {
	tb := newTestBoard(t, DefaultConfig())
	other := recStream(t, tb, "resume", "b", "")
	recNext(t, other)
	mine := recStream(t, tb, "resume", "a", "")
	recNext(t, mine)
	require.Equal(t, http.StatusNoContent, tb.recPost(t, "resume/away?conn=a&n=2"))
	assert.Len(t, recUntilQuiet(other, 300*time.Millisecond), 2, "the other window saw both changes live")
	assert.Equal(t, http.StatusNotFound, tb.recPost(t, "resume/away?conn=nobody&n=1"))
	assert.Equal(t, http.StatusBadRequest, tb.recPost(t, "resume/away?conn=b&n=21"))
	assert.Equal(t, http.StatusNotFound, tb.recPost(t, "drop-newest/pause?conn=nobody&on=1"))
	assert.Equal(t, http.StatusNotFound, tb.recPost(t, "resume/fault?read=1"), "only store scenarios have a store")
}

func TestRecoveryShutdownReleasesHeldWindows(t *testing.T) {
	tb := newTestBoard(t, DefaultConfig())
	s := tb.app.recovery.scenarios["resume"]
	_, err := s.holds.add("held")
	require.NoError(t, err)
	waiting := recStream(t, tb, "resume", "held", "")
	tb.app.Shutdown()
	select {
	case _, ok := <-waiting:
		assert.False(t, ok)
	case <-time.After(2 * time.Second):
		t.Fatal("a held stream did not end at shutdown")
	}
	assert.Zero(t, s.holds.len())
}
