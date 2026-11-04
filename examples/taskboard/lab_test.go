package main

import (
	"io"
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

// command posts a tavern-js style command and returns the status and body.
func (tb *testBoard) command(t *testing.T, path, contentType, body string) (status int, respBody string) {
	t.Helper()
	resp, err := tb.srv.Client().Post(tb.srv.URL+path, contentType, strings.NewReader(body))
	require.NoError(t, err)
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp.StatusCode, string(b)
}

func (tb *testBoard) labTask(t *testing.T, id int) labTask {
	t.Helper()
	tb.app.lab.mu.Lock()
	defer tb.app.lab.mu.Unlock()
	task, ok := tb.app.lab.tasks[id]
	require.True(t, ok, "task %d", id)
	return *task
}

func TestLabCommandsValidateJSON(t *testing.T) {
	tb := newTestBoard(t, DefaultConfig())
	const vote = "/showcase/lab/tasks/1/vote?as=alice"
	for _, c := range []struct {
		name, path, ct, body string
		want                 int
	}{
		{"form encoding", vote, "application/x-www-form-urlencoded", "id=1", http.StatusUnsupportedMediaType},
		{"malformed", vote, "application/json", `{"id":`, http.StatusBadRequest},
		{"unknown field", vote, "application/json", `{"id":"1","votes":"99"}`, http.StatusBadRequest},
		{"two objects", vote, "application/json", `{"id":"1"}{"id":"1"}`, http.StatusBadRequest},
		{"other task", vote, "application/json", `{"id":"2"}`, http.StatusBadRequest},
		{"too large", vote, "application/json", `{"id":"` + strings.Repeat("1", labCommandMax) + `"}`, http.StatusRequestEntityTooLarge},
		{"no persona", "/showcase/lab/tasks/1/vote", "application/json", `{"id":"1"}`, http.StatusBadRequest},
		{"internal task for a contributor", "/showcase/lab/tasks/3/vote?as=bob", "application/json", `{"id":"3"}`, http.StatusNotFound},
		{"burst count missing", "/showcase/lab/burst?as=alice", "application/json", `{}`, http.StatusBadRequest},
		{"burst count too big", "/showcase/lab/burst?as=alice", "application/json", `{"count":"21"}`, http.StatusBadRequest},
		{"burst count zero", "/showcase/lab/burst?as=alice", "application/json", `{"count":"0"}`, http.StatusBadRequest},
	} {
		status, _ := tb.command(t, c.path, c.ct, c.body)
		assert.Equal(t, c.want, status, c.name)
	}
	assert.Zero(t, tb.labTask(t, 1).Votes, "no rejected command changed the task")

	status, _ := tb.command(t, vote, "application/json; charset=utf-8", `{"id":"1"}`)
	assert.Equal(t, http.StatusNoContent, status)
	assert.Equal(t, 1, tb.labTask(t, 1).Votes)
}

// TestLabSavesFromOneRevisionConflict races two saves from the same title
// revision: exactly one succeeds, and the other is told about it.
func TestLabSavesFromOneRevisionConflict(t *testing.T) {
	tb := newTestBoard(t, DefaultConfig())
	for round := range 25 {
		rev := tb.labTask(t, 1).TitleRev
		var wg sync.WaitGroup
		statuses := make([]int, 2)
		bodies := make([]string, 2)
		for i, who := range []string{"alice", "bob"} {
			wg.Go(func() {
				form := url.Values{"as": {who}, "rev": {strconv.Itoa(rev)}, "title": {who + " " + strconv.Itoa(round)}}
				resp, err := tb.srv.Client().PostForm(tb.srv.URL+"/showcase/lab/tasks/1/title", form)
				if err != nil {
					return
				}
				defer resp.Body.Close()
				b, _ := io.ReadAll(resp.Body)
				statuses[i], bodies[i] = resp.StatusCode, string(b)
			})
		}
		wg.Wait()
		ok := 0
		for i, s := range statuses {
			switch s {
			case http.StatusOK:
				ok++
			case http.StatusConflict:
				winner := tb.labTask(t, 1).Title
				assert.Contains(t, bodies[i], "Someone saved “"+winner+"”")
				assert.Contains(t, bodies[i], `name="rev" value="`+strconv.Itoa(rev+1)+`"`, "the conflict offers the current revision")
			default:
				t.Fatalf("round %d: unexpected status %d", round, s)
			}
		}
		require.Equal(t, 1, ok, "round %d: exactly one save wins", round)
		assert.Equal(t, rev+1, tb.labTask(t, 1).TitleRev)
	}
}

func TestLabSaveAfterDeleteKeepsDraftAndDoesNotRecreate(t *testing.T) {
	tb := newTestBoard(t, DefaultConfig())
	rev := tb.labTask(t, 2).TitleRev
	require.Equal(t, http.StatusOK, tb.post(t, "/showcase/lab/tasks/2/delete", url.Values{"as": {"bob"}}))

	form := url.Values{"as": {"alice"}, "rev": {strconv.Itoa(rev)}, "title": {"My <draft>"}}
	resp, err := tb.srv.Client().PostForm(tb.srv.URL+"/showcase/lab/tasks/2/title", form)
	require.NoError(t, err)
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
	assert.Contains(t, string(body), "This task was deleted")
	assert.Contains(t, string(body), `value="My &lt;draft&gt;"`, "the draft is kept, escaped")

	tb.app.lab.mu.Lock()
	_, exists := tb.app.lab.tasks[2]
	tb.app.lab.mu.Unlock()
	assert.False(t, exists, "a save does not recreate a deleted task")

	form.Set("rev", "1")
	form.Set("title", " ")
	resp, err = tb.srv.Client().PostForm(tb.srv.URL+"/showcase/lab/tasks/1/title", form)
	require.NoError(t, err)
	body, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	assert.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode)
	assert.Contains(t, string(body), "give the task a title")
}

