package tavern

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestOnReconnect_FiresOnReconnection(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	b.SetReplayPolicy("events", 10)
	b.PublishWithID("events", "1", "msg-1")
	b.PublishWithID("events", "2", "msg-2")
	b.PublishWithID("events", "3", "msg-3")

	var mu sync.Mutex
	var captured ReconnectInfo
	called := make(chan struct{}, 1)

	b.OnReconnect("events", func(info ReconnectInfo) {
		mu.Lock()
		captured = info
		mu.Unlock()
		called <- struct{}{}
	})

	// Reconnect from ID "1" — should miss msg-2 and msg-3.
	ch, unsub := b.SubscribeFromID("events", "1")
	defer unsub()

	select {
	case <-called:
	case <-time.After(time.Second):
		t.Fatal("OnReconnect callback was not fired")
	}

	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, "events", captured.Topic)
	assert.Equal(t, "1", captured.LastEventID)
	assert.Equal(t, 2, captured.MissedCount)
	assert.Greater(t, captured.Gap, time.Duration(0))

	// Drain channel.
	drainChannel(ch)
}

func TestOnReconnect_DoesNotFireWithoutLastEventID(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	b.SetReplayPolicy("events", 10)
	b.PublishWithID("events", "1", "msg-1")

	called := make(chan struct{}, 1)
	b.OnReconnect("events", func(_ ReconnectInfo) {
		called <- struct{}{}
	})

	// Subscribe without Last-Event-ID.
	ch, unsub := b.SubscribeFromID("events", "")
	defer unsub()

	select {
	case <-called:
		t.Fatal("OnReconnect should not fire when lastEventID is empty")
	case <-time.After(50 * time.Millisecond):
		// expected
	}

	drainChannel(ch)
}

func TestOnReconnect_FiresEvenOnGap(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	b.SetReplayPolicy("events", 2)
	b.PublishWithID("events", "1", "msg-1")
	b.PublishWithID("events", "2", "msg-2")
	b.PublishWithID("events", "3", "msg-3")
	// ID "1" has rolled out of the log.

	reconnectCalled := make(chan struct{}, 1)
	gapCalled := make(chan struct{}, 1)

	b.OnReconnect("events", func(_ ReconnectInfo) {
		reconnectCalled <- struct{}{}
	})
	b.OnReplayGap("events", func(_ *SubscriberInfo, _ string) {
		gapCalled <- struct{}{}
	})

	ch, unsub := b.SubscribeFromID("events", "1")
	defer unsub()

	// Both should fire.
	select {
	case <-reconnectCalled:
	case <-time.After(time.Second):
		t.Fatal("OnReconnect was not fired on gap")
	}
	select {
	case <-gapCalled:
	case <-time.After(time.Second):
		t.Fatal("OnReplayGap was not fired")
	}

	drainChannel(ch)
}

func TestOnReconnect_GapDurationAndMissedCount(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	b.SetReplayPolicy("events", 10)
	b.PublishWithID("events", "1", "msg-1")
	time.Sleep(10 * time.Millisecond) // ensure measurable gap
	b.PublishWithID("events", "2", "msg-2")
	b.PublishWithID("events", "3", "msg-3")
	b.PublishWithID("events", "4", "msg-4")

	var captured ReconnectInfo
	done := make(chan struct{}, 1)

	b.OnReconnect("events", func(info ReconnectInfo) {
		captured = info
		done <- struct{}{}
	})

	ch, unsub := b.SubscribeFromID("events", "1")
	defer unsub()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("callback not fired")
	}

	assert.Equal(t, 3, captured.MissedCount)
	assert.Greater(t, captured.Gap, 10*time.Millisecond)

	drainChannel(ch)
}

func TestOnReconnect_SendToSubscriber(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	b.SetReplayPolicy("events", 10)
	b.PublishWithID("events", "1", "msg-1")

	b.OnReconnect("events", func(info ReconnectInfo) {
		info.SendToSubscriber("custom-catchup")
	})

	ch, unsub := b.SubscribeFromID("events", "1")
	defer unsub()

	// Should receive: reconnected control event, then custom-catchup from callback.
	var msgs []string
	timeout := time.After(time.Second)
loop:
	for i := 0; i < 3; i++ {
		select {
		case msg := <-ch:
			msgs = append(msgs, msg)
		case <-timeout:
			break loop
		}
		if len(msgs) >= 2 {
			break
		}
	}

	// The control event should be present.
	found := false
	for _, m := range msgs {
		if strings.Contains(m, "custom-catchup") {
			found = true
		}
	}
	assert.True(t, found, "expected custom-catchup message from SendToSubscriber, got: %v", msgs)
}

