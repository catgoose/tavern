package tavern

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDefineGroup_Basic(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	b.DefineGroup("dash", []string{"metrics", "alerts"})

	b.groupsMu.RLock()
	g, ok := b.groups["dash"]
	b.groupsMu.RUnlock()

	require.True(t, ok)
	assert.Equal(t, []string{"metrics", "alerts"}, g.topics)
}

func TestDefineGroup_ReplacePrevious(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	b.DefineGroup("dash", []string{"a"})
	b.DefineGroup("dash", []string{"b", "c"})

	b.groupsMu.RLock()
	g := b.groups["dash"]
	b.groupsMu.RUnlock()

	assert.Equal(t, []string{"b", "c"}, g.topics)
}

func TestDefineGroup_CopiesSlice(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	topics := []string{"a", "b"}
	b.DefineGroup("g", topics)
	topics[0] = "mutated"

	b.groupsMu.RLock()
	g := b.groups["g"]
	b.groupsMu.RUnlock()

	assert.Equal(t, "a", g.topics[0], "should not be affected by mutation of original slice")
}

func TestGroupHandler_NotDefined(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	handler := b.GroupHandler("nonexistent")

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/sse/group", http.NoBody)
	handler.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusNotFound, rec.Code)
	assert.Contains(t, rec.Body.String(), "not defined")
}

func TestGroupHandler_StreamsMultipleTopics(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	b.DefineGroup("dash", []string{"metrics", "alerts"})
	handler := b.GroupHandler("dash")

	rec := newFlushRecorder()
	req := httptest.NewRequest("GET", "/sse/dash", http.NoBody)
	ctx, cancel := contextWithCancel(req)
	req = req.WithContext(ctx)

	done := make(chan struct{})
	go func() {
		handler.ServeHTTP(rec, req)
		close(done)
	}()

	// Give handler time to subscribe.
	time.Sleep(50 * time.Millisecond)

	b.Publish("metrics", "cpu=42")
	b.Publish("alerts", "disk-full")

	// Wait for messages to flow through.
	time.Sleep(50 * time.Millisecond)
	cancel()
	<-done

	body := rec.Body.String()
	assert.Contains(t, body, "event: metrics")
	assert.Contains(t, body, "data: cpu=42")
	assert.Contains(t, body, "event: alerts")
	assert.Contains(t, body, "data: disk-full")
}

func TestGroupHandler_SSEHeaders(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	b.DefineGroup("g", []string{"a"})
	handler := b.GroupHandler("g")

	rec := newFlushRecorder()
	req := httptest.NewRequest("GET", "/sse/g", http.NoBody)
	ctx, cancel := contextWithCancel(req)
	req = req.WithContext(ctx)

	done := make(chan struct{})
	go func() {
		handler.ServeHTTP(rec, req)
		close(done)
	}()

	time.Sleep(20 * time.Millisecond)
	cancel()
	<-done

	assert.Equal(t, "text/event-stream", rec.Header().Get("Content-Type"))
	assert.Equal(t, "no-cache", rec.Header().Get("Cache-Control"))
	assert.Equal(t, "keep-alive", rec.Header().Get("Connection"))
}

func TestGroupHandler_CustomWriter(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	var written []string
	b.DefineGroup("g", []string{"t"})
	handler := b.GroupHandler("g", WithSSEWriter(func(w http.ResponseWriter, msg string) error {
		written = append(written, msg)
		return nil
	}))

	rec := newFlushRecorder()
	req := httptest.NewRequest("GET", "/sse/g", http.NoBody)
	ctx, cancel := contextWithCancel(req)
	req = req.WithContext(ctx)

	done := make(chan struct{})
	go func() {
		handler.ServeHTTP(rec, req)
		close(done)
	}()

	time.Sleep(50 * time.Millisecond)
	b.Publish("t", "hello")
	time.Sleep(50 * time.Millisecond)
	cancel()
	<-done

	require.Len(t, written, 1)
	assert.Contains(t, written[0], "event: t")
	assert.Contains(t, written[0], "data: hello")
}

