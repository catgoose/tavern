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
		assert.Contains(t, msg, "hello")
		assert.Contains(t, msg, "id: id-1")
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

	// Replay messages come first, then reconnected control event.
	var got []string
	for range 2 {
		select {
		case msg := <-ch:
			got = append(got, msg)
		case <-time.After(time.Second):
			t.Fatal("timed out waiting for replay message")
		}
	}
	require.Len(t, got, 2)
	assert.Contains(t, got[0], "msg-4")
	assert.Contains(t, got[0], "id: 4")
	assert.Contains(t, got[1], "msg-5")
	assert.Contains(t, got[1], "id: 5")

	// Reconnected control event with replay stats comes after replay.
	select {
	case msg := <-ch:
		assert.Contains(t, msg, "event: tavern-reconnected")
		assert.Contains(t, msg, `"replayDelivered":2`)
		assert.Contains(t, msg, `"replayDropped":0`)
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for reconnected control event")
	}

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
	require.Len(t, got, 3)
	for i, msg := range got {
		assert.Contains(t, msg, fmt.Sprintf("msg-%d", i+1))
		assert.Contains(t, msg, fmt.Sprintf("id: %d", i+1))
	}
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
		assert.Contains(t, msg, "live-msg")
		assert.Contains(t, msg, "id: 4")
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

	// Reconnected control event (no replay messages preceded it).
	select {
	case msg := <-ch:
		assert.Contains(t, msg, "event: tavern-reconnected")
		assert.Contains(t, msg, `"replayDelivered":0`)
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
	require.Len(t, got, 3)
	assert.Contains(t, got[0], "msg-3")
	assert.Contains(t, got[0], "id: 3")
	assert.Contains(t, got[1], "msg-4")
	assert.Contains(t, got[1], "id: 4")
	assert.Contains(t, got[2], "msg-5")
	assert.Contains(t, got[2], "id: 5")
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
	// Note: replayCache stores raw messages without IDs, so regular Subscribe
	// gets raw messages on replay (IDs are only injected for live delivery
	// and SubscribeFromID replay).
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

func TestPublishWithID_SSEFormattedMessage(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	ch, unsub := b.Subscribe("events")
	defer unsub()

	// Publish a pre-formatted SSE message with PublishWithID.
	sseMsg := NewSSEMessage("update", "payload").String()
	b.PublishWithID("events", "evt-42", sseMsg)

	select {
	case msg := <-ch:
		assert.Contains(t, msg, "event: update")
		assert.Contains(t, msg, "data: payload")
		assert.Contains(t, msg, "id: evt-42")
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for message")
	}
}

func TestPublishWithID_SSEMessageWithExistingID(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	ch, unsub := b.Subscribe("events")
	defer unsub()

	// Publish a pre-formatted SSE message that already has an id field.
	// PublishWithID should replace it with the new id.
	sseMsg := NewSSEMessage("update", "payload").WithID("old-id").String()
	b.PublishWithID("events", "new-id", sseMsg)

	select {
	case msg := <-ch:
		assert.Contains(t, msg, "event: update")
		assert.Contains(t, msg, "data: payload")
		assert.Contains(t, msg, "id: new-id")
		assert.NotContains(t, msg, "old-id")
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for message")
	}
}

func TestPublishWithID_RegularPublishNoID(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	ch, unsub := b.Subscribe("events")
	defer unsub()

	// Regular Publish should not add any id field.
	b.Publish("events", "plain-message")

	select {
	case msg := <-ch:
		assert.Equal(t, "plain-message", msg)
		assert.NotContains(t, msg, "id:")
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for message")
	}
}

func TestPublishWithID_RoundTrip(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	b.SetReplayPolicy("events", 10)

	// Publish with IDs.
	b.PublishWithID("events", "evt-1", NewSSEMessage("update", "first").String())
	b.PublishWithID("events", "evt-2", NewSSEMessage("update", "second").String())
	b.PublishWithID("events", "evt-3", NewSSEMessage("update", "third").String())

	// Simulate reconnect from evt-1: should replay evt-2 and evt-3 with id fields.
	ch, unsub := b.SubscribeFromID("events", "evt-1")
	defer unsub()

	// Replayed messages should contain id fields (come before reconnected event).
	for _, expected := range []struct {
		data string
		id   string
	}{
		{"second", "evt-2"},
		{"third", "evt-3"},
	} {
		select {
		case msg := <-ch:
			assert.Contains(t, msg, "data: "+expected.data)
			assert.Contains(t, msg, "id: "+expected.id)
		case <-time.After(time.Second):
			t.Fatalf("timed out waiting for replay of %s", expected.id)
		}
	}

	// Reconnected control event with replay stats comes after replay.
	select {
	case msg := <-ch:
		assert.Contains(t, msg, "event: tavern-reconnected")
		assert.Contains(t, msg, `"replayDelivered":2`)
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for reconnected control event")
	}
}
