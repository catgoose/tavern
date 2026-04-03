package tavern

import (
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSubscribePublishUnsubscribe(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	ch, unsub := b.Subscribe("topic1")
	defer unsub()

	b.Publish("topic1", "hello")

	select {
	case msg := <-ch:
		assert.Equal(t, "hello", msg)
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for message")
	}
}

func TestConcurrentSubscribePublish(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	const n = 50
	var wg sync.WaitGroup

	// Subscribe all first, then publish, then unsub — avoids race between publish and unsub.
	channels := make([]<-chan string, n)
	unsubs := make([]func(), n)
	for i := range n {
		channels[i], unsubs[i] = b.Subscribe("concurrent")
	}

	// Publish concurrently.
	for range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			b.Publish("concurrent", "ping")
		}()
	}
	wg.Wait()

	// Each subscriber should have received messages.
	for i := range n {
		select {
		case msg := <-channels[i]:
			assert.Equal(t, "ping", msg)
		case <-time.After(time.Second):
			t.Errorf("subscriber %d timed out", i)
		}
		unsubs[i]()
	}
}

func TestHasSubscribers(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	require.False(t, b.HasSubscribers("t"))

	_, unsub := b.Subscribe("t")
	require.True(t, b.HasSubscribers("t"))

	unsub()
	require.False(t, b.HasSubscribers("t"))
}

func TestCloseWithActiveSubscribers(t *testing.T) {
	b := NewSSEBroker()

	ch1, _ := b.Subscribe("a")
	ch2, _ := b.Subscribe("b")

	// Should not panic.
	b.Close()

	// Channels should be closed (reads return zero value immediately).
	_, ok1 := <-ch1
	_, ok2 := <-ch2
	assert.False(t, ok1)
	assert.False(t, ok2)
}

func TestPublishToFullChannel(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	ch, unsub := b.Subscribe("full")
	defer unsub()

	// Fill the channel buffer (size 10).
	for i := range 10 {
		b.Publish("full", "msg"+string(rune('0'+i)))
	}

	// This should not block — message is dropped.
	done := make(chan struct{})
	go func() {
		b.Publish("full", "overflow")
		close(done)
	}()
	select {
	case <-done:
		// success — did not block
	case <-time.After(time.Second):
		t.Fatal("Publish blocked on full channel")
	}

	// Drain and verify we got the original 10 messages.
	for range 10 {
		<-ch
	}
}

func TestDoubleUnsubSafety(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	_, unsub := b.Subscribe("topic")

	// First unsub is normal.
	unsub()
	// Second unsub should not panic (after bug fix 1a).
	assert.NotPanics(t, func() { unsub() })
}

func TestUnsubAfterClose(t *testing.T) {
	b := NewSSEBroker()

	_, unsub := b.Subscribe("topic")

	b.Close()

	// Unsub after Close should not panic.
	assert.NotPanics(t, func() { unsub() })
}

func TestPublishToNonExistentTopic(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	// Should not panic.
	assert.NotPanics(t, func() { b.Publish("nonexistent", "msg") })
}

func TestNewSSEBroker_DefaultBuffer(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	assert.Equal(t, 10, b.bufferSize)

	ch, unsub := b.Subscribe("t")
	defer unsub()

	// Fill the default buffer of 10 without blocking.
	for i := range 10 {
		b.Publish("t", string(rune('a'+i)))
	}

	// 11th message should be dropped (buffer full).
	b.Publish("t", "overflow")

	received := 0
	for range 10 {
		select {
		case <-ch:
			received++
		case <-time.After(100 * time.Millisecond):
			t.Fatal("timed out draining channel")
		}
	}
	assert.Equal(t, 10, received)

	// Channel should be empty now.
	select {
	case <-ch:
		t.Fatal("unexpected extra message")
	default:
	}
}

