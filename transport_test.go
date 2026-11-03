package tavern

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// deadlineWriter is a ResponseWriter that supports write deadlines the way
// a net/http connection does. With blocked set, writes wait until the
// deadline passes, so tests can stall a write and observe its release.
type deadlineWriter struct {
	header http.Header

	mu        sync.Mutex
	body      strings.Builder
	deadline  time.Time
	changed   chan struct{} // closed and replaced whenever deadline changes
	deadlines []time.Time
	flushes   int
	blocked   bool
	flushErr  error
	// deadlineFailFrom, when positive, makes that SetWriteDeadline call and
	// every later one fail.
	deadlineFailFrom int
	calls            int

	writing chan struct{} // receives once per write that blocks
}

func newDeadlineWriter() *deadlineWriter {
	return &deadlineWriter{
		header:  make(http.Header),
		changed: make(chan struct{}),
		writing: make(chan struct{}, 16),
	}
}

func (d *deadlineWriter) Header() http.Header { return d.header }
func (d *deadlineWriter) WriteHeader(int)     {}

func (d *deadlineWriter) SetWriteDeadline(t time.Time) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.calls++
	if d.deadlineFailFrom > 0 && d.calls >= d.deadlineFailFrom {
		return errStoreDown
	}
	d.deadline = t
	d.deadlines = append(d.deadlines, t)
	close(d.changed)
	d.changed = make(chan struct{})
	return nil
}

func (d *deadlineWriter) FlushError() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.flushes++
	return d.flushErr
}

func (d *deadlineWriter) Write(p []byte) (int, error) {
	d.mu.Lock()
	if !d.blocked {
		defer d.mu.Unlock()
		return d.body.Write(p)
	}
	d.mu.Unlock()
	d.writing <- struct{}{}
	for {
		d.mu.Lock()
		deadline, changed := d.deadline, d.changed
		d.mu.Unlock()
		if d.waitDeadline(deadline, changed) {
			return 0, os.ErrDeadlineExceeded
		}
	}
}

// waitDeadline waits until deadline passes (true) or it changes (false). A
// zero deadline waits only for a change.
func (d *deadlineWriter) waitDeadline(deadline time.Time, changed <-chan struct{}) bool {
	var expired <-chan time.Time
	if !deadline.IsZero() {
		wait := time.Until(deadline)
		if wait <= 0 {
			return true
		}
		timer := time.NewTimer(wait)
		defer timer.Stop()
		expired = timer.C
	}
	select {
	case <-expired:
		return true
	case <-changed:
		return false
	}
}

func (d *deadlineWriter) String() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.body.String()
}

// lastDeadline returns the most recent deadline set.
func (d *deadlineWriter) lastDeadline() time.Time {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.deadlines[len(d.deadlines)-1]
}

func (d *deadlineWriter) deadlineCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.deadlines)
}

// unwrapWriter hides every optional interface of the writer it wraps except
// through Unwrap, like typical middleware.
type unwrapWriter struct {
	inner http.ResponseWriter
}

func (u *unwrapWriter) Header() http.Header         { return u.inner.Header() }
func (u *unwrapWriter) Write(p []byte) (int, error) { return u.inner.Write(p) }
func (u *unwrapWriter) WriteHeader(code int)        { u.inner.WriteHeader(code) }
func (u *unwrapWriter) Unwrap() http.ResponseWriter { return u.inner }

// writeErrors collects reported write errors.
type writeErrors struct {
	mu   sync.Mutex
	errs []*WriteError
}

func (w *writeErrors) record(e *WriteError) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.errs = append(w.errs, e)
}

func (w *writeErrors) only(t *testing.T) *WriteError {
	t.Helper()
	w.mu.Lock()
	defer w.mu.Unlock()
	require.Len(t, w.errs, 1)
	return w.errs[0]
}