func TestGroupHandler_ConnectionCloseUnsubscribes(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	b.DefineGroup("g", []string{"a", "b"})
	handler := b.GroupHandler("g")

	rec := newFlushRecorder()
	req := httptest.NewRequest("GET", "/sse/g", http.NoBody)
	ctx, cancel := contextWithCancel(req)
	req = req.WithContext(ctx)

	done := make(chan struct{})
	go func() {
		handler.ServeHTTP(rec, req)
		close(done)
	}()

	time.Sleep(50 * time.Millisecond)
	assert.True(t, b.HasSubscribers("a"))
	assert.True(t, b.HasSubscribers("b"))

	cancel()
	<-done

	// Give unsubscribe time to propagate.
	time.Sleep(50 * time.Millisecond)
	assert.False(t, b.HasSubscribers("a"))
	assert.False(t, b.HasSubscribers("b"))
}

func TestGroupHandler_LifecycleHooksPerTopic(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	firstFired := make(chan string, 3)
	lastFired := make(chan string, 3)
	for _, topic := range []string{"x", "y", "z"} {
		b.OnFirstSubscriber(topic, func(t string) { firstFired <- t })
		b.OnLastUnsubscribe(topic, func(t string) { lastFired <- t })
	}

	b.DefineGroup("g", []string{"x", "y", "z"})
	handler := b.GroupHandler("g")

	rec := newFlushRecorder()
	req := httptest.NewRequest("GET", "/sse/g", http.NoBody)
	ctx, cancel := contextWithCancel(req)
	req = req.WithContext(ctx)

	done := make(chan struct{})
	go func() {
		handler.ServeHTTP(rec, req)
		close(done)
	}()

	// Collect OnFirstSubscriber fires.
	firstTopics := collectTopics(t, firstFired, 3, time.Second)
	assert.ElementsMatch(t, []string{"x", "y", "z"}, firstTopics)

	cancel()
	<-done

	// Collect OnLastUnsubscribe fires.
	lastTopics := collectTopics(t, lastFired, 3, time.Second)
	assert.ElementsMatch(t, []string{"x", "y", "z"}, lastTopics)
}

func TestGroupHandler_BrokerClose(t *testing.T) {
	b := NewSSEBroker()

	b.DefineGroup("g", []string{"a"})
	handler := b.GroupHandler("g")

	rec := newFlushRecorder()
	req := httptest.NewRequest("GET", "/sse/g", http.NoBody)

	done := make(chan struct{})
	go func() {
		handler.ServeHTTP(rec, req)
		close(done)
	}()

	time.Sleep(20 * time.Millisecond)
	b.Close()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("handler did not return after broker close")
	}
}

func TestDynamicGroup_Basic(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	b.DynamicGroup("user-dash", func(_ *http.Request) []string {
		return []string{"metrics", "alerts"}
	})

	handler := b.DynamicGroupHandler("user-dash")

	rec := newFlushRecorder()
	req := httptest.NewRequest("GET", "/sse/user-dash", http.NoBody)
	ctx, cancel := contextWithCancel(req)
	req = req.WithContext(ctx)

	done := make(chan struct{})
	go func() {
		handler.ServeHTTP(rec, req)
		close(done)
	}()

	time.Sleep(50 * time.Millisecond)
	b.Publish("metrics", "cpu=99")
	b.Publish("alerts", "warning")

	time.Sleep(50 * time.Millisecond)
	cancel()
	<-done

	body := rec.Body.String()
	assert.Contains(t, body, "event: metrics")
	assert.Contains(t, body, "data: cpu=99")
	assert.Contains(t, body, "event: alerts")
	assert.Contains(t, body, "data: warning")
}

func TestDynamicGroupHandler_NotDefined(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	handler := b.DynamicGroupHandler("nonexistent")

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/sse/dyn", http.NoBody)
	handler.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusNotFound, rec.Code)
	assert.Contains(t, rec.Body.String(), "not defined")
}