func TestNewSSEBroker_CustomBuffer(t *testing.T) {
	b := NewSSEBroker(WithBufferSize(3))
	defer b.Close()

	assert.Equal(t, 3, b.bufferSize)

	ch, unsub := b.Subscribe("t")
	defer unsub()

	// Fill the custom buffer of 3.
	for i := range 3 {
		b.Publish("t", string(rune('a'+i)))
	}

	// 4th message should be dropped.
	b.Publish("t", "overflow")

	received := 0
	for range 3 {
		select {
		case <-ch:
			received++
		case <-time.After(100 * time.Millisecond):
			t.Fatal("timed out draining channel")
		}
	}
	assert.Equal(t, 3, received)

	select {
	case <-ch:
		t.Fatal("unexpected extra message — buffer was larger than configured")
	default:
	}
}

func TestSubscriberCount(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	assert.Equal(t, 0, b.SubscriberCount())

	_, unsub1 := b.Subscribe("a")
	_, unsub2 := b.Subscribe("a")
	_, unsub3 := b.Subscribe("b")

	assert.Equal(t, 3, b.SubscriberCount())

	unsub1()
	assert.Equal(t, 2, b.SubscriberCount())

	unsub2()
	unsub3()
	assert.Equal(t, 0, b.SubscriberCount())
}

func TestPublishDrops(t *testing.T) {
	b := NewSSEBroker(WithBufferSize(1))
	defer b.Close()

	assert.Equal(t, int64(0), b.PublishDrops())

	_, unsub := b.Subscribe("t")
	defer unsub()

	// First publish fills the buffer.
	b.Publish("t", "msg1")
	assert.Equal(t, int64(0), b.PublishDrops())

	// Second publish overflows — should be counted as a drop.
	b.Publish("t", "msg2")
	assert.Equal(t, int64(1), b.PublishDrops())
}

func TestStats(t *testing.T) {
	b := NewSSEBroker(WithBufferSize(1))
	defer b.Close()

	_, unsub1 := b.Subscribe("a")
	_, unsub2 := b.Subscribe("b")
	defer unsub1()
	defer unsub2()

	// Overflow both subscribers.
	b.Publish("a", "m1")
	b.Publish("a", "m2") // drop
	b.Publish("b", "m1")
	b.Publish("b", "m2") // drop

	s := b.Stats()
	assert.Equal(t, 2, s.Topics)
	assert.Equal(t, 2, s.Subscribers)
	assert.Equal(t, int64(2), s.PublishDrops)
}

func TestWithBufferSize_LargeBuffer(t *testing.T) {
	const bufSize = 500
	b := NewSSEBroker(WithBufferSize(bufSize))
	defer b.Close()

	ch, unsub := b.Subscribe("t")
	defer unsub()

	// Publish exactly bufSize messages — none should be dropped.
	for i := range bufSize {
		b.Publish("t", string(rune('0'+(i%10))))
	}

	received := 0
	for range bufSize {
		select {
		case <-ch:
			received++
		case <-time.After(time.Second):
			t.Fatalf("timed out after receiving %d/%d messages", received, bufSize)
		}
	}
	assert.Equal(t, bufSize, received)
}

func TestSubscribeScoped_IsolatesMessages(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	chA, unsubA := b.SubscribeScoped("events", "acct-1")
	defer unsubA()
	chB, unsubB := b.SubscribeScoped("events", "acct-2")
	defer unsubB()

	b.PublishTo("events", "acct-1", "for-acct-1")
	b.PublishTo("events", "acct-2", "for-acct-2")

	select {
	case msg := <-chA:
		assert.Equal(t, "for-acct-1", msg)
	case <-time.After(time.Second):
		t.Fatal("acct-1 timed out")
	}

	select {
	case msg := <-chB:
		assert.Equal(t, "for-acct-2", msg)
	case <-time.After(time.Second):
		t.Fatal("acct-2 timed out")
	}

	// Verify no cross-delivery.
	select {
	case msg := <-chA:
		t.Fatalf("acct-1 received unexpected message: %s", msg)
	default:
	}
	select {
	case msg := <-chB:
		t.Fatalf("acct-2 received unexpected message: %s", msg)
	default:
	}
}