func TestStreamSSEWriteErrorsDefaultNil(t *testing.T) {
	failing := func(http.ResponseWriter, string) error { return errStoreDown }
	ch := make(chan string, 1)
	ch <- "x"
	err := StreamSSE(context.Background(), newFlushRecorder(), ch, func(s string) string { return s },
		WithStreamWriter(failing))
	assert.NoError(t, err, "legacy default returns nil after a write failure")

	ch <- "x"
	err = StreamSSE(context.Background(), newFlushRecorder(), ch, func(s string) string { return s },
		WithStreamWriter(failing), WithStreamWriteErrors())
	var we *WriteError
	require.ErrorAs(t, err, &we)
	assert.Equal(t, WriteOpWrite, we.Op)
	assert.ErrorIs(t, err, errStoreDown)
}

func TestStreamSSEWriteTimeout(t *testing.T) {
	w := newDeadlineWriter()
	ch := make(chan string, 2)
	ch <- "first"
	done := make(chan error, 1)
	go func() {
		done <- StreamSSE(context.Background(), w, ch, func(s string) string { return s },
			WithStreamSnapshot(func() string { return "snap\n\n" }),
			WithStreamWriteTimeout(20*time.Millisecond), WithStreamWriteErrors())
	}()
	require.Eventually(t, func() bool { return strings.Contains(w.String(), "first") }, time.Second, time.Millisecond)
	assert.True(t, w.lastDeadline().IsZero(), "deadline is cleared between frames")

	w.mu.Lock()
	w.blocked = true
	w.mu.Unlock()
	ch <- "stalls"
	err := <-done
	var we *WriteError
	require.ErrorAs(t, err, &we)
	assert.Equal(t, WriteOpWrite, we.Op)
	assert.ErrorIs(t, err, os.ErrDeadlineExceeded)
	assert.Equal(t, "snap\n\nfirst", w.String())
}

func TestStreamSSEHeartbeatUnderDeadline(t *testing.T) {
	w := newDeadlineWriter()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- StreamSSE(ctx, w, make(chan string), func(s string) string { return s },
			WithStreamHeartbeat(time.Millisecond), WithStreamWriteTimeout(time.Second))
	}()
	require.Eventually(t, func() bool { return strings.Contains(w.String(), ": keepalive") }, time.Second, time.Millisecond)
	cancel()
	require.NoError(t, <-done)
	// Probe, then a set/clear pair per heartbeat, plus the cancel release.
	assert.GreaterOrEqual(t, w.deadlineCount(), 3)
}

func TestStreamSSEDeadlineUnsupported(t *testing.T) {
	rec := newFlushRecorder()
	err := StreamSSE(context.Background(), rec, make(chan string), func(s string) string { return s },
		WithStreamWriteTimeout(time.Second))
	var we *WriteError
	require.ErrorAs(t, err, &we)
	assert.Equal(t, WriteOpDeadline, we.Op)
	assert.ErrorIs(t, err, http.ErrNotSupported)
	assert.Empty(t, rec.Header().Get("Content-Type"), "no SSE response is started")
	assert.Empty(t, rec.Body.String())
}

func TestStreamSSEThroughUnwrap(t *testing.T) {
	inner := newDeadlineWriter()
	ch := make(chan string, 1)
	ch <- "hello"
	close(ch)
	err := StreamSSE(context.Background(), &unwrapWriter{inner: inner}, ch, func(s string) string { return s },
		WithStreamWriteTimeout(time.Second))
	require.NoError(t, err)
	assert.Equal(t, "hello", inner.String())
	assert.GreaterOrEqual(t, inner.deadlineCount(), 3, "deadlines reach the wrapped writer")
	assert.Equal(t, 1, inner.flushes, "flushes reach the wrapped writer")
}