func TestOnReconnect_MultipleCallbacks(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	b.SetReplayPolicy("events", 10)
	b.PublishWithID("events", "1", "msg-1")

	var mu sync.Mutex
	var count int
	done := make(chan struct{})

	for i := 0; i < 3; i++ {
		b.OnReconnect("events", func(_ ReconnectInfo) {
			mu.Lock()
			count++
			if count == 3 {
				close(done)
			}
			mu.Unlock()
		})
	}

	ch, unsub := b.SubscribeFromID("events", "1")
	defer unsub()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("not all callbacks fired")
	}

	mu.Lock()
	assert.Equal(t, 3, count)
	mu.Unlock()

	drainChannel(ch)
}

func TestOnReconnect_ClosedBrokerNoop(t *testing.T) {
	b := NewSSEBroker()
	b.Close()

	// Should not panic.
	b.OnReconnect("events", func(_ ReconnectInfo) {})
}

func TestReconnectedControlEvent(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	b.SetReplayPolicy("events", 10)
	b.PublishWithID("events", "1", "msg-1")
	b.PublishWithID("events", "2", "msg-2")

	ch, unsub := b.SubscribeFromID("events", "1")
	defer unsub()

	// First message should be the reconnected control event.
	select {
	case msg := <-ch:
		assert.Contains(t, msg, "event: tavern-reconnected")
	case <-time.After(time.Second):
		t.Fatal("no control event received")
	}

	// Then the replayed message.
	select {
	case msg := <-ch:
		assert.Equal(t, "msg-2", msg)
	case <-time.After(time.Second):
		t.Fatal("no replay message received")
	}
}

func TestBundleOnReconnect(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	b.SetReplayPolicy("events", 10)
	b.SetBundleOnReconnect("events", true)

	b.PublishWithID("events", "1", "msg-1")
	b.PublishWithID("events", "2", "msg-2")
	b.PublishWithID("events", "3", "msg-3")

	ch, unsub := b.SubscribeFromID("events", "1")
	defer unsub()

	// First: reconnected control event.
	select {
	case msg := <-ch:
		assert.Contains(t, msg, "event: tavern-reconnected")
	case <-time.After(time.Second):
		t.Fatal("no control event")
	}

	// Second: bundled replay (msg-2 and msg-3 in a single string).
	select {
	case msg := <-ch:
		assert.Contains(t, msg, "msg-2")
		assert.Contains(t, msg, "msg-3")
	case <-time.After(time.Second):
		t.Fatal("no bundled replay received")
	}

	// No more replay messages — they were bundled.
	select {
	case msg := <-ch:
		t.Fatalf("unexpected extra message: %s", msg)
	case <-time.After(50 * time.Millisecond):
		// expected
	}
}

func TestBundleOnReconnect_Disabled(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	b.SetReplayPolicy("events", 10)
	// BundleOnReconnect is NOT set.

	b.PublishWithID("events", "1", "msg-1")
	b.PublishWithID("events", "2", "msg-2")
	b.PublishWithID("events", "3", "msg-3")

	ch, unsub := b.SubscribeFromID("events", "1")
	defer unsub()

	// Skip control event.
	<-ch

	// Should receive individual messages.
	select {
	case msg := <-ch:
		assert.Equal(t, "msg-2", msg)
	case <-time.After(time.Second):
		t.Fatal("no first replay")
	}
	select {
	case msg := <-ch:
		assert.Equal(t, "msg-3", msg)
	case <-time.After(time.Second):
		t.Fatal("no second replay")
	}
}

func TestSetBundleOnReconnect_Toggle(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	b.SetBundleOnReconnect("events", true)
	b.SetBundleOnReconnect("events", false)

	b.mu.RLock()
	_, exists := b.bundleOnReconnect["events"]
	b.mu.RUnlock()
	assert.False(t, exists, "bundle should be removed when set to false")
}

func TestSetBundleOnReconnect_ClosedBrokerNoop(t *testing.T) {
	b := NewSSEBroker()
	b.Close()

	// Should not panic.
	b.SetBundleOnReconnect("events", true)
}

