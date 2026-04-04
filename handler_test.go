package tavern

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// flushRecorder wraps httptest.ResponseRecorder to implement http.Flusher.
type flushRecorder struct {
	*httptest.ResponseRecorder
	flushed int
}

func (f *flushRecorder) Flush() {
	f.flushed++
	f.ResponseRecorder.Flush()
}

func newFlushRecorder() *flushRecorder {
	return &flushRecorder{ResponseRecorder: httptest.NewRecorder()}
}

func TestSSEHandler_BasicStreaming(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	handler := b.SSEHandler("events")

	// Publish a message before starting the handler
	// (use PublishWithReplay so the subscriber gets it)
	b.PublishWithReplay("events", "hello")

	rec := newFlushRecorder()
	req := httptest.NewRequest("GET", "/sse", http.NoBody)
	ctx, cancel := contextWithCancel(req)
	req = req.WithContext(ctx)

	done := make(chan struct{})
	go func() {
		handler.ServeHTTP(rec, req)
		close(done)
	}()

	// Wait for the replayed message to be written
	time.Sleep(50 * time.Millisecond)
	cancel()
	<-done

	assert.Contains(t, rec.Body.String(), "hello")
	assert.Equal(t, "text/event-stream", rec.Header().Get("Content-Type"))
	assert.Equal(t, "no-cache", rec.Header().Get("Cache-Control"))
	assert.Equal(t, "keep-alive", rec.Header().Get("Connection"))
}

func TestSSEHandler_LastEventID(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	b.SetReplayPolicy("events", 10)
	b.PublishWithID("events", "1", "msg-1")
	b.PublishWithID("events", "2", "msg-2")
	b.PublishWithID("events", "3", "msg-3")

	handler := b.SSEHandler("events")

	rec := newFlushRecorder()
	req := httptest.NewRequest("GET", "/sse", http.NoBody)
	req.Header.Set("Last-Event-ID", "1") // should replay 2 and 3
	ctx, cancel := contextWithCancel(req)
	req = req.WithContext(ctx)

	done := make(chan struct{})
	go func() {
		handler.ServeHTTP(rec, req)
		close(done)
	}()

	time.Sleep(50 * time.Millisecond)
	cancel()
	<-done

	body := rec.Body.String()
	assert.Contains(t, body, "msg-2")
	assert.Contains(t, body, "msg-3")
	assert.NotContains(t, body, "msg-1")
}

func TestSSEHandler_CustomWriter(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	var written []string
	handler := b.SSEHandler("events", WithSSEWriter(func(w http.ResponseWriter, msg string) error {
		written = append(written, msg)
		return nil
	}))

	b.PublishWithReplay("events", "custom-msg")

	rec := newFlushRecorder()
	req := httptest.NewRequest("GET", "/sse", http.NoBody)
	ctx, cancel := contextWithCancel(req)
	req = req.WithContext(ctx)

	done := make(chan struct{})
	go func() {
		handler.ServeHTTP(rec, req)
		close(done)
	}()

	time.Sleep(50 * time.Millisecond)
	cancel()
	<-done

	require.Len(t, written, 1)
	assert.Equal(t, "custom-msg", written[0])
}

func TestSSEHandler_BrokerClose(t *testing.T) {
	b := NewSSEBroker()

	handler := b.SSEHandler("events")

	rec := newFlushRecorder()
	req := httptest.NewRequest("GET", "/sse", http.NoBody)

	done := make(chan struct{})
	go func() {
		handler.ServeHTTP(rec, req)
		close(done)
	}()

	time.Sleep(20 * time.Millisecond)
	b.Close()

	select {
	case <-done:
		// handler returned after broker close
	case <-time.After(time.Second):
		t.Fatal("handler did not return after broker close")
	}
}

func TestSSEHandler_Flushes(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	handler := b.SSEHandler("events")

	b.PublishWithReplay("events", "flush-test")

	rec := newFlushRecorder()
	req := httptest.NewRequest("GET", "/sse", http.NoBody)
	ctx, cancel := contextWithCancel(req)
	req = req.WithContext(ctx)

	done := make(chan struct{})
	go func() {
		handler.ServeHTTP(rec, req)
		close(done)
	}()

	time.Sleep(50 * time.Millisecond)
	cancel()
	<-done

	assert.Greater(t, rec.flushed, 0, "expected at least one flush")
}

// contextWithCancel creates a cancellable context from a request.
func contextWithCancel(r *http.Request) (context.Context, context.CancelFunc) {
	return context.WithCancel(r.Context())
}