func TestStreamSSEStatusThroughTransport(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()
	status := NewSubscriptionStatus()
	ch, unsub := b.SubscribeWith("t", SubWithStatus(status))
	w := newDeadlineWriter()
	w.flushErr = errStoreDown
	b.Publish("t", "msg")
	err := StreamSSE(context.Background(), w, ch, func(s string) string { return s }, WithStreamWriteErrors())
	unsub()
	unsub()
	var we *WriteError
	require.ErrorAs(t, err, &we)
	assert.Equal(t, WriteOpFlush, we.Op)
	assert.Equal(t, Termination{Reason: TerminationUnsubscribe, Topic: "t"}, waitTermination(t, status))
}

func serveSSE(h http.Handler, w http.ResponseWriter, r *http.Request) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.ServeHTTP(w, r)
	}()
	return done
}

func TestSSEHandlerDeadlineUnsupported(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()
	var errs writeErrors
	h := b.SSEHandler("t", WithWriteTimeout(time.Second), WithWriteErrorHandler(errs.record))
	rec := newFlushRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/sse", http.NoBody))
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
	assert.NotEqual(t, "text/event-stream", rec.Header().Get("Content-Type"))
	assert.False(t, b.HasSubscribers("t"), "no subscription without the requested protection")
	we := errs.only(t)
	assert.Equal(t, WriteOpDeadline, we.Op)
	assert.ErrorIs(t, we, http.ErrNotSupported)
}

func TestSSEHandlerFlushFailure(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()
	var errs writeErrors
	h := b.SSEHandler("t", WithWriteErrorHandler(errs.record))
	w := newDeadlineWriter()
	w.flushErr = errStoreDown
	b.PublishWithReplay("t", "replayed")
	<-serveSSE(h, w, httptest.NewRequest("GET", "/sse", http.NoBody))
	we := errs.only(t)
	assert.Equal(t, WriteOpFlush, we.Op)
	assert.ErrorIs(t, we, errStoreDown)
	assert.False(t, b.HasSubscribers("t"))
}

// TestSSEHandlerCancelReleasesBlockedWrite stalls a write while the
// subscriber queue fills, then cancels the request. The deadline-capable
// writer is released and the handler unsubscribes.
func TestSSEHandlerCancelReleasesBlockedWrite(t *testing.T) {
	b := NewSSEBroker(WithBufferSize(2))
	defer b.Close()
	var errs writeErrors
	h := b.SSEHandler("t", WithWriteTimeout(time.Hour), WithWriteErrorHandler(errs.record))
	w := newDeadlineWriter()
	w.blocked = true
	ctx, cancel := context.WithCancel(context.Background())
	done := serveSSE(h, w, httptest.NewRequest("GET", "/sse", http.NoBody).WithContext(ctx))

	require.Eventually(t, func() bool { return b.HasSubscribers("t") }, time.Second, time.Millisecond)
	b.Publish("t", "stalls")
	<-w.writing
	for i := 0; i < 5; i++ {
		b.Publish("t", "queued") // the queue fills behind the blocked write
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("cancellation did not release the blocked write")
	}
	errs.mu.Lock()
	assert.Empty(t, errs.errs, "a write cut short by cancellation is not reported")
	errs.mu.Unlock()
	assert.False(t, b.HasSubscribers("t"))
}

func TestSSEHandlerCustomWriterUnderDeadline(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()
	w := newDeadlineWriter()
	var sawDeadline bool
	custom := func(rw http.ResponseWriter, msg string) error {
		sawDeadline = !w.lastDeadline().IsZero()
		_, err := fmt.Fprintf(rw, "custom:%s", msg)
		return err
	}
	h := b.SSEHandler("t", WithSSEWriter(custom), WithWriteTimeout(time.Second),
		WithMaxConnectionDuration(time.Nanosecond))
	b.PublishWithReplay("t", "m")
	<-serveSSE(h, w, httptest.NewRequest("GET", "/sse", http.NoBody))
	body := w.String()
	if strings.Contains(body, "custom:m") {
		assert.True(t, sawDeadline, "custom writer runs under the deadline")
	}
	// The closing retry frame bypasses the custom writer, as before.
	assert.Contains(t, body, "retry: 1000\n\n: max connection duration reached\n\n")
	assert.NotContains(t, body, "custom:retry")
	assert.True(t, w.lastDeadline().IsZero())
	assert.Equal(t, 1, w.flushes, "only the closing frame is flushed by tavern")
}

