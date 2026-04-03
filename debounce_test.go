package tavern

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPublishDebounced_SingleCall(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	ch, unsub := b.Subscribe("topic")
	defer unsub()

	b.PublishDebounced("topic", "hello", 50*time.Millisecond)

	select {
	case msg := <-ch:
		assert.Equal(t, "hello", msg)
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for debounced message")
	}
}

func TestPublishDebounced_ResetsTimer(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	ch, unsub := b.Subscribe("topic")
	defer unsub()

	b.PublishDebounced("topic", "first", 50*time.Millisecond)
	time.Sleep(10 * time.Millisecond)
	b.PublishDebounced("topic", "second", 50*time.Millisecond)
	time.Sleep(10 * time.Millisecond)
	b.PublishDebounced("topic", "third", 50*time.Millisecond)

	select {
	case msg := <-ch:
		assert.Equal(t, "third", msg)
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for debounced message")
	}

	// Only one message should arrive.
	select {
	case msg := <-ch:
		t.Fatalf("unexpected extra message: %s", msg)
	case <-time.After(100 * time.Millisecond):
		// expected
	}
}

func TestPublishDebounced_IndependentTopics(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	ch1, unsub1 := b.Subscribe("topic-a")
	defer unsub1()
	ch2, unsub2 := b.Subscribe("topic-b")
	defer unsub2()

	b.PublishDebounced("topic-a", "msg-a", 50*time.Millisecond)
	b.PublishDebounced("topic-b", "msg-b", 50*time.Millisecond)

	got := make(map[string]string)
	for i := 0; i < 2; i++ {
		select {
		case msg := <-ch1:
			got["a"] = msg
		case msg := <-ch2:
			got["b"] = msg
		case <-time.After(time.Second):
			t.Fatal("timed out waiting for debounced messages")
		}
	}

	assert.Equal(t, "msg-a", got["a"])
	assert.Equal(t, "msg-b", got["b"])
}

func TestPublishDebounced_CloseStopsTimers(t *testing.T) {
	b := NewSSEBroker()

	ch, _ := b.Subscribe("topic")

	b.PublishDebounced("topic", "should-not-arrive", 5*time.Second)
	b.Close()

	// Wait a bit and verify nothing arrives (channel is closed by Close).
	select {
	case msg, ok := <-ch:
		if ok {
			t.Fatalf("unexpected message after close: %s", msg)
		}
		// channel closed — expected
	case <-time.After(100 * time.Millisecond):
		// also fine — timer was stopped
	}

	require.Empty(t, b.debounce.timers)
}
