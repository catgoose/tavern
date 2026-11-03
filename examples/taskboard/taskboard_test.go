package main

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// logBuffer is a concurrency-safe log sink for asserting stream outcomes.
type logBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *logBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *logBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

type testBoard struct {
	app  *App
	srv  *httptest.Server
	logs *logBuffer
}

func newTestBoard(t *testing.T, cfg Config) *testBoard {
	t.Helper()
	logs := &logBuffer{}
	app := NewApp(cfg, slog.New(slog.NewTextHandler(logs, nil)))
	srv := httptest.NewServer(app)
	t.Cleanup(func() {
		select {
		case <-app.done:
		default:
			app.Shutdown()
		}
		srv.Close()
	})
	return &testBoard{app: app, srv: srv, logs: logs}
}

func (tb *testBoard) post(t *testing.T, path string, form url.Values) int {
	t.Helper()
	resp, err := tb.srv.Client().PostForm(tb.srv.URL+path, form)
	require.NoError(t, err)
	_ = resp.Body.Close()
	return resp.StatusCode
}

func (tb *testBoard) get(t *testing.T, path string) string {
	t.Helper()
	resp, err := tb.srv.Client().Get(tb.srv.URL + path)
	require.NoError(t, err)
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return string(body)
}

// event is one SSE event read by a client. A zero event marks the end of
// the stream.
type event struct{ name, data string }

// sseClient reads one /events stream in the background.
type sseClient struct {
	events chan event
	cancel context.CancelFunc
	status int
}

func (tb *testBoard) connect(t *testing.T, conn string) *sseClient {
	t.Helper()
	return dial(t, tb.srv.Client(), tb.srv.URL, conn)
}

// dial opens /events on base and reads it in the background.
func dial(t *testing.T, client *http.Client, base, conn string) *sseClient {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, "GET", base+"/events?conn="+conn, http.NoBody)
	require.NoError(t, err)
	resp, err := client.Do(req)
	require.NoError(t, err)
	c := &sseClient{events: make(chan event, 256), cancel: cancel, status: resp.StatusCode}
	t.Cleanup(func() { cancel(); _ = resp.Body.Close() })
	go func() {
		defer close(c.events)
		defer resp.Body.Close()
		scanner := bufio.NewScanner(resp.Body)
		var ev event
		for scanner.Scan() {
			line := scanner.Text()
			switch {
			case line == "":
				if ev.name != "" {
					c.events <- ev
				}
				ev = event{}
			case strings.HasPrefix(line, "event: "):
				ev.name = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				ev.data = strings.TrimPrefix(line, "data: ")
			case strings.HasPrefix(line, "retry: "):
				c.events <- event{name: "retry", data: strings.TrimPrefix(line, "retry: ")}
			}
		}
	}()
	return c
}

// next returns the next event, or a zero event once the stream has ended.
func (c *sseClient) next(t *testing.T) event {
	t.Helper()
	select {
	case ev := <-c.events:
		return ev
	case <-time.After(3 * time.Second):
		t.Fatal("no event within 3s")
		return event{}
	}
}

// expect reads until an event with the given name arrives.
func (c *sseClient) expect(t *testing.T, name string) event {
	t.Helper()
	for {
		ev := c.next(t)
		if ev.name == name {
			return ev
		}
		require.NotEmpty(t, ev.name, "stream ended before %q", name)
	}
}

// expectEnd reads until the stream ends and returns the events seen.
func (c *sseClient) expectEnd(t *testing.T) []event {
	t.Helper()
	var seen []event
	for {
		ev := c.next(t)
		if ev.name == "" {
			return seen
		}
		seen = append(seen, ev)
	}
}

func TestPageAndAssets(t *testing.T) {
	tb := newTestBoard(t, DefaultConfig())
	page := tb.get(t, "/")
	assert.Contains(t, page, `sse-connect="/events?conn=`)
	for _, asset := range []string{"/static/htmx-2.0.11.min.js", "/static/htmx-ext-sse-2.2.4.min.js", "/static/style.css"} {
		assert.Contains(t, page, asset)
		resp, err := tb.srv.Client().Get(tb.srv.URL + asset)
		require.NoError(t, err)
		_ = resp.Body.Close()
		assert.Equal(t, http.StatusOK, resp.StatusCode, asset)
	}
}