func TestPublishTo_NoMatchingScope(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	ch, unsub := b.SubscribeScoped("events", "acct-1")
	defer unsub()

	// Publish to a scope that has no subscribers.
	b.PublishTo("events", "acct-999", "ghost")

	select {
	case msg := <-ch:
		t.Fatalf("should not have received message: %s", msg)
	default:
		// expected — no delivery
	}
}

func TestPublishOOBTo(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	ch, unsub := b.SubscribeScoped("updates", "sess-1")
	defer unsub()

	b.PublishOOBTo("updates", "sess-1", Replace("status", "<b>online</b>"))

	select {
	case msg := <-ch:
		assert.Contains(t, msg, `hx-swap-oob="outerHTML"`)
		assert.Contains(t, msg, `id="status"`)
		assert.Contains(t, msg, "<b>online</b>")
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for OOB message")
	}
}

func TestSubscribeScoped_UnsubscribeCleanup(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	_, unsub := b.SubscribeScoped("events", "acct-1")

	require.True(t, b.HasSubscribers("events"))
	require.Equal(t, 1, b.SubscriberCount())

	unsub()

	require.False(t, b.HasSubscribers("events"))
	require.Equal(t, 0, b.SubscriberCount())

	// Double-unsub should not panic.
	assert.NotPanics(t, func() { unsub() })
}

func TestPublishTo_DropsWhenChannelFull(t *testing.T) {
	b := NewSSEBroker(WithBufferSize(1))
	defer b.Close()

	ch, unsub := b.SubscribeScoped("events", "user-1")
	defer unsub()

	// Fill the buffer.
	b.PublishTo("events", "user-1", "first")
	assert.Equal(t, int64(0), b.PublishDrops())

	// Overflow — should be dropped.
	b.PublishTo("events", "user-1", "overflow")
	assert.Equal(t, int64(1), b.PublishDrops())

	// Drain the one message that was buffered.
	select {
	case msg := <-ch:
		assert.Equal(t, "first", msg)
	case <-time.After(time.Second):
		t.Fatal("timed out")
	}
}

func TestPublishTo_NonExistentTopic(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	// Should not panic.
	assert.NotPanics(t, func() { b.PublishTo("ghost", "scope-x", "msg") })
}

func TestClose_WithScopedSubscribers(t *testing.T) {
	b := NewSSEBroker()

	ch1, _ := b.SubscribeScoped("events", "acct-1")
	ch2, _ := b.SubscribeScoped("events", "acct-2")

	// Should not panic.
	b.Close()

	// Channels should be closed.
	_, ok1 := <-ch1
	_, ok2 := <-ch2
	assert.False(t, ok1)
	assert.False(t, ok2)
}

func TestClose_EmptyBroker(t *testing.T) {
	b := NewSSEBroker()
	// Close on a broker with no subscribers should not panic.
	assert.NotPanics(t, func() { b.Close() })
}

func TestSubscribeAfterClose(t *testing.T) {
	b := NewSSEBroker()
	b.Close()

	ch, unsub := b.Subscribe("topic")
	defer unsub()

	// Channel must be closed immediately.
	select {
	case _, ok := <-ch:
		assert.False(t, ok, "channel returned from Subscribe after Close must be closed")
	default:
		t.Fatal("channel from Subscribe after Close was not immediately closed")
	}
}

func TestSubscribeScopedAfterClose(t *testing.T) {
	b := NewSSEBroker()
	b.Close()

	ch, unsub := b.SubscribeScoped("topic", "scope")
	defer unsub()

	select {
	case _, ok := <-ch:
		assert.False(t, ok, "channel returned from SubscribeScoped after Close must be closed")
	default:
		t.Fatal("channel from SubscribeScoped after Close was not immediately closed")
	}
}

func TestPublishAfterClose(t *testing.T) {
	b := NewSSEBroker()
	b.Close()

	// Publish after Close should not panic.
	assert.NotPanics(t, func() { b.Publish("topic", "msg") })
	assert.NotPanics(t, func() { b.PublishTo("topic", "scope", "msg") })
}

