package main

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestThroughProxy drives a running task board through a reverse proxy. It
// needs the environment that deploy/smoke.sh (make proxy-smoke) sets up.
func TestThroughProxy(t *testing.T) {
	base := os.Getenv("TASKBOARD_PROXY_URL")
	if base == "" {
		t.Skip("run with make proxy-smoke, which starts the app behind nginx")
	}
	pid, err := strconv.Atoi(os.Getenv("TASKBOARD_APP_PID"))
	require.NoError(t, err, "TASKBOARD_APP_PID")
	client := &http.Client{}
	post := func(path string, form url.Values) {
		t.Helper()
		resp, err := client.PostForm(base+path, form)
		require.NoError(t, err)
		_ = resp.Body.Close()
		require.Equal(t, http.StatusNoContent, resp.StatusCode, path)
	}
	get := func(path string) string {
		t.Helper()
		resp, err := client.Get(base + path)
		require.NoError(t, err)
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		return string(body)
	}

	// The first frame arrives while the connection stays open, so the proxy
	// is not buffering the stream.
	c := dial(t, client, base, "proxy")
	c.expect(t, "ready")

	post("/tasks", url.Values{"title": {"Through the proxy"}, "name": {"Pat"}})
	c.expect(t, "column-todo")

	// Stay idle past nginx's 3s proxy_read_timeout; heartbeats keep the
	// stream open, so a later change still arrives on it.
	idle := time.NewTimer(4500 * time.Millisecond)
	<-idle.C
	post("/tasks", url.Values{"title": {"After idling"}, "name": {"Pat"}})
	c.expect(t, "column-todo")

	// Overflow a paused stream: it ends, and a reconnect gets the full board.
	post("/demo/pause?conn=proxy", nil)
	post("/demo/sample", url.Values{"name": {"Pat"}})
	post("/demo/resume?conn=proxy", nil)
	c.expectEnd(t)
	again := dial(t, client, base, "proxy")
	again.expect(t, "ready")
	todo := get("/columns/todo")
	for i := 1; i <= 10; i++ {
		assert.Contains(t, todo, fmt.Sprintf("Sample task %d<", i))
	}
	assert.True(t, strings.Contains(todo, "After idling"))

	// Graceful shutdown reaches the browser through the proxy: a retry
	// hint, then the end of the stream.
	proc, err := os.FindProcess(pid)
	require.NoError(t, err)
	require.NoError(t, proc.Signal(syscall.SIGTERM))
	assert.Equal(t, "3000", again.expect(t, "retry").data)
	again.expectEnd(t)
}
