package main

import (
	"bufio"
	"context"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type sseEvent struct{ name, data string }

// openSSE opens an event stream at path and returns its events as they
// arrive. The channel closes when the stream ends.
func openSSE(t *testing.T, tb *testBoard, path string) <-chan sseEvent {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	req, err := http.NewRequestWithContext(ctx, "GET", tb.srv.URL+path, http.NoBody)
	require.NoError(t, err)
	events := make(chan sseEvent, 64)
	go func() {
		defer close(events)
		resp, err := tb.srv.Client().Do(req)
		if err != nil {
			return
		}
		defer resp.Body.Close()
		scanner := bufio.NewScanner(resp.Body)
		scanner.Buffer(nil, 1<<20)
		var ev sseEvent
		var data []string
		for scanner.Scan() {
			line := scanner.Text()
			switch {
			case line == "":
				if ev.name != "" {
					ev.data = strings.Join(data, "\n")
					events <- ev
				}
				ev, data = sseEvent{}, nil
			case strings.HasPrefix(line, "event: "):
				ev.name = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				data = append(data, strings.TrimPrefix(line, "data: "))
			}
		}
	}()
	return events
}

func next(t *testing.T, events <-chan sseEvent) sseEvent {
	t.Helper()
	select {
	case ev, ok := <-events:
		require.True(t, ok, "stream ended")
		return ev
	case <-time.After(3 * time.Second):
		t.Fatal("no event within 3s")
		return sseEvent{}
	}
}

// none checks that no event arrives within a short wait.
func none(t *testing.T, events <-chan sseEvent) {
	t.Helper()
	select {
	case ev := <-events:
		t.Fatalf("unexpected event %s: %s", ev.name, ev.data)
	case <-time.After(150 * time.Millisecond):
	}
}

func (tb *testBoard) as(t *testing.T, who, path string, form url.Values) int {
	t.Helper()
	if form == nil {
		form = url.Values{}
	}
	form.Set("as", who)
	return tb.post(t, "/showcase/collaboration"+path, form)
}

func collabStream(t *testing.T, tb *testBoard, who string) <-chan sseEvent {
	t.Helper()
	events := openSSE(t, tb, "/showcase/collaboration/events?as="+who+"&conn="+who)
	snap := next(t, events)
	require.Equal(t, "board", snap.name)
	return events
}

// findTask returns the ID of the seeded or created task with the title.
func findTask(t *testing.T, tb *testBoard, title string) string {
	t.Helper()
	tb.app.collab.mu.Lock()
	defer tb.app.collab.mu.Unlock()
	for _, task := range tb.app.collab.tasks {
		if task.Title == title {
			return cardID(task.ID)[len("collab-task-"):]
		}
	}
	t.Fatalf("no task %q", title)
	return ""
}

func TestCollabSnapshotsFollowVisibility(t *testing.T) {
	tb := newTestBoard(t, DefaultConfig())
	alice := openSSE(t, tb, "/showcase/collaboration/events?as=alice&conn=a")
	bob := openSSE(t, tb, "/showcase/collaboration/events?as=bob&conn=b")

	snap := next(t, alice)
	assert.Equal(t, "board", snap.name)
	assert.Contains(t, snap.data, "Plan the reorg")
	for _, id := range []string{"collab-col-todo", "collab-col-doing", "collab-col-done", "collab-inbox"} {
		assert.Contains(t, snap.data, `<div id="`+id+`" hx-swap-oob="outerHTML">`)
	}

	snap = next(t, bob)
	assert.Contains(t, snap.data, "Write the release notes")
	assert.NotContains(t, snap.data, "Plan the reorg", "internal tasks are not in a contributor's snapshot")
	assert.NotContains(t, snap.data, "Internal")
}

func TestCollabCRUDReachesEachViewer(t *testing.T) {
	tb := newTestBoard(t, DefaultConfig())
	alice, bob := collabStream(t, tb, "alice"), collabStream(t, tb, "bob")

	require.Equal(t, http.StatusNoContent, tb.as(t, "bob", "/tasks", url.Values{"title": {"Ship <b>it</b> & tell"}}))
	for _, s := range []<-chan sseEvent{alice, bob} {
		ev := next(t, s)
		assert.Contains(t, ev.data, `<div id="collab-col-todo" hx-swap-oob="beforeend"><div id="collab-task-5">`, "Append to the column")
		assert.Contains(t, ev.data, "Ship &lt;b&gt;it&lt;/b&gt; &amp; tell", "user text is escaped")
		assert.NotContains(t, ev.data, "<b>it</b>")
	}
	id := findTask(t, tb, "Ship <b>it</b> & tell")

	require.Equal(t, http.StatusOK, tb.as(t, "alice", "/tasks/"+id+"/title", url.Values{"title": {"Ship it"}}))
	for _, s := range []<-chan sseEvent{alice, bob} {
		ev := next(t, s)
		assert.True(t, strings.HasPrefix(ev.data, `<div id="collab-task-`+id+`" hx-swap-oob="outerHTML"><article`), "Replace the card: %s", ev.data)
	}

	require.Equal(t, http.StatusNoContent, tb.as(t, "alice", "/tasks/"+id+"/move", url.Values{"to": {"doing"}}))
	for _, s := range []<-chan sseEvent{alice, bob} {
		ev := next(t, s)
		assert.True(t, strings.HasPrefix(ev.data, `<div id="collab-task-`+id+`" hx-swap-oob="delete"></div><div id="collab-col-doing" hx-swap-oob="beforeend">`), "Delete then Append: %s", ev.data)
	}

	require.Equal(t, http.StatusNoContent, tb.as(t, "bob", "/tasks/"+id+"/delete", nil))
	for _, s := range []<-chan sseEvent{alice, bob} {
		assert.Equal(t, `<div id="collab-task-`+id+`" hx-swap-oob="delete"></div>`, next(t, s).data)
	}

	// An internal task reaches only the lead.
	require.Equal(t, http.StatusNoContent, tb.as(t, "alice", "/tasks", url.Values{"title": {"Budget"}, "internal": {"on"}}))
	assert.Contains(t, next(t, alice).data, "Budget")
	none(t, bob)
	internal := findTask(t, tb, "Budget")
	assert.Equal(t, http.StatusNotFound, tb.as(t, "bob", "/tasks/"+internal+"/move", url.Values{"to": {"done"}}))
	assert.Equal(t, http.StatusForbidden, tb.as(t, "alice", "/tasks/"+internal+"/assign", url.Values{"to": {"bob"}}))
	assert.Equal(t, http.StatusForbidden, tb.as(t, "bob", "/tasks", url.Values{"title": {"Secret"}, "internal": {"on"}}))
	none(t, alice)
	none(t, bob)
}

func TestCollabInboxIsScopedAndFiltered(t *testing.T) {
	tb := newTestBoard(t, DefaultConfig())
	alice, bob := collabStream(t, tb, "alice"), collabStream(t, tb, "bob")
	review := findTask(t, tb, "Review the upgrade guide") // created by Bob

	require.Equal(t, http.StatusNoContent, tb.as(t, "alice", "/tasks/"+review+"/assign", url.Values{"to": {"bob"}}))
	next(t, alice) // card
	next(t, bob)   // card
	ev := next(t, bob)
	assert.Equal(t, "inbox-assigned", ev.name)
	assert.Contains(t, ev.data, `<div id="collab-inbox" hx-swap-oob="afterbegin">`)
	assert.Contains(t, ev.data, "Alice assigned &#34;Review the upgrade guide&#34; to you")
	none(t, alice) // the notice was published to Bob's scope only

	// Bob created the task, but contributors do not take "completed"
	// notices: the filter keeps it off his stream.
	require.Equal(t, http.StatusNoContent, tb.as(t, "alice", "/tasks/"+review+"/move", url.Values{"to": {"done"}}))
	next(t, alice)
	assert.Equal(t, "board", next(t, bob).name)
	none(t, bob)

	// Alice does take them.
	notes := findTask(t, tb, "Write the release notes") // created by Alice
	require.Equal(t, http.StatusNoContent, tb.as(t, "bob", "/tasks/"+notes+"/move", url.Values{"to": {"done"}}))
	next(t, alice)
	next(t, bob)
	ev = next(t, alice)
	assert.Equal(t, "inbox-completed", ev.name)
	assert.Contains(t, ev.data, "Bob finished &#34;Write the release notes&#34;, which you created")

	// A new stream's inbox follows the same rules.
	fresh := openSSE(t, tb, "/showcase/collaboration/events?as=bob&conn=b2")
	snap := next(t, fresh).data
	assert.Contains(t, snap, "assigned &#34;Review the upgrade guide&#34; to you")
	assert.NotContains(t, snap, "finished")
}

func TestCollabFeedIsADynamicGroup(t *testing.T) {
	tb := newTestBoard(t, DefaultConfig())
	require.Equal(t, http.StatusNoContent, tb.as(t, "alice", "/tasks", url.Values{"title": {"Budget"}, "internal": {"on"}}))
	require.Equal(t, http.StatusNoContent, tb.as(t, "bob", "/tasks", url.Values{"title": {"Docs"}}))

	got := map[string]string{}
	bob := openSSE(t, tb, "/showcase/collaboration/feed?as=bob")
	for range 2 {
		ev := next(t, bob)
		got[ev.name] = ev.data
	}
	require.Contains(t, got, "collab/activity/contributor")
	require.Contains(t, got, "collab/stats/contributor")
	assert.Contains(t, got["collab/activity/contributor"], "Bob added &#34;Docs&#34;")
	assert.NotContains(t, got["collab/activity/contributor"], "Budget")
	assert.Contains(t, got["collab/stats/contributor"], `<strong data-column="todo">2</strong>`)

	alice := openSSE(t, tb, "/showcase/collaboration/feed?as=alice")
	got = map[string]string{}
	for range 2 {
		ev := next(t, alice)
		got[ev.name] = ev.data
	}
	assert.Contains(t, got["collab/activity/lead"], "Alice added &#34;Budget&#34;")
	assert.Contains(t, got["collab/stats/lead"], `<strong data-column="todo">4</strong>`)

	// The group's topics come from the persona, never from the request.
	resp, err := tb.srv.Client().Get(tb.srv.URL + "/showcase/collaboration/feed?as=mallory&topics=collab/activity/lead")
	require.NoError(t, err)
	_ = resp.Body.Close()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

// TestCollabBulkClearIsOneBatch checks that clearing finished tasks reaches
// each viewer as one event holding every removal it may see.
func TestCollabBulkClearIsOneBatch(t *testing.T) {
	tb := newTestBoard(t, DefaultConfig())
	for _, title := range []string{"Write the release notes", "Plan the reorg (leads only)"} {
		require.Equal(t, http.StatusNoContent, tb.as(t, "alice", "/tasks/"+findTask(t, tb, title)+"/move", url.Values{"to": {"done"}}))
	}
	alice, bob := collabStream(t, tb, "alice"), collabStream(t, tb, "bob")

	require.Equal(t, http.StatusNoContent, tb.as(t, "alice", "/clear-done", nil))
	ev := next(t, alice)
	assert.Equal(t, 3, strings.Count(ev.data, `hx-swap-oob="delete"`), "the lead's three removals arrive together: %s", ev.data)
	ev = next(t, bob)
	assert.Equal(t, 2, strings.Count(ev.data, `hx-swap-oob="delete"`), "the contributor's two removals arrive together: %s", ev.data)
	assert.NotContains(t, ev.data, `collab-task-3"`, "the internal task's removal is not sent to a contributor")
	none(t, alice)
	none(t, bob)

	feed := openSSE(t, tb, "/showcase/collaboration/feed?as=bob")
	for range 2 {
		if ev := next(t, feed); ev.name == "collab/activity/contributor" {
			assert.Contains(t, ev.data, "Alice cleared 2 finished tasks", "a contributor is told only about tasks they could see")
		}
	}
}

// TestCollabChangeDuringSnapshotIsNotLost holds a new stream inside its
// snapshot and makes a change meanwhile. The change waits for the
// subscription and then arrives live, after the snapshot that lacks it.
func TestCollabChangeDuringSnapshotIsNotLost(t *testing.T) {
	tb := newTestBoard(t, DefaultConfig())
	inSnapshot, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	tb.app.collab.mu.Lock()
	tb.app.collab.beforeSnapshot = func() {
		once.Do(func() {
			close(inSnapshot)
			<-release
		})
	}
	tb.app.collab.mu.Unlock()

	events := openSSE(t, tb, "/showcase/collaboration/events?as=bob&conn=b")
	<-inSnapshot
	created := make(chan int, 1)
	go func() { created <- tb.as(t, "alice", "/tasks", url.Values{"title": {"Raced"}}) }()
	select {
	case <-created:
		t.Fatal("the change finished while the snapshot was being taken")
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	assert.Equal(t, http.StatusNoContent, <-created)

	snap := next(t, events)
	assert.NotContains(t, snap.data, "Raced")
	assert.Contains(t, next(t, events).data, "Raced")
}

// TestCollabReconnectStartsFromCanonicalState drops a stream, changes the
// board while its reconnect is held, and checks the new snapshot.
func TestCollabReconnectStartsFromCanonicalState(t *testing.T) {
	tb := newTestBoard(t, DefaultConfig())
	first := collabStream(t, tb, "alice")
	require.Equal(t, http.StatusNoContent, tb.post(t, "/showcase/collaboration/drop?conn=alice", nil))
	_, open := <-first
	assert.False(t, open, "the dropped stream ends")

	again := openSSE(t, tb, "/showcase/collaboration/events?as=alice&conn=alice")
	none(t, again) // held until resume
	review := findTask(t, tb, "Review the upgrade guide")
	require.Equal(t, http.StatusNoContent, tb.as(t, "bob", "/tasks/"+review+"/delete", nil))
	require.Equal(t, http.StatusOK, tb.as(t, "bob", "/tasks/"+findTask(t, tb, "Write the release notes")+"/title", url.Values{"title": {"Notes v2"}}))
	require.Equal(t, http.StatusNoContent, tb.post(t, "/showcase/collaboration/resume?conn=alice", nil))

	snap := next(t, again)
	assert.Equal(t, "board", snap.name)
	assert.NotContains(t, snap.data, "Review the upgrade guide", "a deleted task stays deleted")
	assert.Equal(t, 1, strings.Count(snap.data, "Notes v2"))
	assert.NotContains(t, snap.data, "Write the release notes")
	none(t, again) // nothing old is replayed after the snapshot
}

func TestCollabRejectsMalformedInput(t *testing.T) {
	tb := newTestBoard(t, DefaultConfig())
	long := strings.Repeat("x", collabTitleMax+1)
	for _, c := range []struct {
		who, path string
		form      url.Values
		want      int
	}{
		{"mallory", "/tasks", url.Values{"title": {"x"}}, http.StatusBadRequest},
		{"bob", "/tasks", url.Values{"title": {"  "}}, http.StatusBadRequest},
		{"bob", "/tasks", url.Values{"title": {long}}, http.StatusBadRequest},
		{"bob", "/tasks/abc/move", url.Values{"to": {"done"}}, http.StatusBadRequest},
		{"bob", "/tasks/0/move", url.Values{"to": {"done"}}, http.StatusBadRequest},
		{"bob", "/tasks/1/move", url.Values{"to": {"nowhere"}}, http.StatusBadRequest},
		{"bob", "/tasks/99/move", url.Values{"to": {"done"}}, http.StatusNotFound},
		{"bob", "/tasks/1/assign", url.Values{"to": {"mallory"}}, http.StatusBadRequest},
		{"bob", "/tasks/99/delete", nil, http.StatusNotFound},
	} {
		assert.Equal(t, c.want, tb.as(t, c.who, c.path, c.form), "%s %s %v", c.who, c.path, c.form)
	}
	assert.Equal(t, http.StatusNotFound, tb.post(t, "/showcase/collaboration/drop?conn=nobody", nil))
	assert.Equal(t, http.StatusNotFound, tb.post(t, "/showcase/collaboration/resume?conn=nobody", nil))
	assert.Contains(t, tb.get(t, "/showcase/collaboration"), "Open as Alice")
}