func TestDynamicGroup_PerRequestAuthorization(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	b.DynamicGroup("auth-dash", func(r *http.Request) []string {
		topics := []string{"public"}
		if r.Header.Get("X-Admin") == "true" {
			topics = append(topics, "admin-only")
		}
		return topics
	})

	// Admin request — should see both topics.
	handler := b.DynamicGroupHandler("auth-dash")

	adminRec := newFlushRecorder()
	adminReq := httptest.NewRequest("GET", "/sse/dash", http.NoBody)
	adminReq.Header.Set("X-Admin", "true")
	adminCtx, adminCancel := contextWithCancel(adminReq)
	adminReq = adminReq.WithContext(adminCtx)

	adminDone := make(chan struct{})
	go func() {
		handler.ServeHTTP(adminRec, adminReq)
		close(adminDone)
	}()

	// Regular request — should only see public.
	userRec := newFlushRecorder()
	userReq := httptest.NewRequest("GET", "/sse/dash", http.NoBody)
	userCtx, userCancel := contextWithCancel(userReq)
	userReq = userReq.WithContext(userCtx)

	userDone := make(chan struct{})
	go func() {
		handler.ServeHTTP(userRec, userReq)
		close(userDone)
	}()

	time.Sleep(50 * time.Millisecond)

	b.Publish("public", "hello-everyone")
	b.Publish("admin-only", "secret-data")

	time.Sleep(50 * time.Millisecond)
	adminCancel()
	userCancel()
	<-adminDone
	<-userDone

	adminBody := adminRec.Body.String()
	assert.Contains(t, adminBody, "data: hello-everyone")
	assert.Contains(t, adminBody, "data: secret-data")

	userBody := userRec.Body.String()
	assert.Contains(t, userBody, "data: hello-everyone")
	assert.NotContains(t, userBody, "secret-data")
}

func TestDynamicGroup_LifecycleHooks(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	firstFired := make(chan string, 2)
	lastFired := make(chan string, 2)
	for _, topic := range []string{"a", "b"} {
		b.OnFirstSubscriber(topic, func(t string) { firstFired <- t })
		b.OnLastUnsubscribe(topic, func(t string) { lastFired <- t })
	}

	b.DynamicGroup("dyn", func(_ *http.Request) []string {
		return []string{"a", "b"}
	})
	handler := b.DynamicGroupHandler("dyn")

	rec := newFlushRecorder()
	req := httptest.NewRequest("GET", "/sse/dyn", http.NoBody)
	ctx, cancel := contextWithCancel(req)
	req = req.WithContext(ctx)

	done := make(chan struct{})
	go func() {
		handler.ServeHTTP(rec, req)
		close(done)
	}()

	firstTopics := collectTopics(t, firstFired, 2, time.Second)
	assert.ElementsMatch(t, []string{"a", "b"}, firstTopics)

	cancel()
	<-done

	lastTopics := collectTopics(t, lastFired, 2, time.Second)
	assert.ElementsMatch(t, []string{"a", "b"}, lastTopics)
}

func TestDynamicGroup_ConnectionCloseUnsubscribes(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	b.DynamicGroup("dyn", func(_ *http.Request) []string {
		return []string{"x", "y"}
	})
	handler := b.DynamicGroupHandler("dyn")

	rec := newFlushRecorder()
	req := httptest.NewRequest("GET", "/sse/dyn", http.NoBody)
	ctx, cancel := contextWithCancel(req)
	req = req.WithContext(ctx)

	done := make(chan struct{})
	go func() {
		handler.ServeHTTP(rec, req)
		close(done)
	}()

	time.Sleep(50 * time.Millisecond)
	assert.True(t, b.HasSubscribers("x"))
	assert.True(t, b.HasSubscribers("y"))

	cancel()
	<-done
	time.Sleep(50 * time.Millisecond)

	assert.False(t, b.HasSubscribers("x"))
	assert.False(t, b.HasSubscribers("y"))
}

func TestGroupHandler_SSEMessageFormat(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	b.DefineGroup("g", []string{"topic1"})
	handler := b.GroupHandler("g")

	rec := newFlushRecorder()
	req := httptest.NewRequest("GET", "/sse/g", http.NoBody)
	ctx, cancel := contextWithCancel(req)
	req = req.WithContext(ctx)

	done := make(chan struct{})
	go func() {
		handler.ServeHTTP(rec, req)
		close(done)
	}()

	time.Sleep(50 * time.Millisecond)
	b.Publish("topic1", "payload")
	time.Sleep(50 * time.Millisecond)
	cancel()
	<-done

	body := rec.Body.String()
	// Should contain properly formatted SSE with event type = topic name.
	assert.Contains(t, body, "event: topic1\n")
	assert.Contains(t, body, "data: payload\n")
	// Message should end with double newline (SSE spec).
	assert.True(t, strings.Contains(body, "\n\n"), "SSE message should be terminated by double newline")
}

