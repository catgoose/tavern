package main

import (
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"net"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// lifeline opens who's lifeline as conn and consumes the replayed notices.
func lifeline(t *testing.T, tb *testBoard, who, conn string) <-chan sseEvent {
	t.Helper()
	events := openSSE(t, tb, "/showcase/shell/lifeline?as="+who+"&conn="+conn)
	require.Equal(t, "shell/notices", next(t, events).name)
	return events
}

func (tb *testBoard) shellPost(t *testing.T, who, path string) int {
	t.Helper()
	return tb.post(t, "/showcase/shell"+path, map[string][]string{"as": {who}})
}

// nextN reads n events and returns them by name.
func nextN(t *testing.T, events <-chan sseEvent, n int) map[string]string {
	t.Helper()
	got := map[string]string{}
	for range n {
		ev := next(t, events)
		got[ev.name] = ev.data
	}
	return got
}

func TestShellLifelineBelongsToItsWindow(t *testing.T) {
	tb := newTestBoard(t, DefaultConfig())
	lifeline(t, tb, "alice", "a")

	resp, err := tb.srv.Client().Get(tb.srv.URL + "/showcase/shell/lifeline?as=bob&conn=a")
	require.NoError(t, err)
	_ = resp.Body.Close()
	assert.Equal(t, http.StatusConflict, resp.StatusCode, "a connection id is used once")

	assert.Equal(t, http.StatusNotFound, tb.shellPost(t, "bob", "/follow?conn=a&channel=releases&on=1"), "only the owner changes its topics")
	assert.Equal(t, http.StatusBadRequest, tb.shellPost(t, "alice", "/follow?conn=a&channel=secrets&on=1"), "only allowlisted channels")
	assert.Equal(t, http.StatusNotFound, tb.shellPost(t, "alice", "/follow?conn=nobody&channel=releases&on=1"))
	assert.Equal(t, []string{topicShellNotices}, tb.app.broker.SubscriberTopics("a"))
}

func TestShellFollowChangesLifelineTopics(t *testing.T) {
	tb := newTestBoard(t, DefaultConfig())
	events := lifeline(t, tb, "alice", "a")

	require.Equal(t, http.StatusNoContent, tb.shellPost(t, "alice", "/follow?conn=a&channel=releases&on=1"))
	got := nextN(t, events, 2)
	var changed struct {
		Action, Topic string
		Topics        []string
	}
	require.NoError(t, json.Unmarshal([]byte(got["tavern-topics-changed"]), &changed))
	assert.Equal(t, "added", changed.Action)
	assert.Equal(t, "shell/channel/releases", changed.Topic)
	slices.Sort(changed.Topics)
	assert.Equal(t, []string{"shell/channel/releases", "shell/notices"}, changed.Topics)
	assert.Contains(t, got["shell/channel/releases"], "Nothing yet", "a new topic starts with its latest post")
	assert.Equal(t, http.StatusOK, tb.shellPost(t, "alice", "/follow?conn=a&channel=releases&on=1"), "following twice changes nothing")

	require.Equal(t, http.StatusNoContent, tb.shellPost(t, "bob", "/channels/releases"))
	ev := next(t, events)
	assert.Equal(t, "shell/channel/releases", ev.name)
	assert.Contains(t, ev.data, "Bob posted")

	require.Equal(t, http.StatusNoContent, tb.shellPost(t, "alice", "/follow?conn=a&channel=releases&on=0"))
	assert.Contains(t, next(t, events).data, `"action":"removed"`)
	require.Equal(t, http.StatusNoContent, tb.shellPost(t, "bob", "/channels/releases"))
	none(t, events)
	assert.Equal(t, []string{topicShellNotices}, tb.app.broker.SubscriberTopics("a"))
}

func TestShellLeadsChannelAndNotesFollowRole(t *testing.T) {
	tb := newTestBoard(t, DefaultConfig())
	alice := lifeline(t, tb, "alice", "a")
	bob := lifeline(t, tb, "bob", "b")

	assert.Equal(t, http.StatusForbidden, tb.shellPost(t, "bob", "/leads?on=1"))
	require.Equal(t, http.StatusNoContent, tb.shellPost(t, "alice", "/leads?on=1"))
	got := nextN(t, alice, 2)
	assert.Contains(t, got, "tavern-topics-changed")
	assert.Contains(t, got, "shell/channel/leads")
	none(t, bob)
	assert.NotContains(t, tb.app.broker.SubscriberTopics("b"), topicLeadsChannel)

	require.Equal(t, http.StatusNoContent, tb.shellPost(t, "alice", "/role-note?to=lead"))
	ev := next(t, alice)
	assert.Equal(t, "shell/role", ev.name)
	assert.Contains(t, ev.data, "Alice sent a note to team leads")
	none(t, bob)
	require.Equal(t, http.StatusNoContent, tb.shellPost(t, "alice", "/role-note?to=contributor"))
	assert.Equal(t, "shell/role", next(t, bob).name)
	none(t, alice)
}

func TestShellFeedsGlob(t *testing.T) {
	tb := newTestBoard(t, DefaultConfig())
	lifeline(t, tb, "alice", "a")
	feeds := openSSE(t, tb, "/showcase/shell/views/feeds/events?as=alice&conn=a")
	assert.Equal(t, "shell-feed-ready", next(t, feeds).name)

	for _, f := range []string{"web", "noise", "api"} {
		require.Equal(t, http.StatusNoContent, tb.shellPost(t, "alice", "/feeds/"+f))
	}
	tb.app.broker.Publish("shell/feeds/brand/new/topic", "first post")
	for _, topic := range []string{"shell/feeds/releases/web", "shell/feeds/releases/api", "shell/feeds/brand/new/topic"} {
		ev := next(t, feeds)
		assert.Equal(t, "shell-feed", ev.name)
		assert.Contains(t, ev.data, "<code>"+topic+"</code>")
	}
	none(t, feeds)
}

func TestShellOpsGroup(t *testing.T) {
	tb := newTestBoard(t, DefaultConfig())
	ops := openSSE(t, tb, "/showcase/shell/views/ops/events?as=alice")
	got := nextN(t, ops, 2)
	assert.Contains(t, got["shell/ops/deploys"], "0 deploys")
	assert.Contains(t, got["shell/ops/alerts"], "0 alerts")

	require.Equal(t, http.StatusNoContent, tb.shellPost(t, "alice", "/ops/deploy"))
	ev := next(t, ops)
	assert.Equal(t, "shell/ops/deploys", ev.name)
	assert.Contains(t, ev.data, "1 deploys")
	assert.Equal(t, http.StatusBadRequest, tb.shellPost(t, "alice", "/ops/reboot"))
}

// TestShellBoardRecoversAfterLoss lets a board stream fall behind until it
// ends, stops changing the board, and checks that the next stream starts
// from the current board.
func TestShellBoardRecoversAfterLoss(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Buffer = 2
	cfg.WriteTimeout = 200 * time.Millisecond
	tb := newTestBoard(t, cfg)
	lifeline(t, tb, "alice", "a")

	conn, err := net.Dial("tcp", tb.srv.Listener.Addr().String())
	require.NoError(t, err)
	defer conn.Close()
	_, err = fmt.Fprintf(conn, "GET /showcase/shell/views/board/events?as=alice&conn=a HTTP/1.1\r\nHost: x\r\n\r\n")
	require.NoError(t, err)
	require.Eventually(t, func() bool { _, ok := tb.app.shell.boardStreams.Load("a"); return ok }, 2*time.Second, time.Millisecond)

	for i := 0; ; i++ {
		require.Less(t, i, 10000, "the stream never ended")
		if _, ok := tb.app.shell.boardStreams.Load("a"); !ok {
			break
		}
		tb.app.shell.addBoardTask(personas[0])
	}
	require.Eventually(t, func() bool {
		return strings.Contains(tb.logs.String(), "shell board stream ended") && strings.Contains(tb.logs.String(), "reason=queue-loss")
	}, 3*time.Second, time.Millisecond)

	tb.app.shell.mu.Lock()
	latest := tb.app.shell.board[0]
	tb.app.shell.mu.Unlock()
	board := openSSE(t, tb, "/showcase/shell/views/board/events?as=alice&conn=a")
	snap := next(t, board)
	assert.Equal(t, "shell-board-snapshot", snap.name)
	assert.Contains(t, snap.data, template.HTMLEscapeString(latest))
}

func TestShellBreakHoldsTheBoardUntilRecover(t *testing.T) {
	tb := newTestBoard(t, DefaultConfig())
	lifeline(t, tb, "alice", "a")
	first := openSSE(t, tb, "/showcase/shell/views/board/events?as=alice&conn=a")
	next(t, first)
	assert.Equal(t, http.StatusNotFound, tb.shellPost(t, "bob", "/views/board/break?conn=a"), "only the owner breaks it")
	require.Equal(t, http.StatusNoContent, tb.shellPost(t, "alice", "/views/board/break?conn=a"))
	for ev := range first {
		// The stream sends one snapshot per topic; nothing else before it ends.
		assert.Equal(t, "shell-board-snapshot", ev.name)
	}

	again := openSSE(t, tb, "/showcase/shell/views/board/events?as=alice&conn=a")
	none(t, again)
	require.Equal(t, http.StatusNoContent, tb.shellPost(t, "alice", "/views/board/recover?conn=a"))
	assert.Equal(t, "shell-board-snapshot", next(t, again).name)
	assert.Equal(t, http.StatusNotFound, tb.shellPost(t, "alice", "/views/board/recover?conn=a"))
}

func TestShellLifelineReleasesOnCancelAndShutdown(t *testing.T) {
	tb := newTestBoard(t, DefaultConfig())
	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, "GET", tb.srv.URL+"/showcase/shell/lifeline?as=alice&conn=gone", http.NoBody)
	require.NoError(t, err)
	resp, err := tb.srv.Client().Do(req)
	require.NoError(t, err)
	require.NotNil(t, tb.app.broker.SubscriberTopics("gone"))
	cancel()
	_ = resp.Body.Close()
	require.Eventually(t, func() bool { return tb.app.broker.SubscriberTopics("gone") == nil }, 2*time.Second, time.Millisecond)
	assert.Error(t, tb.app.shell.owner(personas[0], "gone"))

	events := lifeline(t, tb, "alice", "a")
	tb.app.Shutdown()
	for range events {
	}
	assert.Nil(t, tb.app.broker.SubscriberTopics("a"))
}
