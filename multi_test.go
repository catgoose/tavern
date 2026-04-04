package tavern

import (
	"sort"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestSubscribeMulti_ReceivesFromAllTopics(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	ch, unsub := b.SubscribeMulti("a", "b", "c")
	defer unsub()

	b.Publish("a", "from-a")
	b.Publish("b", "from-b")
	b.Publish("c", "from-c")

	received := make(map[string]string)
	for range 3 {
		select {
		case msg := <-ch:
			received[msg.Topic] = msg.Data
		case <-time.After(time.Second):
			t.Fatalf("timed out, got %d/3", len(received))
		}
	}

	assert.Equal(t, "from-a", received["a"])
	assert.Equal(t, "from-b", received["b"])
	assert.Equal(t, "from-c", received["c"])
}

func TestSubscribeMulti_UnsubscribeStopsAll(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	ch, unsub := b.SubscribeMulti("a", "b")
	unsub()

	// Channel should be closed after unsub
	_, ok := <-ch
	assert.False(t, ok, "channel should be closed after unsubscribe")
}

func TestSubscribeMulti_CountsPerTopic(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	_, unsub := b.SubscribeMulti("a", "b")
	defer unsub()

	assert.True(t, b.HasSubscribers("a"))
	assert.True(t, b.HasSubscribers("b"))
	assert.Equal(t, 2, b.SubscriberCount())
}

func TestSubscribeMulti_LifecycleHooks(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	fired := make(chan string, 2)
	b.OnFirstSubscriber("x", func(topic string) { fired <- topic })
	b.OnFirstSubscriber("y", func(topic string) { fired <- topic })

	_, unsub := b.SubscribeMulti("x", "y")
	defer unsub()

	topics := make([]string, 0, 2)
	for range 2 {
		select {
		case topic := <-fired:
			topics = append(topics, topic)
		case <-time.After(time.Second):
			break
		}
	}
	sort.Strings(topics)
	assert.Equal(t, []string{"x", "y"}, topics)
}

func TestSubscribeMulti_BrokerClose(t *testing.T) {
	b := NewSSEBroker()

	ch, unsub := b.SubscribeMulti("a", "b")
	defer unsub()

	b.Close()

	// Drain any remaining messages, channel should eventually close
	for range ch {
	}
	// If we get here, channel was closed — test passes
}

func TestSubscribeMulti_DoubleUnsub(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	_, unsub := b.SubscribeMulti("a")
	unsub()
	assert.NotPanics(t, func() { unsub() })
}

func TestSubscribeMulti_SingleTopic(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	ch, unsub := b.SubscribeMulti("only")
	defer unsub()

	b.Publish("only", "hello")

	select {
	case msg := <-ch:
		assert.Equal(t, "only", msg.Topic)
		assert.Equal(t, "hello", msg.Data)
	case <-time.After(time.Second):
		t.Fatal("timed out")
	}
}