func TestScopedAndUnscopedCoexist(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	// Unscoped subscriber.
	chGlobal, unsubGlobal := b.Subscribe("events")
	defer unsubGlobal()

	// Scoped subscriber.
	chScoped, unsubScoped := b.SubscribeScoped("events", "acct-1")
	defer unsubScoped()

	// Unscoped publish reaches only unscoped subscribers.
	b.Publish("events", "broadcast")

	select {
	case msg := <-chGlobal:
		assert.Equal(t, "broadcast", msg)
	case <-time.After(time.Second):
		t.Fatal("global subscriber timed out")
	}

	select {
	case msg := <-chScoped:
		t.Fatalf("scoped subscriber should not receive unscoped publish: %s", msg)
	default:
	}

	// Scoped publish reaches only matching scoped subscribers.
	b.PublishTo("events", "acct-1", "scoped-msg")

	select {
	case msg := <-chScoped:
		assert.Equal(t, "scoped-msg", msg)
	case <-time.After(time.Second):
		t.Fatal("scoped subscriber timed out")
	}

	select {
	case msg := <-chGlobal:
		t.Fatalf("global subscriber should not receive scoped publish: %s", msg)
	default:
	}

	// Both count toward HasSubscribers and SubscriberCount.
	assert.True(t, b.HasSubscribers("events"))
	assert.Equal(t, 2, b.SubscriberCount())

	counts := b.TopicCounts()
	assert.Equal(t, 2, counts["events"])

	s := b.Stats()
	assert.Equal(t, 1, s.Topics)
	assert.Equal(t, 2, s.Subscribers)
}

func TestPublishWithReplay_NewSubscriberGetsReplay(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	b.PublishWithReplay("topic", "cached-msg")

	ch, unsub := b.Subscribe("topic")
	defer unsub()

	select {
	case msg := <-ch:
		assert.Equal(t, "cached-msg", msg)
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for replay message")
	}
}

func TestPublishWithReplay_UpdatesCache(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	b.PublishWithReplay("topic", "first")
	b.PublishWithReplay("topic", "second")

	ch, unsub := b.Subscribe("topic")
	defer unsub()

	select {
	case msg := <-ch:
		assert.Equal(t, "second", msg)
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for replay message")
	}

	// Only one replay message should be delivered.
	select {
	case msg := <-ch:
		t.Fatalf("unexpected extra replay message: %s", msg)
	default:
	}
}

func TestPublishWithReplay_ExistingSubscribersGetMessage(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	ch, unsub := b.Subscribe("topic")
	defer unsub()

	b.PublishWithReplay("topic", "live-msg")

	select {
	case msg := <-ch:
		assert.Equal(t, "live-msg", msg)
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for message")
	}
}

func TestPublishWithReplay_NoReplayForRegularPublish(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	b.Publish("topic", "regular-msg")

	ch, unsub := b.Subscribe("topic")
	defer unsub()

	select {
	case msg := <-ch:
		t.Fatalf("should not have received replay for regular publish: %s", msg)
	default:
		// expected — no replay
	}
}

func TestPublishWithReplay_ScopedSubscriberGetsReplay(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	b.PublishWithReplay("events", "cached-event")

	ch, unsub := b.SubscribeScoped("events", "acct-1")
	defer unsub()

	select {
	case msg := <-ch:
		assert.Equal(t, "cached-event", msg)
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for replay message on scoped subscriber")
	}
}

func TestClearReplay(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	b.PublishWithReplay("topic", "cached-msg")
	b.ClearReplay("topic")

	ch, unsub := b.Subscribe("topic")
	defer unsub()

	select {
	case msg := <-ch:
		t.Fatalf("should not have received replay after clear: %s", msg)
	default:
		// expected — no replay
	}
}

func TestPublishWithReplay_AfterClose(t *testing.T) {
	b := NewSSEBroker()
	b.Close()

	assert.NotPanics(t, func() { b.PublishWithReplay("topic", "msg") })
}
