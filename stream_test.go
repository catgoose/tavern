package tavern

import (
	"context"
	"net/http"
	"strings"
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

// The tests in this file deliberately avoid concurrent access to the
// flushRecorder. StreamSSE writes to rec.Header() and rec.Body; reading
// those from the main goroutine while StreamSSE runs in a background
// goroutine would race under -race. The pattern used here is:
//
//   - Pre-fill and close the channel, then call StreamSSE synchronously.
//     The loop drains the buffered values and returns on the channel close.
//   - Only tests that genuinely need mid-stream timing (Heartbeat,
//     ContextCancellation) spawn a goroutine, and they read rec only
//     after receiving from the done channel.

func TestStreamSSE_StreamsString(t *testing.T) {
	t.Parallel()

	ch := make(chan string, 3)
	ch <- NewSSEMessage("ping", "one").String()
	ch <- NewSSEMessage("ping", "two").String()
	ch <- NewSSEMessage("ping", "three").String()
	close(ch)

	rec := newFlushRecorder()
	err := StreamSSE(t.Context(), rec, ch, func(s string) string { return s })
	require.NoError(t, err)

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
	close(ch)

	rec := newFlushRecorder()
	err := StreamSSE(t.Context(), rec, ch, func(tm TopicMessage) string {
		return NewSSEMessage(tm.Topic, tm.Data).String()
	})
	require.NoError(t, err)

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
	close(ch)

	rec := newFlushRecorder()
	err := StreamSSE(t.Context(), rec, ch, func(s string) string { return s })
	require.NoError(t, err)
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

	// Pre-closed channel: StreamSSE sets headers, enters the select loop,
	// immediately receives the !ok from the closed channel, and returns.
	// This gives us synchronous access to rec.Header() with no race.
	ch := make(chan string)
	close(ch)

	rec := newFlushRecorder()
	err := StreamSSE(t.Context(), rec, ch, func(s string) string { return s })
	require.NoError(t, err)

	assert.Equal(t, "text/event-stream", rec.Header().Get("Content-Type"))
	assert.Equal(t, "no-cache", rec.Header().Get("Cache-Control"))
	assert.Equal(t, "keep-alive", rec.Header().Get("Connection"))
}

func TestStreamSSE_Snapshot(t *testing.T) {
	t.Parallel()

	ch := make(chan string, 1)
	ch <- NewSSEMessage("update", "live").String()
	close(ch)

	snapshot := NewSSEMessage("snapshot", "initial").String()

	rec := newFlushRecorder()
	err := StreamSSE(t.Context(), rec, ch, func(s string) string { return s },
		WithStreamSnapshot(func() string { return snapshot }),
	)
	require.NoError(t, err)

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
	close(ch)

	var snapshotCalls int
	rec := newFlushRecorder()
	err := StreamSSE(t.Context(), rec, ch, func(s string) string { return s },
		WithStreamSnapshot(func() string {
			snapshotCalls++
			return ""
		}),
	)
	require.NoError(t, err)

	assert.Equal(t, 1, snapshotCalls)
	// Body should contain exactly one event — the live one.
	assert.Equal(t, 1, strings.Count(rec.Body.String(), "event: event"))
}

func TestStreamSSE_Heartbeat(t *testing.T) {
	t.Parallel()

	ch := make(chan string)
	rec := newFlushRecorder()
	ctx, cancel := context.WithCancel(t.Context())

	done := make(chan error, 1)
	go func() {
		done <- StreamSSE(ctx, rec, ch, func(s string) string { return s },
			WithStreamHeartbeat(10*time.Millisecond),
		)
	}()

	// Let the heartbeat fire a few times before cancelling. We do NOT read
	// rec from the main goroutine while the streaming goroutine is running —
	// that would race on rec.Body. The sleep is bounded and deterministic:
	// 60ms / 10ms = 6 expected ticks, well above the asserted minimum of 2.
	time.Sleep(60 * time.Millisecond)
	cancel()
	require.NoError(t, <-done)

	// Safe to read rec.Body after <-done; the goroutine has exited and the
	// receive on done establishes happens-before with the writes inside it.
	assert.GreaterOrEqual(t, strings.Count(rec.Body.String(), ": keepalive\n\n"), 2)
}

func TestStreamSSE_EncoderEmptySkips(t *testing.T) {
	t.Parallel()

	ch := make(chan int, 4)
	ch <- 1
	ch <- 2 // skipped
	ch <- 3
	ch <- 4 // skipped
	close(ch)

	encode := func(n int) string {
		if n%2 == 0 {
			return ""
		}
		return NewSSEMessage("odd", string(rune('0'+n))).String()
	}

	rec := newFlushRecorder()
	err := StreamSSE(t.Context(), rec, ch, encode)
	require.NoError(t, err)

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
	close(ch)

	var calls int
	custom := func(w http.ResponseWriter, msg string) error {
		calls++
		if _, err := w.Write([]byte("custom:" + msg + "|")); err != nil {
			return err
		}
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		return nil
	}

	rec := newFlushRecorder()
	err := StreamSSE(t.Context(), rec, ch, func(s string) string { return s },
		WithStreamWriter(custom),
	)
	require.NoError(t, err)

	assert.Equal(t, 2, calls)
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