func TestMaxConnectionDurationTiny(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()
	for _, d := range []time.Duration{time.Nanosecond, 9 * time.Nanosecond} {
		rec := newFlushRecorder()
		h := b.SSEHandler("t", WithMaxConnectionDuration(d))
		<-serveSSE(h, rec, httptest.NewRequest("GET", "/sse", http.NoBody))
		assert.Contains(t, rec.Body.String(), "max connection duration reached")

		rec = newFlushRecorder()
		b.DefineGroup("g", []string{"t"})
		<-serveSSE(b.GroupHandler("g", WithMaxConnectionDuration(d)), rec, httptest.NewRequest("GET", "/sse", http.NoBody))
		assert.Contains(t, rec.Body.String(), "max connection duration reached")
	}
}

func TestGroupHandlerReplayWriteFailure(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()
	b.SetReplayPolicy("a", 5)
	b.PublishWithID("a", "1", "one")
	b.PublishWithID("a", "2", "two")
	b.DefineGroup("g", []string{"a", "b"})
	var errs writeErrors
	failing := func(http.ResponseWriter, string) error { return errStoreDown }
	h := b.GroupHandler("g", WithSSEWriter(failing), WithWriteErrorHandler(errs.record))
	req := httptest.NewRequest("GET", "/sse", http.NoBody)
	req.Header.Set("Last-Event-ID", "1")
	<-serveSSE(h, newFlushRecorder(), req)
	we := errs.only(t)
	assert.Equal(t, WriteOpWrite, we.Op)
	assert.ErrorIs(t, we, errStoreDown)
	assert.False(t, b.HasSubscribers("a"))
	assert.False(t, b.HasSubscribers("b"))
}

func TestDynamicGroupHandlerDeadlineUnsupported(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()
	b.DynamicGroup("d", func(*http.Request) []string { return []string{"a"} })
	rec := newFlushRecorder()
	b.DynamicGroupHandler("d", WithWriteTimeout(time.Second)).ServeHTTP(rec, httptest.NewRequest("GET", "/sse", http.NoBody))
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
	assert.False(t, b.HasSubscribers("a"))
}

// TestSSEHandlerRealServerSlowClient streams to a TCP client that never
// reads. Once socket buffers fill, the write timeout ends the connection.
func TestSSEHandlerRealServerSlowClient(t *testing.T) {
	b := NewSSEBroker(WithBufferSize(4))
	defer b.Close()
	errCh := make(chan *WriteError, 1)
	srv := httptest.NewServer(b.SSEHandler("t", WithWriteTimeout(100*time.Millisecond),
		WithWriteErrorHandler(func(e *WriteError) { errCh <- e })))
	defer srv.Close()

	conn, err := net.Dial("tcp", srv.Listener.Addr().String())
	require.NoError(t, err)
	defer conn.Close()
	_, err = fmt.Fprintf(conn, "GET / HTTP/1.1\r\nHost: x\r\n\r\n")
	require.NoError(t, err)
	require.Eventually(t, func() bool { return b.HasSubscribers("t") }, 2*time.Second, time.Millisecond)

	big := "data: " + strings.Repeat("x", 1<<20) + "\n\n"
	deadline := time.After(10 * time.Second)
	for {
		select {
		case e := <-errCh:
			assert.ErrorIs(t, e, os.ErrDeadlineExceeded)
			require.Eventually(t, func() bool { return !b.HasSubscribers("t") }, 2*time.Second, time.Millisecond)
			return
		case <-deadline:
			t.Fatal("write timeout never fired for a client that does not read")
		default:
			b.Publish("t", big)
		}
	}
}

