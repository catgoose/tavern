package tavern

import (
	"bufio"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newH2Server starts a TLS test server with HTTP/2 enabled.
func newH2Server(t *testing.T, h http.Handler) *httptest.Server {
	t.Helper()
	srv := httptest.NewUnstartedServer(h)
	srv.EnableHTTP2 = true
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv
}

// openH2Stream starts a request and returns the response once headers
// arrive. Headers go out with the first frame, so publish runs after the
// handler has subscribed.
func openH2Stream(t *testing.T, ctx context.Context, srv *httptest.Server, b *SSEBroker, topic string, publish func()) *http.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, "GET", srv.URL, http.NoBody)
	require.NoError(t, err)
	type result struct {
		resp *http.Response
		err  error
	}
	got := make(chan result, 1)
	go func() {
		resp, err := srv.Client().Do(req)
		got <- result{resp, err}
	}()
	require.Eventually(t, func() bool { return b.HasSubscribers(topic) }, 2*time.Second, time.Millisecond)
	publish()
	select {
	case res := <-got:
		require.NoError(t, res.err)
		require.Equal(t, 2, res.resp.ProtoMajor, "the test must run over HTTP/2")
		t.Cleanup(func() { _ = res.resp.Body.Close() })
		return res.resp
	case <-time.After(5 * time.Second):
		t.Fatal("no response headers")
		return nil
	}
}

func TestHTTP2WriteTimeoutIdle(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()
	var errs writeErrors
	srv := newH2Server(t, b.SSEHandler("t", WithWriteTimeout(20*time.Millisecond), WithWriteErrorHandler(errs.record)))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	resp := openH2Stream(t, ctx, srv, b, "t", func() { b.Publish("t", "data: one\n\n") })
	reader := bufio.NewReader(resp.Body)
	line, err := reader.ReadString('\n')
	require.NoError(t, err)
	assert.Equal(t, "data: one\n", line)

	idle := time.NewTimer(100 * time.Millisecond)
	<-idle.C
	b.Publish("t", "data: two\n\n")
	for line == "data: one\n" || line == "\n" {
		line, err = reader.ReadString('\n')
		require.NoError(t, err)
	}
	assert.Equal(t, "data: two\n", line, "idle time between frames is not limited over HTTP/2")
	errs.mu.Lock()
	assert.Empty(t, errs.errs)
	errs.mu.Unlock()
}

// TestHTTP2SlowClientTimesOut streams to an HTTP/2 client that stops
// reading. Once flow control blocks the stream, the write timeout ends it.
func TestHTTP2SlowClientTimesOut(t *testing.T) {
	b := NewSSEBroker(WithBufferSize(4))
	defer b.Close()
	errCh := make(chan *WriteError, 1)
	srv := newH2Server(t, b.SSEHandler("t", WithWriteTimeout(100*time.Millisecond),
		WithWriteErrorHandler(func(e *WriteError) { errCh <- e })))
	big := "data: " + strings.Repeat("x", 1<<20) + "\n\n"
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	openH2Stream(t, ctx, srv, b, "t", func() { b.Publish("t", big) })

	deadline := time.After(10 * time.Second)
	for {
		select {
		case e := <-errCh:
			assert.Equal(t, WriteOpWrite, e.Op)
			assert.ErrorIs(t, e, os.ErrDeadlineExceeded)
			require.Eventually(t, func() bool { return !b.HasSubscribers("t") }, 2*time.Second, time.Millisecond)
			return
		case <-deadline:
			t.Fatal("write timeout never fired for an HTTP/2 client that does not read")
		default:
			b.Publish("t", big)
		}
	}
}

// TestHTTP2CancelEndsQuietly cancels the client request while the server is
// blocked writing to it. The handler returns without reporting an error.
func TestHTTP2CancelEndsQuietly(t *testing.T) {
	b := NewSSEBroker(WithBufferSize(4))
	defer b.Close()
	var errs writeErrors
	done := make(chan struct{})
	h := b.SSEHandler("t", WithWriteTimeout(time.Hour), WithWriteErrorHandler(errs.record))
	srv := newH2Server(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(done)
		h.ServeHTTP(w, r)
	}))
	big := "data: " + strings.Repeat("x", 1<<20) + "\n\n"
	ctx, cancel := context.WithCancel(context.Background())
	openH2Stream(t, ctx, srv, b, "t", func() { b.Publish("t", big) })
	for range 16 { // more than the client's stream window, so a write blocks
		b.Publish("t", big)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("handler did not return after the client cancelled")
	}
	errs.mu.Lock()
	defer errs.mu.Unlock()
	assert.Empty(t, errs.errs, "client cancellation is not a write failure")
}
