package tavern

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPublishWithID_BasicDelivery(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	ch, unsub := b.Subscribe("events")
	defer unsub()

	b.PublishWithID("events", "id-1", "hello")

	select {
	case msg := <-ch:
		assert.Equal(t, "hello", msg)
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for message")
	}
}

func TestSubscribeFromID_ReplaysAfterID(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	b.SetReplayPolicy("events", 10)
	for i := 1; i <= 5; i++ {
		b.PublishWithID("events", fmt.Sprintf("%d", i), fmt.Sprintf("msg-%d", i))
	}

	ch, unsub := b.SubscribeFromID("events", "3")
	defer unsub()

	// First message is the reconnected control event.
	select {
	case msg := <-ch:
		assert.Contains(t, msg, "event: tavern-reconnected")
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for reconnected control event")
	}

	var got []string
	for range 2 {
		select {
		case msg := <-ch:
			got = append(got, msg)
		case <-time.After(time.Second):
			t.Fatal("timed out waiting for replay message")
		}
	}
	assert.Equal(t, []string{"msg-4", "msg-5"}, got)

	// Verify no more replay messages are buffered.
	select {
	case msg := <-ch:
		t.Fatalf("unexpected extra message: %s", msg)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestSubscribeFromID_EmptyID(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	b.SetReplayPolicy("events", 10)
	for i := 1; i <= 3; i++ {
		b.PublishWithID("events", fmt.Sprintf("%d", i), fmt.Sprintf("msg-%d", i))
	}

	ch, unsub := b.SubscribeFromID("events", "")
	defer unsub()

	var got []string
	for range 3 {
		select {
		case msg := <-ch:
			got = append(got, msg)
		case <-time.After(time.Second):
			t.Fatal("timed out waiting for replay message")
		}
	}
	assert.Equal(t, []string{"msg-1", "msg-2", "msg-3"}, got)
}

func TestSubscribeFromID_UnknownID(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	b.SetReplayPolicy("events", 10)
	for i := 1; i <= 3; i++ {
		b.PublishWithID("events", fmt.Sprintf("%d", i), fmt.Sprintf("msg-%d", i))
	}

	ch, unsub := b.SubscribeFromID("events", "unknown-id")
	defer unsub()

	// First message is the reconnected control event.
	select {
	case msg := <-ch:
		assert.Contains(t, msg, "event: tavern-reconnected")
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for reconnected control event")
	}

	// No replay should occur for an unknown ID.
	select {
	case msg := <-ch:
		t.Fatalf("expected no replay, got: %s", msg)
	case <-time.After(50 * time.Millisecond):
	}

	// Live messages should still work.
	b.PublishWithID("events", "4", "live-msg")
	select {
	case msg := <-ch:
		assert.Equal(t, "live-msg", msg)
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for live message")
	}
}

func TestSubscribeFromID_AllCaughtUp(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	b.SetReplayPolicy("events", 10)
	for i := 1; i <= 3; i++ {
		b.PublishWithID("events", fmt.Sprintf("%d", i), fmt.Sprintf("msg-%d", i))
	}

	// Subscribe from the latest ID: nothing to replay (but reconnect event fires).
	ch, unsub := b.SubscribeFromID("events", "3")
	defer unsub()

	// First message is the reconnected control event.
	select {
	case msg := <-ch:
		assert.Contains(t, msg, "event: tavern-reconnected")
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for reconnected control event")
	}

	// No replay messages.
	select {
	case msg := <-ch:
		t.Fatalf("expected no replay, got: %s", msg)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestPublishWithID_RespectsReplayPolicy(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	b.SetReplayPolicy("events", 3)
	for i := 1; i <= 5; i++ {
		b.PublishWithID("events", fmt.Sprintf("%d", i), fmt.Sprintf("msg-%d", i))
	}

	// Empty ID: replay all cached (should be last 3).
	ch, unsub := b.SubscribeFromID("events", "")
	defer unsub()

	var got []string
	for range 3 {
		select {
		case msg := <-ch:
			got = append(got, msg)
		case <-time.After(time.Second):
			t.Fatal("timed out waiting for replay message")
		}
	}
	assert.Equal(t, []string{"msg-3", "msg-4", "msg-5"}, got)
}

func TestPublishWithID_AfterClose(t *testing.T) {
	b := NewSSEBroker()
	b.Close()

	// Should not panic.
	require.NotPanics(t, func() {
		b.PublishWithID("events", "id-1", "hello")
	})
}

func TestSubscribeFromID_LifecycleHooks(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	var mu sync.Mutex
	var fired bool
	b.OnFirstSubscriber("events", func(topic string) {
		mu.Lock()
		fired = true
		mu.Unlock()
	})

	_, unsub := b.SubscribeFromID("events", "")
	defer unsub()

	// Give the goroutine time to fire.
	time.Sleep(50 * time.Millisecond)
	mu.Lock()
	assert.True(t, fired, "OnFirstSubscriber should fire for SubscribeFromID")
	mu.Unlock()
}

func TestPublishWithID_UpdatesRegularReplayCache(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	b.SetReplayPolicy("events", 5)
	for i := 1; i <= 3; i++ {
		b.PublishWithID("events", fmt.Sprintf("%d", i), fmt.Sprintf("msg-%d", i))
	}

	// Regular Subscribe should also get the replay from PublishWithID.
	ch, unsub := b.Subscribe("events")
	defer unsub()

	var got []string
	for range 3 {
		select {
		case msg := <-ch:
			got = append(got, msg)
		case <-time.After(time.Second):
			t.Fatal("timed out waiting for replay message")
		}
	}
	assert.Equal(t, []string{"msg-1", "msg-2", "msg-3"}, got)
}