// TestSSEHandlerRealServerIdle checks that a write timeout does not limit
// idle time between frames on a real connection.
func TestSSEHandlerRealServerIdle(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()
	srv := httptest.NewServer(b.SSEHandler("t", WithWriteTimeout(20*time.Millisecond)))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", srv.URL, http.NoBody)
	require.NoError(t, err)
	// Response headers go out with the first frame, so publish it once the
	// handler has subscribed and only then wait for the response.
	type result struct {
		resp *http.Response
		err  error
	}
	got := make(chan result, 1)
	go func() {
		resp, err := srv.Client().Do(req)
		got <- result{resp, err}
	}()
	require.Eventually(t, func() bool { return b.HasSubscribers("t") }, time.Second, time.Millisecond)
	b.Publish("t", "data: one\n\n")
	res := <-got
	require.NoError(t, res.err)
	resp := res.resp
	defer resp.Body.Close()
	assert.Equal(t, "text/event-stream", resp.Header.Get("Content-Type"))
	reader := bufio.NewReader(resp.Body)
	line, err := reader.ReadString('\n')
	require.NoError(t, err)
	assert.Equal(t, "data: one\n", line)

	// Stay idle for several timeout periods, then publish again.
	idle := time.NewTimer(100 * time.Millisecond)
	<-idle.C
	b.Publish("t", "data: two\n\n")
	for line == "data: one\n" || line == "\n" {
		line, err = reader.ReadString('\n')
		require.NoError(t, err)
	}
	assert.Equal(t, "data: two\n", line)
	assert.False(t, errors.Is(err, os.ErrDeadlineExceeded))
}

// waitExpired blocks until w's deadline has been set to a time that passed.
func (d *deadlineWriter) waitExpired() {
	for {
		d.mu.Lock()
		deadline, changed := d.deadline, d.changed
		d.mu.Unlock()
		if !deadline.IsZero() && !deadline.After(time.Now()) {
			return
		}
		<-changed
	}
}

// finishWithin waits for a stream to return. On failure it expires the
// writer's deadline so the stalled write, and the test, can end.
func finishWithin(t *testing.T, w *deadlineWriter, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(2 * time.Second):
		_ = w.SetWriteDeadline(time.Now())
		<-done
		t.Fatal("cancelled stream started or rearmed a blocked write")
		return nil
	}
}

func identity(s string) string { return s }