func TestGroupHandler_MultilineData(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	b.DefineGroup("g", []string{"t"})
	handler := b.GroupHandler("g")

	rec := newFlushRecorder()
	req := httptest.NewRequest("GET", "/sse/g", http.NoBody)
	ctx, cancel := contextWithCancel(req)
	req = req.WithContext(ctx)

	done := make(chan struct{})
	go func() {
		handler.ServeHTTP(rec, req)
		close(done)
	}()

	time.Sleep(50 * time.Millisecond)
	b.Publish("t", "line1\nline2")
	time.Sleep(50 * time.Millisecond)
	cancel()
	<-done

	body := rec.Body.String()
	assert.Contains(t, body, "data: line1\n")
	assert.Contains(t, body, "data: line2\n")
}

func TestGroupHandlerLastEventID(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	b.SetReplayPolicy("metrics", 10)
	b.SetReplayPolicy("alerts", 10)
	b.PublishWithID("metrics", "1", "m1")
	b.PublishWithID("metrics", "2", "m2")
	b.PublishWithID("alerts", "1", "a1")
	b.PublishWithID("alerts", "2", "a2")

	b.DefineGroup("dash", []string{"metrics", "alerts"})
	handler := b.GroupHandler("dash")

	rec := newFlushRecorder()
	req := httptest.NewRequest("GET", "/sse/dash", http.NoBody)
	req.Header.Set("Last-Event-ID", "1") // replay after ID "1"
	ctx, cancel := contextWithCancel(req)
	req = req.WithContext(ctx)

	done := make(chan struct{})
	go func() {
		handler.ServeHTTP(rec, req)
		close(done)
	}()

	time.Sleep(100 * time.Millisecond)
	cancel()
	<-done

	body := rec.Body.String()
	// Should replay m2 and a2 (after ID "1"), but not m1 or a1.
	assert.Contains(t, body, "m2")
	assert.Contains(t, body, "a2")
	assert.NotContains(t, body, "data: m1\n")
	assert.NotContains(t, body, "data: a1\n")
}

func TestDynamicGroupHandlerLastEventID(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	b.SetReplayPolicy("x", 10)
	b.SetReplayPolicy("y", 10)
	b.PublishWithID("x", "a", "x1")
	b.PublishWithID("x", "b", "x2")
	b.PublishWithID("y", "a", "y1")
	b.PublishWithID("y", "b", "y2")

	b.DynamicGroup("dyn", func(_ *http.Request) []string {
		return []string{"x", "y"}
	})
	handler := b.DynamicGroupHandler("dyn")

	rec := newFlushRecorder()
	req := httptest.NewRequest("GET", "/sse/dyn", http.NoBody)
	req.Header.Set("Last-Event-ID", "a") // replay after ID "a"
	ctx, cancel := contextWithCancel(req)
	req = req.WithContext(ctx)

	done := make(chan struct{})
	go func() {
		handler.ServeHTTP(rec, req)
		close(done)
	}()

	time.Sleep(100 * time.Millisecond)
	cancel()
	<-done

	body := rec.Body.String()
	assert.Contains(t, body, "x2")
	assert.Contains(t, body, "y2")
	assert.NotContains(t, body, "data: x1\n")
	assert.NotContains(t, body, "data: y1\n")
}

func TestGroupHandler_ReplayPreservesControlEvents(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	b.SetReplayPolicy("metrics", 10)
	b.PublishWithID("metrics", "1", "m1")
	b.PublishWithID("metrics", "2", "m2")

	b.DefineGroup("dash", []string{"metrics"})
	handler := b.GroupHandler("dash")

	rec := newFlushRecorder()
	req := httptest.NewRequest("GET", "/sse/dash", http.NoBody)
	req.Header.Set("Last-Event-ID", "1")
	ctx, cancel := contextWithCancel(req)
	req = req.WithContext(ctx)

	done := make(chan struct{})
	go func() {
		handler.ServeHTTP(rec, req)
		close(done)
	}()

	time.Sleep(100 * time.Millisecond)
	cancel()
	<-done

	body := rec.Body.String()

	// The tavern-reconnected control event should appear as its own event type,
	// NOT wrapped inside another event type (no double-wrapping).
	assert.Contains(t, body, "event: tavern-reconnected\n",
		"control event should appear with its own event type")
	assert.NotContains(t, body, "data: event: tavern-reconnected",
		"control event must not be nested inside a data field")
}

