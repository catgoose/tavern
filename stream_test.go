package tavern

import (
	"context"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// nonFlushResponseWriter is a minimal http.ResponseWriter that deliberately
// does NOT implement http.Flusher. Used to exercise the Flusher check in
// StreamSSE; net/http/httptest.ResponseRecorder cannot be used for this
// purpose because it implements Flush() itself.
type nonFlushResponseWriter struct {
	header http.Header
	body   strings.Builder
	code   int
}

func newNonFlushResponseWriter() *nonFlushResponseWriter {
	return &nonFlushResponseWriter{header: make(http.Header), code: http.StatusOK}
}

func (n *nonFlushResponseWriter) Header() http.Header         { return n.header }
func (n *nonFlushResponseWriter) Write(b []byte) (int, error) { return n.body.Write(b) }
func (n *nonFlushResponseWriter) WriteHeader(code int)        { n.code = code }

// waitFor polls cond every 2ms until it returns true or timeout elapses.
func waitFor(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("waitFor: condition not met within %s", timeout)
}

func TestStreamSSE_StreamsString(t *testing.T) {
	t.Parallel()

	ch := make(chan string, 3)
	ch <- NewSSEMessage("ping", "one").String()
	ch <- NewSSEMessage("ping", "two").String()
	ch <- NewSSEMessage("ping", "three").String()

	rec := newFlushRecorder()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- StreamSSE(ctx, rec, ch, func(s string) string { return s })
	}()

	waitFor(t, time.Second, func() bool {
		return strings.Count(rec.Body.String(), "data: three") == 1
	})
	cancel()
	require.NoError(t, <-done)

	body := rec.Body.String()
	assert.Contains(t, body, "data: one")
	assert.Contains(t, body, "data: two")
	assert.Contains(t, body, "data: three")
}

func TestStreamSSE_StreamsTopicMessage(t *testing.T) {
	t.Parallel()

	ch := make(chan TopicMessage, 2)
	ch <- TopicMessage{Topic: "orders", Data: "created"}
	ch <- TopicMessage{Topic: "invoices", Data: "paid"}

	rec := newFlushRecorder()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- StreamSSE(ctx, rec, ch, func(tm TopicMessage) string {
			return NewSSEMessage(tm.Topic, tm.Data).String()
		})
	}()

	waitFor(t, time.Second, func() bool {
		return strings.Contains(rec.Body.String(), "event: invoices")
	})
	cancel()
	require.NoError(t, <-done)

	body := rec.Body.String()
	assert.Contains(t, body, "event: orders\ndata: created")
	assert.Contains(t, body, "event: invoices\ndata: paid")
}

func TestStreamSSE_ContextCancellation(t *testing.T) {
	t.Parallel()

	ch := make(chan string)
	rec := newFlushRecorder()
	ctx, cancel := context.WithCancel(t.Context())

	done := make(chan error, 1)
	go func() {
		done <- StreamSSE(ctx, rec, ch, func(s string) string { return s })
	}()

	cancel()

	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("StreamSSE did not return after context cancellation")
	}
}

func TestStreamSSE_ChannelClose(t *testing.T) {
	t.Parallel()

	ch := make(chan string)
	rec := newFlushRecorder()

	done := make(chan error, 1)
	go func() {
		done <- StreamSSE(t.Context(), rec, ch, func(s string) string { return s })
	}()

	close(ch)

	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("StreamSSE did not return after channel close")
	}
}

func TestStreamSSE_MissingFlusher(t *testing.T) {
	t.Parallel()

	// httptest.ResponseRecorder implements http.Flusher, so we use a custom
	// writer that intentionally does not, to exercise the Flusher check.
	rec := newNonFlushResponseWriter()
	ch := make(chan string)

	err := StreamSSE(context.Background(), rec, ch, func(s string) string { return s })
	require.Error(t, err)
	assert.Contains(t, err.Error(), "http.Flusher")

	// Headers must not have been set when flusher check fails.
	assert.Empty(t, rec.Header().Get("Content-Type"))
	assert.Empty(t, rec.Header().Get("Cache-Control"))
	assert.Empty(t, rec.Header().Get("Connection"))
}

func TestStreamSSE_HeadersSet(t *testing.T) {
	t.Parallel()

	rec := newFlushRecorder()
	ch := make(chan string)
	ctx, cancel := context.WithCancel(t.Context())

	done := make(chan error, 1)
	go func() {
		done <- StreamSSE(ctx, rec, ch, func(s string) string { return s })
	}()

	waitFor(t, time.Second, func() bool {
		return rec.Header().Get("Content-Type") == "text/event-stream"
	})

	assert.Equal(t, "text/event-stream", rec.Header().Get("Content-Type"))
	assert.Equal(t, "no-cache", rec.Header().Get("Cache-Control"))
	assert.Equal(t, "keep-alive", rec.Header().Get("Connection"))

	cancel()
	<-done
}

