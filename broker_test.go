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