// TestMutationsReachEveryViewer checks the ready-then-fetch sequence and that
// both windows are told about each change.
func TestMutationsReachEveryViewer(t *testing.T) {
	tb := newTestBoard(t, DefaultConfig())
	alice, bob := tb.connect(t, "alice"), tb.connect(t, "bob")
	alice.expect(t, "ready")
	bob.expect(t, "ready")
	assert.Contains(t, tb.get(t, "/columns/todo"), "Nothing here")

	require.Equal(t, http.StatusNoContent, tb.post(t, "/tasks", url.Values{"title": {"Write docs"}, "name": {"Alice"}}))
	for _, c := range []*sseClient{alice, bob} {
		c.expect(t, "column-todo")
	}
	assert.Contains(t, tb.get(t, "/columns/todo"), "Write docs")
	assert.Contains(t, tb.get(t, "/activity"), `Alice added &#34;Write docs&#34;`)

	require.Equal(t, http.StatusNoContent, tb.post(t, "/tasks/1/move", url.Values{"to": {"doing"}, "name": {"Bob"}}))
	for _, c := range []*sseClient{alice, bob} {
		c.expect(t, "column-doing")
	}
	assert.NotContains(t, tb.get(t, "/columns/todo"), "Write docs")
	assert.Contains(t, tb.get(t, "/columns/doing"), "Write docs")

	assert.Equal(t, http.StatusBadRequest, tb.post(t, "/tasks", url.Values{"title": {" "}}))
	assert.Equal(t, http.StatusNotFound, tb.post(t, "/tasks/99/move", url.Values{"to": {"done"}}))
}

// TestLossEndsStreamAndReconnectRefetches pauses one window, overflows its
// queue and resumes it. The stream ends instead of skipping a change, and a
// reconnect gets "ready" and the full canonical board.
func TestLossEndsStreamAndReconnectRefetches(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Buffer = 4
	tb := newTestBoard(t, cfg)
	c := tb.connect(t, "slow")
	c.expect(t, "ready")

	require.Equal(t, http.StatusNoContent, tb.post(t, "/demo/pause?conn=slow", nil))
	require.Equal(t, http.StatusNoContent, tb.post(t, "/demo/sample", url.Values{"name": {"Sam"}}))
	require.Equal(t, http.StatusNoContent, tb.post(t, "/demo/resume?conn=slow", nil))
	seen := c.expectEnd(t)
	// The paused writer holds one invalidation; the queue holds the rest.
	assert.LessOrEqual(t, len(seen), cfg.Buffer+1, "only queued invalidations are delivered before the stream ends")
	require.Eventually(t, func() bool { return strings.Contains(tb.logs.String(), "reason=queue-loss") }, 2*time.Second, time.Millisecond)

	again := tb.connect(t, "slow")
	again.expect(t, "ready")
	todo := tb.get(t, "/columns/todo")
	for i := 1; i <= 10; i++ {
		assert.Contains(t, todo, fmt.Sprintf("Sample task %d<", i))
	}
}

// TestNonReadingClientIsReleased connects a client that never reads. Its
// queue overflows, the stream ends, and the server keeps no subscription.
func TestNonReadingClientIsReleased(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Buffer = 4
	cfg.WriteTimeout = 200 * time.Millisecond
	tb := newTestBoard(t, cfg)

	conn, err := net.Dial("tcp", tb.srv.Listener.Addr().String())
	require.NoError(t, err)
	defer conn.Close()
	_, err = fmt.Fprintf(conn, "GET /events?conn=stuck HTTP/1.1\r\nHost: x\r\n\r\n")
	require.NoError(t, err)
	require.Eventually(t, func() bool { return tb.app.broker.HasSubscribers(topicBoard) }, 2*time.Second, time.Millisecond)

	for i := 0; tb.app.broker.HasSubscribers(topicBoard); i++ {
		require.Less(t, i, 100000, "the stream never ended")
		tb.app.invalidate(uint64(i), "todo")
	}
	require.Eventually(t, func() bool { return strings.Contains(tb.logs.String(), "stream ended") }, 5*time.Second, time.Millisecond)
	assert.Zero(t, tb.app.broker.SubscriberCount())
}

func TestAdmissionLimit(t *testing.T) {
	cfg := DefaultConfig()
	cfg.MaxSubscribers = 1
	tb := newTestBoard(t, cfg)
	first := tb.connect(t, "one")
	first.expect(t, "ready")
	second := tb.connect(t, "two")
	assert.Equal(t, http.StatusServiceUnavailable, second.status)
}

// TestShutdownSendsRetryAndEndsStreams checks that Shutdown tells browsers
// to wait before reconnecting and ends every stream, including a paused one.
func TestShutdownSendsRetryAndEndsStreams(t *testing.T) {
	tb := newTestBoard(t, DefaultConfig())
	live, paused := tb.connect(t, "live"), tb.connect(t, "paused")
	live.expect(t, "ready")
	paused.expect(t, "ready")
	require.Equal(t, http.StatusNoContent, tb.post(t, "/demo/pause?conn=paused", nil))

	tb.app.Shutdown()
	assert.Equal(t, "3000", live.expect(t, "retry").data)
	live.expectEnd(t)
	paused.expectEnd(t)
	require.Eventually(t, func() bool { return strings.Count(tb.logs.String(), "reason=broker-closed") == 2 }, 2*time.Second, time.Millisecond)
}
