package tavern

import (
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
loop:
	for range 2 {
		select {
		case topic := <-fired:
			topics = append(topics, topic)
		case <-time.After(time.Second):
			break loop
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

func TestSubscribeMultiFromID_ReplaysAcrossTopics(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	b.SetReplayPolicy("t1", 10)
	b.SetReplayPolicy("t2", 10)

	b.PublishWithID("t1", "a1", "msg-a1")
	b.PublishWithID("t1", "a2", "msg-a2")
	b.PublishWithID("t1", "a3", "msg-a3")
	b.PublishWithID("t2", "b1", "msg-b1")
	b.PublishWithID("t2", "b2", "msg-b2")
	b.PublishWithID("t2", "b3", "msg-b3")

	ch, unsub := b.SubscribeMultiFromID([]string{"t1", "t2"}, "a1")
	defer unsub()

	// Expected messages:
	//   t1: tavern-reconnected, msg-a2, msg-a3 (resumes after a1)
	//   t2: tavern-reconnected (a1 not found -> gap; default silent strategy,
	//       so no gap control event and no replay).
	// Total: 4 messages across both topics.
	t1Payloads := make([]string, 0, 2)
	t1Reconnects := 0
	t2Reconnects := 0
	deadline := time.After(time.Second)
	for range 4 {
		select {
		case tm := <-ch:
			switch tm.Topic {
			case "t1":
				if containsReconnected(tm.Data) {
					t1Reconnects++
				} else {
					t1Payloads = append(t1Payloads, tm.Data)
				}
			case "t2":
				if containsReconnected(tm.Data) {
					t2Reconnects++
				} else {
					t.Fatalf("unexpected t2 payload (a1 should be unknown to t2): %s", tm.Data)
				}
			default:
				t.Fatalf("unexpected topic: %s", tm.Topic)
			}
		case <-deadline:
			t.Fatalf("timed out; got t1 payloads=%d t1 reconnects=%d t2 reconnects=%d",
				len(t1Payloads), t1Reconnects, t2Reconnects)
		}
	}

	assert.Equal(t, 1, t1Reconnects, "t1 should emit one reconnected control event")
	assert.Equal(t, 1, t2Reconnects, "t2 should emit one reconnected control event")
	require.Len(t, t1Payloads, 2, "t1 should replay msg-a2 and msg-a3")
	assert.Contains(t, t1Payloads[0], "msg-a2")
	assert.Contains(t, t1Payloads[0], "id: a2")
	assert.Contains(t, t1Payloads[1], "msg-a3")
	assert.Contains(t, t1Payloads[1], "id: a3")

	// No further messages should arrive.
	select {
	case tm := <-ch:
		t.Fatalf("unexpected extra message: topic=%s data=%s", tm.Topic, tm.Data)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestSubscribeMultiFromID_EmptyIDReplaysAllCached(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	b.SetReplayPolicy("t1", 10)
	b.SetReplayPolicy("t2", 10)
	for i := 1; i <= 3; i++ {
		b.PublishWithID("t1", fmt.Sprintf("a%d", i), fmt.Sprintf("msg-a%d", i))
		b.PublishWithID("t2", fmt.Sprintf("b%d", i), fmt.Sprintf("msg-b%d", i))
	}

	ch, unsub := b.SubscribeMultiFromID([]string{"t1", "t2"}, "")
	defer unsub()

	t1Got := make([]string, 0, 3)
	t2Got := make([]string, 0, 3)
	deadline := time.After(time.Second)
	for range 6 {
		select {
		case tm := <-ch:
			switch tm.Topic {
			case "t1":
				t1Got = append(t1Got, tm.Data)
			case "t2":
				t2Got = append(t2Got, tm.Data)
			default:
				t.Fatalf("unexpected topic: %s", tm.Topic)
			}
		case <-deadline:
			t.Fatalf("timed out; got t1=%d t2=%d", len(t1Got), len(t2Got))
		}
	}

	require.Len(t, t1Got, 3)
	require.Len(t, t2Got, 3)
	for i, msg := range t1Got {
		assert.Contains(t, msg, fmt.Sprintf("msg-a%d", i+1))
		assert.Contains(t, msg, fmt.Sprintf("id: a%d", i+1))
	}
	for i, msg := range t2Got {
		assert.Contains(t, msg, fmt.Sprintf("msg-b%d", i+1))
		assert.Contains(t, msg, fmt.Sprintf("id: b%d", i+1))
	}
}

func TestSubscribeMultiFromID_LiveContinuation(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	ch, unsub := b.SubscribeMultiFromID([]string{"t1", "t2"}, "")
	defer unsub()

	b.PublishWithID("t1", "a1", "live-a1")
	b.PublishWithID("t2", "b1", "live-b1")
	b.PublishWithID("t1", "a2", "live-a2")

	got := make(map[string][]string)
	deadline := time.After(time.Second)
	for range 3 {
		select {
		case tm := <-ch:
			got[tm.Topic] = append(got[tm.Topic], tm.Data)
		case <-deadline:
			t.Fatalf("timed out; got t1=%d t2=%d", len(got["t1"]), len(got["t2"]))
		}
	}

	require.Len(t, got["t1"], 2)
	require.Len(t, got["t2"], 1)
	assert.Contains(t, got["t1"][0], "live-a1")
	assert.Contains(t, got["t1"][0], "id: a1")
	assert.Contains(t, got["t1"][1], "live-a2")
	assert.Contains(t, got["t1"][1], "id: a2")
	assert.Contains(t, got["t2"][0], "live-b1")
	assert.Contains(t, got["t2"][0], "id: b1")
}

func TestSubscribeMultiFromID_UnsubscribeClosesAll(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	ch, unsub := b.SubscribeMultiFromID([]string{"t1", "t2"}, "")

	b.PublishWithID("t1", "a1", "msg-a1")
	b.PublishWithID("t2", "b1", "msg-b1")

	// Drain any delivered messages briefly before unsubscribing.
drain:
	for {
		select {
		case _, ok := <-ch:
			if !ok {
				break drain
			}
		case <-time.After(50 * time.Millisecond):
			break drain
		}
	}

	unsub()

	// After unsubscribe, channel must be drained and closed within a bounded
	// timeout.
	closed := make(chan struct{})
	go func() {
		for range ch {
		}
		close(closed)
	}()
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("channel did not close after unsubscribe")
	}

	// Both topics should have no subscribers now.
	assert.False(t, b.HasSubscribers("t1"), "t1 should have no subscribers")
	assert.False(t, b.HasSubscribers("t2"), "t2 should have no subscribers")

	// Double unsubscribe is safe.
	assert.NotPanics(t, func() { unsub() })
}

func TestSubscribeMultiFromID_ReconnectControlEventsPerTopic(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	b.SetReplayPolicy("t1", 10)
	b.SetReplayPolicy("t2", 10)
	b.PublishWithID("t1", "a1", "msg-a1")
	b.PublishWithID("t2", "b1", "msg-b1")

	ch, unsub := b.SubscribeMultiFromID([]string{"t1", "t2"}, "some-id")
	defer unsub()

	reconnects := make(map[string]int)
	deadline := time.After(time.Second)
	for range 2 {
		select {
		case tm := <-ch:
			if containsReconnected(tm.Data) {
				reconnects[tm.Topic]++
			} else {
				t.Fatalf("unexpected payload on topic %s: %s", tm.Topic, tm.Data)
			}
		case <-deadline:
			t.Fatalf("timed out; got reconnects=%v", reconnects)
		}
	}
	assert.Equal(t, 1, reconnects["t1"])
	assert.Equal(t, 1, reconnects["t2"])
}

func TestSubscribeMultiFromID_EmptyTopicList(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	for _, topics := range [][]string{nil, {}} {
		ch, unsub := b.SubscribeMultiFromID(topics, "")
		require.NotNil(t, ch)
		require.NotNil(t, unsub)

		// No messages should ever arrive.
		select {
		case tm, ok := <-ch:
			if ok {
				t.Fatalf("unexpected message on empty-topic subscription: %+v", tm)
			}
		case <-time.After(25 * time.Millisecond):
		}

		// Unsubscribe must close the channel and must not panic.
		assert.NotPanics(t, unsub)
		closed := make(chan struct{})
		go func() {
			for range ch {
			}
			close(closed)
		}()
		select {
		case <-closed:
		case <-time.After(time.Second):
			t.Fatal("channel did not close after unsubscribe on empty topic list")
		}
	}
}

// containsReconnected reports whether msg is a tavern-reconnected control
// event emitted by SubscribeFromID-style subscriptions.
func containsReconnected(msg string) bool {
	return strings.Contains(msg, "event: tavern-reconnected")
}
