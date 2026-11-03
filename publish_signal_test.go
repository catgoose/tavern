package tavern

import (
	"sync"
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

func TestPublishBlocking_ReleasedByUnsubscribe(t *testing.T) {
	b := NewSSEBroker(WithBufferSize(1))
	defer b.Close()

	ch, unsub := b.Subscribe("t")
	b.Publish("t", "fill")

	result := make(chan error, 1)
	go func() { result <- b.PublishBlocking("t", "waiting", time.Minute) }()
	waitForBlockedSender(t, b, "t", ch)

	// Ordinary publishing is not held up by the blocked publisher.
	b.Publish("t", "nonblocking")
	assert.Equal(t, int64(1), b.PublishDrops())

	unsub()
	select {
	case err := <-result:
		assert.NoError(t, err, "a closed subscriber is not a timeout")
	case <-time.After(time.Second):
		t.Fatal("unsubscribe did not release the blocked publisher")
	}
	_, ok := <-ch
	assert.True(t, ok, "buffered message stays readable")
	_, ok = <-ch
	assert.False(t, ok)
}

func TestPublishBlocking_ReleasedByClose(t *testing.T) {
	b := NewSSEBroker(WithBufferSize(1))

	ch, unsub := b.Subscribe("t")
	defer unsub()
	b.Publish("t", "fill")

	result := make(chan error, 1)
	go func() { result <- b.PublishBlocking("t", "waiting", time.Minute) }()
	waitForBlockedSender(t, b, "t", ch)

	b.Close()
	select {
	case err := <-result:
		assert.NoError(t, err, "a closed subscriber is not a timeout")
	case <-time.After(time.Second):
		t.Fatal("Close did not release the blocked publisher")
	}
}

func TestPublishBlocking_ConcurrentLossEvictionNoRace(t *testing.T) {
	for range 20 {
		b := NewSSEBroker(WithBufferSize(1))
		_, unsubLoss := b.SubscribeWith("t", SubWithDisconnectOnLoss())
		ordinary, unsubOrdinary := b.Subscribe("t")
		b.Publish("t", "fill")

		var wg sync.WaitGroup
		for range 10 {
			wg.Go(func() { _ = b.PublishBlocking("t", "lost", time.Millisecond) })
			wg.Go(func() { b.Publish("t", "nonblocking") })
		}
		wg.Go(func() {
			for range ordinary {
			}
		})
		wg.Go(unsubOrdinary)
		wg.Go(unsubLoss)
		wg.Go(b.Close)
		wg.Wait()
	}
}

// waitForBlockedSender waits until a PublishBlocking call is parked on the
// subscriber channel behind ro.
func waitForBlockedSender(t *testing.T, b *SSEBroker, topic string, ro <-chan string) {
	t.Helper()
	ch := b.readToBidir(topic, ro)
	require.NotNil(t, ch)
	v, ok := b.chanGuards.Load(ch)
	require.True(t, ok)
	g := v.(*chanGuard)
	require.Eventually(t, func() bool {
		g.mu.Lock()
		defer g.mu.Unlock()
		return g.closedSig != nil
	}, time.Second, time.Millisecond)
}
