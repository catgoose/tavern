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
