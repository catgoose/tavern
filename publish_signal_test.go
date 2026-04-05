package tavern

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOnPublishDrop_Fires(t *testing.T) {
	b := NewSSEBroker(WithBufferSize(1))
	defer b.Close()

	var dropTopic string
	var dropCount atomic.Int32
	b.OnPublishDrop(func(topic string, droppedForCount int) {
		dropTopic = topic
		dropCount.Add(int32(droppedForCount))
	})

	ch, unsub := b.Subscribe("events")
	defer unsub()

	// Fill the buffer.
	b.Publish("events", "msg1")
	// This should drop and fire the callback.
	b.Publish("events", "msg2")

	time.Sleep(50 * time.Millisecond)

	assert.Equal(t, "events", dropTopic)
	assert.GreaterOrEqual(t, dropCount.Load(), int32(1))

	// Drain to verify first message arrived.
	select {
	case msg := <-ch:
		assert.Equal(t, "msg1", msg)
	case <-time.After(100 * time.Millisecond):
		t.Fatal("timeout")
	}
}

func TestPublishBlocking_Succeeds(t *testing.T) {
	b := NewSSEBroker(WithBufferSize(1))
	defer b.Close()

	ch, unsub := b.Subscribe("topic")
	defer unsub()

	// Channel has buffer of 1, so first message fits.
	err := b.PublishBlocking("topic", "hello", 100*time.Millisecond)
	require.NoError(t, err)

	select {
	case msg := <-ch:
		assert.Equal(t, "hello", msg)
	case <-time.After(500 * time.Millisecond):
		t.Fatal("timeout")
	}
}

func TestPublishBlocking_TimesOut(t *testing.T) {
	b := NewSSEBroker(WithBufferSize(1))
	defer b.Close()

	_, unsub := b.Subscribe("topic")
	defer unsub()

	// Fill the buffer.
	err := b.PublishBlocking("topic", "msg1", 100*time.Millisecond)
	require.NoError(t, err)

	// Second publish should time out since nobody is draining.
	err = b.PublishBlocking("topic", "msg2", 10*time.Millisecond)
	assert.ErrorIs(t, err, ErrPublishTimeout)
}

func TestPublishBlockingTo_Scoped(t *testing.T) {
	b := NewSSEBroker(WithBufferSize(1))
	defer b.Close()

	ch, unsub := b.SubscribeScoped("topic", "scope1")
	defer unsub()

	err := b.PublishBlockingTo("topic", "scope1", "hello", 100*time.Millisecond)
	require.NoError(t, err)

	select {
	case msg := <-ch:
		assert.Equal(t, "hello", msg)
	case <-time.After(500 * time.Millisecond):
		t.Fatal("timeout")
	}
}

func TestPublishBlockingTo_TimesOut(t *testing.T) {
	b := NewSSEBroker(WithBufferSize(1))
	defer b.Close()

	_, unsub := b.SubscribeScoped("topic", "scope1")
	defer unsub()

	// Fill buffer.
	err := b.PublishBlockingTo("topic", "scope1", "msg1", 100*time.Millisecond)
	require.NoError(t, err)

	err = b.PublishBlockingTo("topic", "scope1", "msg2", 10*time.Millisecond)
	assert.ErrorIs(t, err, ErrPublishTimeout)
}

func TestPublishBlocking_ZeroTimeout_NonBlocking(t *testing.T) {
	b := NewSSEBroker(WithBufferSize(1))
	defer b.Close()

	ch, unsub := b.Subscribe("topic")
	defer unsub()

	// Zero timeout should behave like normal Publish.
	err := b.PublishBlocking("topic", "hello", 0)
	require.NoError(t, err)

	select {
	case msg := <-ch:
		assert.Equal(t, "hello", msg)
	case <-time.After(200 * time.Millisecond):
		t.Fatal("timeout")
	}
}

func TestOnPublishDrop_NotCalledWhenNoDrops(t *testing.T) {
	b := NewSSEBroker(WithBufferSize(10))
	defer b.Close()

	var called atomic.Bool
	b.OnPublishDrop(func(_ string, _ int) {
		called.Store(true)
	})

	ch, unsub := b.Subscribe("topic")
	defer unsub()

	b.Publish("topic", "msg1")

	select {
	case <-ch:
	case <-time.After(100 * time.Millisecond):
	}

	assert.False(t, called.Load(), "callback should not fire when no drops occur")
}
