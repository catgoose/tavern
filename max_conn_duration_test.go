package tavern

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMaxConnectionDuration_Basic(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	handler := b.SSEHandler("events", WithMaxConnectionDuration(100*time.Millisecond))

	rec := newFlushRecorder()
	req := httptest.NewRequest("GET", "/sse", http.NoBody)

	start := time.Now()
	done := make(chan struct{})
	go func() {
		handler.ServeHTTP(rec, req)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("handler did not return after max connection duration")
	}

	elapsed := time.Since(start)
	// Should close between 100ms and 110ms + some tolerance for scheduling.
	assert.GreaterOrEqual(t, elapsed, 100*time.Millisecond, "closed too early")
	assert.Less(t, elapsed, 200*time.Millisecond, "closed too late (jitter should be at most 10%)")
}

func TestMaxConnectionDuration_Jitter(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	const n = 10
	durations := make([]time.Duration, n)
	var wg sync.WaitGroup
	wg.Add(n)

	for i := range n {
		go func(idx int) {
			defer wg.Done()
			handler := b.SSEHandler("events", WithMaxConnectionDuration(100*time.Millisecond))
			rec := newFlushRecorder()
			req := httptest.NewRequest("GET", "/sse", http.NoBody)
			start := time.Now()
			handler.ServeHTTP(rec, req)
			durations[idx] = time.Since(start)
		}(i)
	}

	wg.Wait()

	// Check that not all durations are identical — at least two differ by > 1ms.
	allSame := true
	for i := 1; i < n; i++ {
		diff := durations[i] - durations[0]
		if diff < 0 {
			diff = -diff
		}
		if diff > time.Millisecond {
			allSame = false
			break
		}
	}
	assert.False(t, allSame, "expected jitter to produce varying durations, got: %v", durations)
}

func TestMaxConnectionDuration_RetryDirective(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	handler := b.SSEHandler("events", WithMaxConnectionDuration(50*time.Millisecond))

	rec := newFlushRecorder()
	req := httptest.NewRequest("GET", "/sse", http.NoBody)

	done := make(chan struct{})
	go func() {
		handler.ServeHTTP(rec, req)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("handler did not return")
	}

	body := rec.Body.String()
	assert.Contains(t, body, "retry: 1000\n\n", "should contain retry directive")
	assert.Contains(t, body, ": max connection duration reached\n\n", "should contain close comment")

	// Retry should come before the comment.
	retryIdx := strings.Index(body, "retry: 1000")
	commentIdx := strings.Index(body, ": max connection duration reached")
	assert.Less(t, retryIdx, commentIdx, "retry directive should precede close comment")
}

func TestMaxConnectionDuration_LastEventIDReplay(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	b.SetReplayPolicy("events", 10)
	b.PublishWithID("events", "1", "msg-1")
	b.PublishWithID("events", "2", "msg-2")
	b.PublishWithID("events", "3", "msg-3")

	// First connection: short max duration, no Last-Event-ID.
	handler := b.SSEHandler("events", WithMaxConnectionDuration(50*time.Millisecond))

	rec1 := newFlushRecorder()
	req1 := httptest.NewRequest("GET", "/sse", http.NoBody)

	done1 := make(chan struct{})
	go func() {
		handler.ServeHTTP(rec1, req1)
		close(done1)
	}()

	select {
	case <-done1:
	case <-time.After(2 * time.Second):
		t.Fatal("first connection did not close")
	}

	// Second connection: reconnect with Last-Event-ID "1", should replay 2 and 3.
	rec2 := newFlushRecorder()
	req2 := httptest.NewRequest("GET", "/sse", http.NoBody)
	req2.Header.Set("Last-Event-ID", "1")
	ctx2, cancel2 := contextWithCancel(req2)
	req2 = req2.WithContext(ctx2)

	handler2 := b.SSEHandler("events", WithMaxConnectionDuration(200*time.Millisecond))
	done2 := make(chan struct{})
	go func() {
		handler2.ServeHTTP(rec2, req2)
		close(done2)
	}()

	// Give time for replay.
	time.Sleep(50 * time.Millisecond)
	cancel2()
	<-done2

	body := rec2.Body.String()
	assert.Contains(t, body, "msg-2")
	assert.Contains(t, body, "msg-3")
	assert.NotContains(t, body, "msg-1")
}

func TestMaxConnectionDuration_GroupHandler(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	b.DefineGroup("dash", []string{"metrics", "alerts"})
	handler := b.GroupHandler("dash", WithMaxConnectionDuration(100*time.Millisecond))

	rec := newFlushRecorder()
	req := httptest.NewRequest("GET", "/sse/dash", http.NoBody)

	// Publish some data so we can verify messages flow before close.
	go func() {
		time.Sleep(20 * time.Millisecond)
		b.Publish("metrics", "cpu=42")
	}()

	start := time.Now()
	done := make(chan struct{})
	go func() {
		handler.ServeHTTP(rec, req)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("handler did not return after max connection duration")
	}

	elapsed := time.Since(start)
	assert.GreaterOrEqual(t, elapsed, 100*time.Millisecond)
	assert.Less(t, elapsed, 200*time.Millisecond)

	body := rec.Body.String()
	assert.Contains(t, body, "retry: 1000")
	assert.Contains(t, body, ": max connection duration reached")
	assert.Contains(t, body, "cpu=42")
}

func TestMaxConnectionDuration_DynamicGroupHandler(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	b.DynamicGroup("dyn", func(_ *http.Request) []string {
		return []string{"a", "b"}
	})
	handler := b.DynamicGroupHandler("dyn", WithMaxConnectionDuration(100*time.Millisecond))

	rec := newFlushRecorder()
	req := httptest.NewRequest("GET", "/sse/dyn", http.NoBody)

	start := time.Now()
	done := make(chan struct{})
	go func() {
		handler.ServeHTTP(rec, req)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("handler did not return after max connection duration")
	}

	elapsed := time.Since(start)
	assert.GreaterOrEqual(t, elapsed, 100*time.Millisecond)
	assert.Less(t, elapsed, 200*time.Millisecond)

	body := rec.Body.String()
	assert.Contains(t, body, "retry: 1000")
	assert.Contains(t, body, ": max connection duration reached")
}

func TestMaxConnectionDuration_Zero_Disabled(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	// WithMaxConnectionDuration(0) should be a no-op.
	handler := b.SSEHandler("events", WithMaxConnectionDuration(0))

	rec := newFlushRecorder()
	req := httptest.NewRequest("GET", "/sse", http.NoBody)
	ctx, cancel := contextWithCancel(req)
	req = req.WithContext(ctx)

	done := make(chan struct{})
	go func() {
		handler.ServeHTTP(rec, req)
		close(done)
	}()

	// Should not close on its own within 150ms.
	time.Sleep(150 * time.Millisecond)
	select {
	case <-done:
		t.Fatal("handler should not have returned with zero duration")
	default:
	}

	cancel()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("handler did not return after context cancel")
	}

	// Body should not contain the close comment.
	require.NotContains(t, rec.Body.String(), ": max connection duration reached")
}