func TestBundleOnReconnect_NoLastEventID(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	b.SetReplayPolicy("events", 10)
	b.SetBundleOnReconnect("events", true)

	b.PublishWithID("events", "1", "msg-1")
	b.PublishWithID("events", "2", "msg-2")

	// Subscribe without Last-Event-ID: should get individual replays, no bundling.
	ch, unsub := b.SubscribeFromID("events", "")
	defer unsub()

	select {
	case msg := <-ch:
		assert.Equal(t, "msg-1", msg)
	case <-time.After(time.Second):
		t.Fatal("no first replay")
	}
	select {
	case msg := <-ch:
		assert.Equal(t, "msg-2", msg)
	case <-time.After(time.Second):
		t.Fatal("no second replay")
	}
}

func TestOnReconnect_GapWithZeroMissedAndGap(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	b.SetReplayPolicy("events", 2)
	b.PublishWithID("events", "1", "msg-1")
	b.PublishWithID("events", "2", "msg-2")
	b.PublishWithID("events", "3", "msg-3")
	// ID "1" rolled out.

	var captured ReconnectInfo
	done := make(chan struct{}, 1)

	b.OnReconnect("events", func(info ReconnectInfo) {
		captured = info
		done <- struct{}{}
	})

	ch, unsub := b.SubscribeFromID("events", "1")
	defer unsub()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("callback not fired")
	}

	// ID not found in log, so Gap and MissedCount should be zero.
	assert.Equal(t, time.Duration(0), captured.Gap)
	assert.Equal(t, 0, captured.MissedCount)

	drainChannel(ch)
}

func TestSSEHandler_ReconnectedControlEvent(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	b.SetReplayPolicy("events", 10)
	b.PublishWithID("events", "1", "msg-1")
	b.PublishWithID("events", "2", "msg-2")

	handler := b.SSEHandler("events")

	rec := newFlushRecorder()
	req := httptest.NewRequest("GET", "/sse", http.NoBody)
	req.Header.Set("Last-Event-ID", "1")
	ctx, cancel := context.WithCancel(req.Context())
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
	assert.Contains(t, body, "event: tavern-reconnected")
	assert.Contains(t, body, "msg-2")
}

func TestSSEHandler_BundledReplay(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	b.SetReplayPolicy("events", 10)
	b.SetBundleOnReconnect("events", true)

	b.PublishWithID("events", "1", "msg-1")
	b.PublishWithID("events", "2", "msg-2")
	b.PublishWithID("events", "3", "msg-3")

	handler := b.SSEHandler("events")

	rec := newFlushRecorder()
	req := httptest.NewRequest("GET", "/sse", http.NoBody)
	req.Header.Set("Last-Event-ID", "1")
	ctx, cancel := context.WithCancel(req.Context())
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
	assert.Contains(t, body, "event: tavern-reconnected")
	assert.Contains(t, body, "msg-2")
	assert.Contains(t, body, "msg-3")
}

func TestSSEHandler_OnReconnectWithHandler(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	b.SetReplayPolicy("events", 10)
	b.PublishWithID("events", "1", "msg-1")

	var captured ReconnectInfo
	done := make(chan struct{}, 1)

	b.OnReconnect("events", func(info ReconnectInfo) {
		captured = info
		done <- struct{}{}
	})

	handler := b.SSEHandler("events")

	rec := newFlushRecorder()
	req := httptest.NewRequest("GET", "/sse", http.NoBody)
	req.Header.Set("Last-Event-ID", "1")
	ctx, cancel := context.WithCancel(req.Context())
	req = req.WithContext(ctx)

	handlerDone := make(chan struct{})
	go func() {
		handler.ServeHTTP(rec, req)
		close(handlerDone)
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("OnReconnect not fired via SSEHandler")
	}

	cancel()
	<-handlerDone

	assert.Equal(t, "events", captured.Topic)
	assert.Equal(t, "1", captured.LastEventID)
}

func TestOnReconnect_SubscriberIDFromMeta(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	b.SetReplayPolicy("events", 10)
	b.PublishWithID("events", "1", "msg-1")

	var captured ReconnectInfo
	done := make(chan struct{}, 1)

	b.OnReconnect("events", func(info ReconnectInfo) {
		captured = info
		done <- struct{}{}
	})

	// SubscribeFromID then set metadata on the subscriber.
	ch, unsub := b.SubscribeFromID("events", "1")
	defer unsub()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("callback not fired")
	}

	// SubscriberID comes from SubscriberInfo.ID which is empty by default.
	assert.Equal(t, "", captured.SubscriberID)

	drainChannel(ch)
}