func TestStreamSSE_Snapshot(t *testing.T) {
	t.Parallel()

	ch := make(chan string, 1)
	ch <- NewSSEMessage("update", "live").String()

	rec := newFlushRecorder()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	snapshot := NewSSEMessage("snapshot", "initial").String()
	done := make(chan error, 1)
	go func() {
		done <- StreamSSE(ctx, rec, ch, func(s string) string { return s },
			WithStreamSnapshot(func() string { return snapshot }),
		)
	}()

	waitFor(t, time.Second, func() bool {
		return strings.Contains(rec.Body.String(), "data: live")
	})
	cancel()
	require.NoError(t, <-done)

	body := rec.Body.String()
	snapshotIdx := strings.Index(body, "data: initial")
	liveIdx := strings.Index(body, "data: live")
	require.GreaterOrEqual(t, snapshotIdx, 0, "snapshot not written")
	require.GreaterOrEqual(t, liveIdx, 0, "live frame not written")
	assert.Less(t, snapshotIdx, liveIdx, "snapshot should precede live frame")
}

func TestStreamSSE_SnapshotEmpty(t *testing.T) {
	t.Parallel()

	ch := make(chan string, 1)
	ch <- NewSSEMessage("event", "hello").String()

	rec := newFlushRecorder()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	var snapshotCalls int32
	done := make(chan error, 1)
	go func() {
		done <- StreamSSE(ctx, rec, ch, func(s string) string { return s },
			WithStreamSnapshot(func() string {
				atomic.AddInt32(&snapshotCalls, 1)
				return ""
			}),
		)
	}()

	waitFor(t, time.Second, func() bool {
		return strings.Contains(rec.Body.String(), "data: hello")
	})
	cancel()
	require.NoError(t, <-done)

	assert.Equal(t, int32(1), atomic.LoadInt32(&snapshotCalls))
	// Body should contain exactly one event — the live one.
	assert.Equal(t, 1, strings.Count(rec.Body.String(), "event: event"))
}

func TestStreamSSE_Heartbeat(t *testing.T) {
	t.Parallel()

	ch := make(chan string)
	rec := newFlushRecorder()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- StreamSSE(ctx, rec, ch, func(s string) string { return s },
			WithStreamHeartbeat(10*time.Millisecond),
		)
	}()

	waitFor(t, time.Second, func() bool {
		return strings.Count(rec.Body.String(), ": keepalive\n\n") >= 2
	})
	cancel()
	require.NoError(t, <-done)

	assert.GreaterOrEqual(t, strings.Count(rec.Body.String(), ": keepalive\n\n"), 2)
}

func TestStreamSSE_EncoderEmptySkips(t *testing.T) {
	t.Parallel()

	ch := make(chan int, 4)
	ch <- 1
	ch <- 2 // skipped
	ch <- 3
	ch <- 4 // skipped

	rec := newFlushRecorder()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	encode := func(n int) string {
		if n%2 == 0 {
			return ""
		}
		return NewSSEMessage("odd", string(rune('0'+n))).String()
	}

	done := make(chan error, 1)
	go func() {
		done <- StreamSSE(ctx, rec, ch, encode)
	}()

	waitFor(t, time.Second, func() bool {
		return strings.Contains(rec.Body.String(), "data: 3")
	})
	cancel()
	require.NoError(t, <-done)

	body := rec.Body.String()
	assert.Contains(t, body, "data: 1")
	assert.Contains(t, body, "data: 3")
	assert.NotContains(t, body, "data: 2")
	assert.NotContains(t, body, "data: 4")
	// Exactly two odd events written.
	assert.Equal(t, 2, strings.Count(body, "event: odd"))
}

func TestStreamSSE_CustomWriter(t *testing.T) {
	t.Parallel()

	ch := make(chan string, 2)
	ch <- "frame-a"
	ch <- "frame-b"

	rec := newFlushRecorder()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	var calls int32
	custom := func(w http.ResponseWriter, msg string) error {
		atomic.AddInt32(&calls, 1)
		_, err := w.Write([]byte("custom:" + msg + "|"))
		if err != nil {
			return err
		}
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		return nil
	}

	done := make(chan error, 1)
	go func() {
		done <- StreamSSE(ctx, rec, ch, func(s string) string { return s },
			WithStreamWriter(custom),
		)
	}()

	waitFor(t, time.Second, func() bool {
		return atomic.LoadInt32(&calls) >= 2
	})
	cancel()
	require.NoError(t, <-done)

	assert.GreaterOrEqual(t, atomic.LoadInt32(&calls), int32(2))
	body := rec.Body.String()
	assert.Contains(t, body, "custom:frame-a|")
	assert.Contains(t, body, "custom:frame-b|")
}

func TestStreamSSE_NilEncode(t *testing.T) {
	t.Parallel()

	rec := newFlushRecorder()
	ch := make(chan string)

	err := StreamSSE(context.Background(), rec, ch, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "encode")
}
