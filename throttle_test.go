package tavern

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPublishThrottled_FirstCallImmediate(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	ch, unsub := b.Subscribe("topic")
	defer unsub()

	b.PublishThrottled("topic", "immediate", time.Second)

	select {
	case msg := <-ch:
		assert.Equal(t, "immediate", msg)
	case <-time.After(100 * time.Millisecond):
		t.Fatal("first throttled call should publish immediately")
	}
}

func TestPublishThrottled_RateLimits(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	ch, unsub := b.Subscribe("topic")
	defer unsub()

	interval := 100 * time.Millisecond

	// First call — immediate.
	b.PublishThrottled("topic", "msg-1", interval)
	select {
	case msg := <-ch:
		assert.Equal(t, "msg-1", msg)
	case <-time.After(time.Second):
		t.Fatal("timed out on first message")
	}

	// Rapid calls within interval.
	for i := 2; i <= 5; i++ {
		b.PublishThrottled("topic", "msg-rapid", interval)
	}

	// Should get exactly one trailing message after interval.
	select {
	case msg := <-ch:
		assert.Equal(t, "msg-rapid", msg)
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for trailing throttled message")
	}

	// No more messages.
	select {
	case msg := <-ch:
		t.Fatalf("unexpected extra message: %s", msg)
	case <-time.After(150 * time.Millisecond):
		// expected
	}
}

func TestPublishThrottled_LastMessageWins(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	ch, unsub := b.Subscribe("topic")
	defer unsub()

	interval := 100 * time.Millisecond

	// First call — immediate.
	b.PublishThrottled("topic", "first", interval)
	<-ch

	// Rapid calls with different messages.
	b.PublishThrottled("topic", "second", interval)
	b.PublishThrottled("topic", "third", interval)
	b.PublishThrottled("topic", "fourth", interval)

	select {
	case msg := <-ch:
		assert.Equal(t, "fourth", msg, "trailing publish should use the latest message")
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for trailing throttled message")
	}
}

func TestPublishThrottled_IndependentTopics(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	ch1, unsub1 := b.Subscribe("topic-a")
	defer unsub1()
	ch2, unsub2 := b.Subscribe("topic-b")
	defer unsub2()

	interval := 100 * time.Millisecond

	b.PublishThrottled("topic-a", "a-1", interval)
	b.PublishThrottled("topic-b", "b-1", interval)

	select {
	case msg := <-ch1:
		assert.Equal(t, "a-1", msg)
	case <-time.After(time.Second):
		t.Fatal("timed out on topic-a")
	}

	select {
	case msg := <-ch2:
		assert.Equal(t, "b-1", msg)
	case <-time.After(time.Second):
		t.Fatal("timed out on topic-b")
	}
}

func TestPublishThrottled_CloseStopsTimers(t *testing.T) {
	b := NewSSEBroker()

	ch, _ := b.Subscribe("topic")

	// First call immediate.
	b.PublishThrottled("topic", "first", 5*time.Second)
	<-ch

	// Second call within interval — creates pending timer.
	b.PublishThrottled("topic", "should-not-arrive", 5*time.Second)

	b.Close()

	// Verify nothing arrives.
	select {
	case msg, ok := <-ch:
		if ok {
			t.Fatalf("unexpected message after close: %s", msg)
		}
	case <-time.After(100 * time.Millisecond):
	}

	require.Empty(t, b.throttle.state)
}