func TestLabResyncReachesOnlyItsOwnStream(t *testing.T) {
	tb := newTestBoard(t, DefaultConfig())
	alice := openSSE(t, tb, "/showcase/lab/editor/events?as=alice&conn=a")
	bob := openSSE(t, tb, "/showcase/lab/editor/events?as=bob&conn=b")
	snap := next(t, alice)
	assert.Equal(t, "lab-snapshot", snap.name)
	assert.Contains(t, snap.data, "Decide the budget")
	snap = next(t, bob)
	assert.NotContains(t, snap.data, "Decide the budget", "internal tasks are not in a contributor's snapshot")

	status, _ := tb.command(t, "/showcase/lab/resync?as=bob&conn=a&region=editor", "application/json", `{}`)
	assert.Equal(t, http.StatusNotFound, status, "a window can only resync its own streams")
	status, _ = tb.command(t, "/showcase/lab/resync?as=alice&conn=a&region=nowhere", "application/json", `{}`)
	assert.Equal(t, http.StatusNotFound, status)

	status, _ = tb.command(t, "/showcase/lab/resync?as=alice&conn=a&region=editor", "application/json", `{}`)
	require.Equal(t, http.StatusNoContent, status)
	assert.Equal(t, "lab-snapshot", next(t, alice).name)
	none(t, bob)
}

func TestLabUpdatesFollowVisibilityAndEscape(t *testing.T) {
	tb := newTestBoard(t, DefaultConfig())
	alice := openSSE(t, tb, "/showcase/lab/votes/events?as=alice&conn=a")
	bob := openSSE(t, tb, "/showcase/lab/votes/events?as=bob&conn=b")
	next(t, alice)
	next(t, bob)

	require.Equal(t, http.StatusNoContent, tb.post(t, "/showcase/lab/tasks", url.Values{"as": {"bob"}, "title": {"<b>bold</b>"}}))
	for _, s := range []<-chan sseEvent{alice, bob} {
		ev := next(t, s)
		assert.Equal(t, "lab-update", ev.name)
		assert.Contains(t, ev.data, "&lt;b&gt;bold&lt;/b&gt;")
		assert.NotContains(t, ev.data, "<b>bold")
	}

	status, _ := tb.command(t, "/showcase/lab/tasks/3/vote?as=alice", "application/json", `{"id":"3"}`)
	require.Equal(t, http.StatusNoContent, status)
	assert.Contains(t, next(t, alice).data, "Decide the budget")
	none(t, bob)
}

func TestLabBurstIsBoundedAndStopsAtShutdown(t *testing.T) {
	tb := newTestBoard(t, DefaultConfig())
	start := func() int {
		status, _ := tb.command(t, "/showcase/lab/burst?as=alice", "application/json", `{"count":"20"}`)
		return status
	}
	require.Equal(t, http.StatusNoContent, start())
	assert.Equal(t, http.StatusConflict, start(), "one burst at a time")
	tb.app.Shutdown()
	require.Eventually(t, func() bool {
		tb.app.lab.mu.Lock()
		defer tb.app.lab.mu.Unlock()
		return !tb.app.lab.burst
	}, 2*time.Second, 5*time.Millisecond)
	votes := 0
	for id := 1; id <= 2; id++ {
		votes += tb.labTask(t, id).Votes
	}
	assert.Less(t, votes, 20, "the burst stopped early")
}