// TestStreamSSECancellationIsNotRearmed cancels the request at each point a
// frame write can be reached. The stream must end without starting a new
// blocked write, setting a fresh deadline, or reporting an error.
func TestStreamSSECancellationIsNotRearmed(t *testing.T) {
	cases := []struct {
		name string
		run  func(ctx context.Context, cancel func(), w *deadlineWriter) error
		body string
	}{
		{
			name: "during snapshot",
			run: func(ctx context.Context, cancel func(), w *deadlineWriter) error {
				return StreamSSE(ctx, w, make(chan string), identity, WithStreamWriteTimeout(time.Hour),
					WithStreamWriteErrors(), WithStreamSnapshot(func() string {
						cancel()
						w.waitExpired()
						return "snap\n\n"
					}))
			},
		},
		{
			name: "during encode",
			run: func(ctx context.Context, cancel func(), w *deadlineWriter) error {
				ch := make(chan string, 1)
				ch <- "m"
				return StreamSSE(ctx, w, ch, func(s string) string {
					cancel()
					w.waitExpired()
					return s
				}, WithStreamWriteTimeout(time.Hour), WithStreamWriteErrors())
			},
		},
		{
			name: "between deadline and write",
			run: func(ctx context.Context, cancel func(), w *deadlineWriter) error {
				ch := make(chan string, 1)
				ch <- "m"
				custom := func(rw http.ResponseWriter, msg string) error {
					cancel()
					w.waitExpired()
					_, err := io.WriteString(rw, msg)
					return err
				}
				return StreamSSE(ctx, w, ch, identity, WithStreamWriter(custom),
					WithStreamWriteTimeout(time.Hour), WithStreamWriteErrors())
			},
		},
		{
			name: "after write before clear, with a buffered next frame",
			run: func(ctx context.Context, cancel func(), w *deadlineWriter) error {
				ch := make(chan string, 2)
				ch <- "first"
				ch <- "second"
				custom := func(rw http.ResponseWriter, msg string) error {
					w.mu.Lock()
					w.blocked = false
					w.mu.Unlock()
					if _, err := io.WriteString(rw, msg); err != nil {
						return err
					}
					w.mu.Lock()
					w.blocked = true
					w.mu.Unlock()
					cancel()
					w.waitExpired()
					return nil
				}
				return StreamSSE(ctx, w, ch, identity, WithStreamWriter(custom),
					WithStreamWriteTimeout(time.Hour), WithStreamWriteErrors())
			},
			body: "first",
		},
		{
			name: "already cancelled",
			run: func(ctx context.Context, cancel func(), w *deadlineWriter) error {
				cancel()
				return StreamSSE(ctx, w, make(chan string), identity, WithStreamWriteTimeout(time.Hour),
					WithStreamWriteErrors(), WithStreamSnapshot(func() string { return "snap\n\n" }))
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := newDeadlineWriter()
			w.blocked = true
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- tc.run(ctx, cancel, w) }()
			require.NoError(t, finishWithin(t, w, done), "cancellation ends the stream quietly")
			assert.Equal(t, tc.body, w.String())
			if n := w.deadlineCount(); n > 0 {
				last := w.lastDeadline()
				assert.False(t, last.IsZero() || last.After(time.Now()),
					"the cancellation deadline is never replaced or cleared")
			}
		})
	}
}

func TestStreamSSECancelDeadlineFailureReported(t *testing.T) {
	w := newDeadlineWriter()
	w.deadlineFailFrom = 2 // the probe succeeds; expiring on cancel fails
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- StreamSSE(ctx, w, make(chan string), identity,
			WithStreamWriteTimeout(time.Hour), WithStreamWriteErrors())
	}()
	cancel()
	err := <-done
	var we *WriteError
	require.ErrorAs(t, err, &we)
	assert.Equal(t, WriteOpDeadline, we.Op)
	assert.ErrorIs(t, err, errStoreDown)
}

func TestHandlersSkipWritesAfterCancel(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()
	b.SetReplayPolicy("a", 5)
	b.PublishWithID("a", "1", "one")
	b.PublishWithID("a", "2", "two")
	b.DefineGroup("g", []string{"a"})
	handlers := map[string]func(opts ...SSEHandlerOption) http.Handler{
		"SSEHandler":   func(opts ...SSEHandlerOption) http.Handler { return b.SSEHandler("a", opts...) },
		"GroupHandler": func(opts ...SSEHandlerOption) http.Handler { return b.GroupHandler("g", opts...) },
	}
	for name, newHandler := range handlers {
		t.Run(name, func(t *testing.T) {
			var errs writeErrors
			h := newHandler(WithWriteTimeout(time.Hour), WithWriteErrorHandler(errs.record))
			w := newDeadlineWriter()
			w.blocked = true
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			req := httptest.NewRequest("GET", "/sse", http.NoBody).WithContext(ctx)
			req.Header.Set("Last-Event-ID", "1") // replay is pending
			done := make(chan error, 1)
			go func() {
				h.ServeHTTP(w, req)
				done <- nil
			}()
			_ = finishWithin(t, w, done)
			assert.Empty(t, w.String())
			errs.mu.Lock()
			assert.Empty(t, errs.errs)
			errs.mu.Unlock()
			assert.False(t, b.HasSubscribers("a"))
		})
	}
}