func TestBundleOnReconnect_ClearedBySetReplayPolicy(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	b.SetReplayPolicy("events", 10)
	b.SetBundleOnReconnect("events", true)

	// Setting replay policy to 0 should clear bundle setting.
	b.SetReplayPolicy("events", 0)

	b.mu.RLock()
	_, exists := b.bundleOnReconnect["events"]
	b.mu.RUnlock()
	assert.False(t, exists, "bundle should be cleared when replay policy is set to 0")
}

func TestBundleOnReconnect_ClearedByClearReplay(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	b.SetReplayPolicy("events", 10)
	b.SetBundleOnReconnect("events", true)

	b.ClearReplay("events")

	b.mu.RLock()
	_, exists := b.bundleOnReconnect["events"]
	b.mu.RUnlock()
	assert.False(t, exists, "bundle should be cleared by ClearReplay")
}

func TestReconnectedControlEvent_Format(t *testing.T) {
	msg := reconnectedControlEvent()
	assert.Contains(t, msg, "event: tavern-reconnected")
	assert.Contains(t, msg, "data: ")
	assert.True(t, strings.HasSuffix(msg, "\n\n"), "SSE message must end with double newline")
}

func TestOnReconnect_MissedCountAtEndOfLog(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	b.SetReplayPolicy("events", 10)
	b.PublishWithID("events", "1", "msg-1")

	var captured ReconnectInfo
	done := make(chan struct{}, 1)

	b.OnReconnect("events", func(info ReconnectInfo) {
		captured = info
		done <- struct{}{}
	})

	// Reconnect from the last ID — 0 missed.
	ch, unsub := b.SubscribeFromID("events", "1")
	defer unsub()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("callback not fired")
	}

	assert.Equal(t, 0, captured.MissedCount)
	drainChannel(ch)
}

func TestBundleOnReconnect_SingleMessage(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	b.SetReplayPolicy("events", 10)
	b.SetBundleOnReconnect("events", true)

	b.PublishWithID("events", "1", "msg-1")
	b.PublishWithID("events", "2", "msg-2")

	ch, unsub := b.SubscribeFromID("events", "1")
	defer unsub()

	// Skip control event.
	<-ch

	// Single bundled message containing only msg-2.
	select {
	case msg := <-ch:
		assert.Equal(t, "msg-2", msg)
	case <-time.After(time.Second):
		t.Fatal("no bundled replay received")
	}
}

// drainChannel reads all available messages from a channel without blocking.
func drainChannel(ch <-chan string) {
	for {
		select {
		case _, ok := <-ch:
			if !ok {
				return
			}
		case <-time.After(50 * time.Millisecond):
			return
		}
	}
}

func TestOnReconnect_TopicIsolation(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	b.SetReplayPolicy("events-a", 10)
	b.SetReplayPolicy("events-b", 10)
	b.PublishWithID("events-a", "1", "a-1")
	b.PublishWithID("events-b", "1", "b-1")

	calledA := make(chan struct{}, 1)
	calledB := make(chan struct{}, 1)

	b.OnReconnect("events-a", func(_ ReconnectInfo) { calledA <- struct{}{} })
	b.OnReconnect("events-b", func(_ ReconnectInfo) { calledB <- struct{}{} })

	// Only reconnect to events-a.
	ch, unsub := b.SubscribeFromID("events-a", "1")
	defer unsub()

	select {
	case <-calledA:
	case <-time.After(time.Second):
		t.Fatal("events-a callback not fired")
	}

	select {
	case <-calledB:
		t.Fatal("events-b callback should not have fired")
	case <-time.After(50 * time.Millisecond):
		// expected
	}

	drainChannel(ch)
}

func TestOnReconnect_ConcurrentSafety(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	b.SetReplayPolicy("events", 100)
	for i := 0; i < 50; i++ {
		b.PublishWithID("events", fmt.Sprintf("%d", i), fmt.Sprintf("msg-%d", i))
	}

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			b.OnReconnect("events", func(_ ReconnectInfo) {})
		}()
	}

	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ch, unsub := b.SubscribeFromID("events", "25")
			drainChannel(ch)
			unsub()
		}()
	}

	wg.Wait()
}
