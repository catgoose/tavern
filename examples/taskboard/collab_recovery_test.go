package main

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// overflowFeed opens Bob's feed, pauses its writes, and adds public tasks
// until feed updates are dropped. The last task added is the first whose
// activity or counts update was dropped (Bob's feed is the only
// subscriber), and nothing changes the board afterwards. It returns the
// feed's events and that task's title.
func overflowFeed(t *testing.T, tb *testBoard) (feed <-chan sseEvent, last string) {
	t.Helper()
	feed = openSSE(t, tb, "/showcase/collaboration/feed?as=bob&conn=f")
	next(t, feed)
	next(t, feed)
	require.Equal(t, http.StatusNoContent, tb.post(t, "/showcase/collaboration/feed/pause?conn=f", nil))
	for i := 0; tb.app.broker.PublishDrops() == 0; i++ {
		require.Less(t, i, 200, "feed updates were never dropped")
		last = fmt.Sprintf("Burst %d", i)
		require.Equal(t, http.StatusNoContent, tb.as(t, "bob", "/tasks", url.Values{"title": {last}}))
	}
	require.Equal(t, http.StatusNoContent, tb.post(t, "/showcase/collaboration/feed/resume?conn=f", nil))
	return feed, last
}

func currentStats(tb *testBoard) string {
	tb.app.collab.mu.Lock()
	defer tb.app.collab.mu.Unlock()
	return tb.app.collab.renderStatsLocked(roleContributor)
}

// drain reads feed events until none arrives for a while and returns the
// last data seen per event name.
func drain(t *testing.T, feed <-chan sseEvent, quiet time.Duration) map[string]string {
	t.Helper()
	last := map[string]string{}
	for {
		select {
		case ev, ok := <-feed:
			require.True(t, ok, "feed stream ended")
			last[ev.name] = ev.data
		case <-time.After(quiet):
			return last
		}
	}
}

func smallRepairDelays(tb *testBoard) {
	f := &tb.app.collab.feed
	f.mu.Lock()
	f.base, f.max, f.delay = 20*time.Millisecond, 100*time.Millisecond, 20*time.Millisecond
	f.mu.Unlock()
}

// TestCollabFeedRecoversAfterDroppedUpdate drops the feed's final updates,
// stops changing the board, and checks that the feed still ends up showing
// the current counts and activity.
func TestCollabFeedRecoversAfterDroppedUpdate(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Buffer = 2
	tb := newTestBoard(t, cfg)
	smallRepairDelays(tb)

	feed, last := overflowFeed(t, tb)
	got := drain(t, feed, 500*time.Millisecond)

	assert.Contains(t, got["collab/stats/contributor"], currentStats(tb))
	assert.Contains(t, got["collab/activity/contributor"], "Bob added &#34;"+last+"&#34;")
	assert.False(t, tb.app.collab.feed.pending(), "repairs stop once one is delivered")
}

// TestCollabFeedStaysStaleWithoutRepair is the same run with repair turned
// off, showing that the loss is real and the repair is what fixes it. The
// final change's activity update, counts update or both were dropped, so at
// least one of them is never shown.
func TestCollabFeedStaysStaleWithoutRepair(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Buffer = 2
	tb := newTestBoard(t, cfg)
	tb.app.collab.feed.close()

	feed, last := overflowFeed(t, tb)
	got := drain(t, feed, 300*time.Millisecond)
	statsCurrent := strings.Contains(got["collab/stats/contributor"], currentStats(tb))
	activityCurrent := strings.Contains(got["collab/activity/contributor"], "Bob added &#34;"+last+"&#34;")
	t.Logf("current without repair: counts %v, activity %v", statsCurrent, activityCurrent)
	assert.False(t, statsCurrent && activityCurrent, "without repair the feed should still miss the final change")
}

// waitHeld starts a reconnect for conn and reports when it returns.
func waitHeld(t *testing.T, tb *testBoard, conn string) (cancel context.CancelFunc, done <-chan int) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	req, err := http.NewRequestWithContext(ctx, "GET", tb.srv.URL+"/showcase/collaboration/events?as=alice&conn="+conn, http.NoBody)
	require.NoError(t, err)
	out := make(chan int, 1)
	go func() {
		resp, err := tb.srv.Client().Do(req)
		if err != nil {
			out <- 0
			return
		}
		_ = resp.Body.Close()
		out <- resp.StatusCode
	}()
	return cancel, out
}