func TestGroupHandler_ReplayPreservesEventIDs(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	b.SetReplayPolicy("metrics", 10)
	b.PublishWithID("metrics", "1", "m1")
	b.PublishWithID("metrics", "2", "m2")
	b.PublishWithID("metrics", "3", "m3")

	b.DefineGroup("dash", []string{"metrics"})
	handler := b.GroupHandler("dash")

	rec := newFlushRecorder()
	req := httptest.NewRequest("GET", "/sse/dash", http.NoBody)
	req.Header.Set("Last-Event-ID", "1")
	ctx, cancel := contextWithCancel(req)
	req = req.WithContext(ctx)

	done := make(chan struct{})
	go func() {
		handler.ServeHTTP(rec, req)
		close(done)
	}()

	time.Sleep(100 * time.Millisecond)
	cancel()
	<-done

	body := rec.Body.String()

	// Replayed messages should preserve their event IDs.
	assert.Contains(t, body, "id: 2\n", "event ID 2 should be preserved in replay")
	assert.Contains(t, body, "id: 3\n", "event ID 3 should be preserved in replay")
	// And the topic should be used as the event type.
	assert.Contains(t, body, "event: metrics\n", "topic should be used as event type")
}

func TestGroupHandler_LiveMessagesPreserveIDs(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	b.SetReplayPolicy("alerts", 10)
	b.DefineGroup("dash", []string{"alerts"})
	handler := b.GroupHandler("dash")

	rec := newFlushRecorder()
	req := httptest.NewRequest("GET", "/sse/dash", http.NoBody)
	ctx, cancel := contextWithCancel(req)
	req = req.WithContext(ctx)

	done := make(chan struct{})
	go func() {
		handler.ServeHTTP(rec, req)
		close(done)
	}()

	time.Sleep(50 * time.Millisecond)
	b.PublishWithID("alerts", "evt-42", "disk-full")
	time.Sleep(50 * time.Millisecond)
	cancel()
	<-done

	body := rec.Body.String()
	assert.Contains(t, body, "event: alerts\n", "topic should be event type")
	assert.Contains(t, body, "data: disk-full\n", "payload should be in data field")
	assert.Contains(t, body, "id: evt-42\n", "event ID should be preserved for live messages")
}

func TestGroupHandler_NoNestedControlEventsInReplay(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	// Set up two topics to exercise the multi-topic replay path.
	b.SetReplayPolicy("t1", 10)
	b.SetReplayPolicy("t2", 10)
	b.PublishWithID("t1", "1", "msg-t1")
	b.PublishWithID("t2", "1", "msg-t2")
	b.PublishWithID("t1", "2", "msg-t1-2")
	b.PublishWithID("t2", "2", "msg-t2-2")

	b.DefineGroup("g", []string{"t1", "t2"})
	handler := b.GroupHandler("g")

	rec := newFlushRecorder()
	req := httptest.NewRequest("GET", "/sse/g", http.NoBody)
	req.Header.Set("Last-Event-ID", "1")
	ctx, cancel := contextWithCancel(req)
	req = req.WithContext(ctx)

	done := make(chan struct{})
	go func() {
		handler.ServeHTTP(rec, req)
		close(done)
	}()

	time.Sleep(100 * time.Millisecond)
	cancel()
	<-done

	body := rec.Body.String()

	// Verify no control event content appears nested inside data fields.
	assert.NotContains(t, body, "data: event: tavern-",
		"control events must not appear as data payloads")
	assert.NotContains(t, body, "data: event: tavern-reconnected",
		"tavern-reconnected must not be nested")

	// Verify control events appear correctly.
	assert.Contains(t, body, "event: tavern-reconnected\n",
		"reconnected control event should be present")

	// Verify replay payloads appear with correct topic event types and IDs.
	assert.Contains(t, body, "event: t1\n")
	assert.Contains(t, body, "event: t2\n")
	assert.Contains(t, body, "id: 2\n")
}

// collectTopics drains n topic names from ch within the given timeout.
func collectTopics(t *testing.T, ch <-chan string, n int, timeout time.Duration) []string {
	t.Helper()
	topics := make([]string, 0, n)
	deadline := time.After(timeout)
	for range n {
		select {
		case topic := <-ch:
			topics = append(topics, topic)
		case <-deadline:
			t.Fatalf("timed out collecting topics, got %d/%d: %v", len(topics), n, topics)
		}
	}
	return topics
}
