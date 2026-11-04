package main

import (
	"bufio"
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// openCounter opens the counter stream, optionally resuming after lastID,
// and returns its events (id:event:data) as they arrive.
func openCounter(t *testing.T, tb *testBoard, conn, lastID string) <-chan string {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	req, err := http.NewRequestWithContext(ctx, "GET", tb.srv.URL+"/showcase/connection/events?conn="+conn, http.NoBody)
	require.NoError(t, err)
	if lastID != "" {
		req.Header.Set("Last-Event-ID", lastID)
	}
	resp, err := tb.srv.Client().Do(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	events := make(chan string, 64)
	go func() {
		defer close(events)
		defer resp.Body.Close()
		scanner := bufio.NewScanner(resp.Body)
		var id, name, data string
		for scanner.Scan() {
			line := scanner.Text()
			switch {
			case line == "":
				if name != "" {
					events <- id + ":" + name + ":" + data
				}
				id, name, data = "", "", ""
			case strings.HasPrefix(line, "id: "):
				id = strings.TrimPrefix(line, "id: ")
			case strings.HasPrefix(line, "event: "):
				name = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				data = strings.TrimPrefix(line, "data: ")
			}
		}
	}()
	return events
}

func nextEvent(t *testing.T, events <-chan string) string {
	t.Helper()
	select {
	case ev, ok := <-events:
		require.True(t, ok, "stream ended")
		return ev
	case <-time.After(3 * time.Second):
		t.Fatal("no event within 3s")
		return ""
	}
}

func TestCounterReachesEveryViewer(t *testing.T) {
	tb := newTestBoard(t, DefaultConfig())
	a, b := openCounter(t, tb, "a", ""), openCounter(t, tb, "b", "")
	assert.Equal(t, "0:counter:0", nextEvent(t, a), "a fresh stream starts with the current value")
	assert.Equal(t, "0:counter:0", nextEvent(t, b))

	require.Equal(t, http.StatusNoContent, tb.post(t, "/showcase/connection/increment", nil))
	assert.Equal(t, "1:counter:1", nextEvent(t, a))
	assert.Equal(t, "1:counter:1", nextEvent(t, b))

	// A new window replays the kept values, ending with the current one.
	c := openCounter(t, tb, "c", "")
	assert.Equal(t, "0:counter:0", nextEvent(t, c))
	assert.Equal(t, "1:counter:1", nextEvent(t, c))
}

// TestDropReplaysWhatTheWindowMissed checks the drop control: the stream
// ends, the counter changes before the window can resubscribe, and the
// resumed stream replays that value, then confirms the reconnect.
func TestDropReplaysWhatTheWindowMissed(t *testing.T) {
	tb := newTestBoard(t, DefaultConfig())
	require.Equal(t, http.StatusNoContent, tb.post(t, "/showcase/connection/increment", nil))
	first := openCounter(t, tb, "a", "")
	assert.Equal(t, "0:counter:0", nextEvent(t, first))
	assert.Equal(t, "1:counter:1", nextEvent(t, first))

	require.Equal(t, http.StatusNoContent, tb.post(t, "/showcase/connection/drop?conn=a&away=1", nil))
	select {
	case _, ok := <-first:
		assert.False(t, ok, "the dropped stream should end")
	case <-time.After(3 * time.Second):
		t.Fatal("the dropped stream did not end")
	}

	resumed := openCounter(t, tb, "a", "1")
	assert.Equal(t, "2:counter:2", nextEvent(t, resumed))
	assert.Equal(t, `:tavern-reconnected:{"replayDelivered":1,"replayDropped":0}`, nextEvent(t, resumed))

	assert.Equal(t, http.StatusNotFound, tb.post(t, "/showcase/connection/drop?conn=nobody", nil))
}

func TestShowcasePages(t *testing.T) {
	tb := newTestBoard(t, DefaultConfig())
	assert.Contains(t, tb.get(t, "/"), `href="/showcase"`)
	assert.Contains(t, tb.get(t, "/showcase"), `href="/showcase/connection"`)
	page := tb.get(t, "/showcase/connection")
	assert.Contains(t, page, `sse-connect="/showcase/connection/events?conn=`)
	for _, asset := range []string{"/static/htmx-2.0.11.min.js", "/static/htmx-ext-sse-2.2.4.min.js", "/static/tavern-0.0.24.min.js"} {
		assert.Contains(t, page, asset)
		resp, err := tb.srv.Client().Get(tb.srv.URL + asset)
		require.NoError(t, err)
		_ = resp.Body.Close()
		assert.Equal(t, http.StatusOK, resp.StatusCode, asset)
	}
}