func dropped(t *testing.T, tb *testBoard, conn string) {
	t.Helper()
	first := openSSE(t, tb, "/showcase/collaboration/events?as=alice&conn="+conn)
	next(t, first)
	require.Equal(t, http.StatusNoContent, tb.post(t, "/showcase/collaboration/drop?conn="+conn, nil))
}

func TestCollabHoldOutlivesAnAbandonedAttemptThenExpires(t *testing.T) {
	tb := newTestBoard(t, DefaultConfig())
	tb.app.collabStreams.holds.ttl = 150 * time.Millisecond
	dropped(t, tb, "a")

	cancel, done := waitHeld(t, tb, "a")
	select {
	case <-done:
		t.Fatal("a held reconnect returned before resume or expiry")
	case <-time.After(50 * time.Millisecond):
	}
	cancel()
	<-done
	assert.Equal(t, 1, tb.app.collabStreams.holds.len(), "an abandoned attempt leaves the hold in place")

	require.Eventually(t, func() bool { return tb.app.collabStreams.holds.len() == 0 }, 2*time.Second, 5*time.Millisecond)
	assert.Equal(t, "board", next(t, openSSE(t, tb, "/showcase/collaboration/events?as=alice&conn=a")).name)
	assert.Equal(t, http.StatusNotFound, tb.post(t, "/showcase/collaboration/resume?conn=a", nil), "an expired hold cannot be resumed")
}

func TestCollabHoldsAreBounded(t *testing.T) {
	tb := newTestBoard(t, DefaultConfig())
	tb.app.collabStreams.holds.max = 2
	dropped(t, tb, "a")
	dropped(t, tb, "b")
	first := openSSE(t, tb, "/showcase/collaboration/events?as=alice&conn=c")
	next(t, first)
	assert.Equal(t, http.StatusServiceUnavailable, tb.post(t, "/showcase/collaboration/drop?conn=c", nil))
	none(t, first) // c's stream was left open
	require.Equal(t, http.StatusNoContent, tb.post(t, "/showcase/collaboration/resume?conn=a", nil))
	assert.Equal(t, http.StatusNoContent, tb.post(t, "/showcase/collaboration/drop?conn=c", nil))
}

func TestCollabShutdownReleasesHolds(t *testing.T) {
	tb := newTestBoard(t, DefaultConfig())
	dropped(t, tb, "a")
	_, done := waitHeld(t, tb, "a")
	tb.app.Shutdown()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("a held reconnect did not end at shutdown")
	}
	assert.Equal(t, 0, tb.app.collabStreams.holds.len())
	_, err := tb.app.collabStreams.holds.add("b")
	assert.ErrorIs(t, err, errHoldsClosed)
	assert.False(t, tb.app.collab.feed.pending())
}

// TestHoldOwnership checks that releasing an old hold cannot remove or
// close a newer one for the same window, and that concurrent use releases
// each hold exactly once.
func TestHoldOwnership(t *testing.T) {
	hs := &holds{ttl: time.Hour, max: 4}
	old, err := hs.add("a")
	require.NoError(t, err)
	require.True(t, hs.take("a"))
	newer, err := hs.add("a")
	require.NoError(t, err)
	hs.remove("a", old) // a late expiry of the old hold
	assert.Same(t, newer, hs.get("a"))
	select {
	case <-newer.released:
		t.Fatal("the newer hold was released")
	default:
	}

	var wg sync.WaitGroup
	for i := range 64 {
		wg.Go(func() {
			conn := strconv.Itoa(i % 3)
			if h, err := hs.add(conn); err == nil {
				switch i % 3 {
				case 0:
					hs.take(conn)
				case 1:
					hs.remove(conn, h)
				default:
					h.release()
				}
			}
		})
	}
	wg.Go(hs.close)
	wg.Wait()
	hs.close()
	assert.Equal(t, 0, hs.len())
}
