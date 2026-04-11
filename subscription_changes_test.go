package tavern

import (
	"encoding/json"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSubscribeMultiWithMeta_ReceivesFromAllTopics(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	ch, unsub := b.SubscribeMultiWithMeta(SubscribeMeta{ID: "sub1"}, "a", "b")
	defer unsub()

	b.Publish("a", "from-a")
	b.Publish("b", "from-b")

	received := make(map[string]string)
	for range 2 {
		select {
		case msg := <-ch:
			received[msg.Topic] = msg.Data
		case <-time.After(time.Second):
			t.Fatalf("timed out, got %d/2", len(received))
		}
	}

	assert.Equal(t, "from-a", received["a"])
	assert.Equal(t, "from-b", received["b"])
}

func TestSubscribeMultiWithMeta_Unsubscribe(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	ch, unsub := b.SubscribeMultiWithMeta(SubscribeMeta{ID: "sub1"}, "a", "b")
	unsub()

	_, ok := <-ch
	assert.False(t, ok, "channel should be closed after unsubscribe")
}

func TestSubscribeMultiWithMeta_DoubleUnsub(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	_, unsub := b.SubscribeMultiWithMeta(SubscribeMeta{ID: "sub1"}, "a")
	unsub()
	assert.NotPanics(t, func() { unsub() })
}

func TestAddTopic_Basic(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	ch, unsub := b.SubscribeMultiWithMeta(SubscribeMeta{ID: "sub1"}, "a")
	defer unsub()

	ok := b.AddTopic("sub1", "b", false)
	assert.True(t, ok)

	b.Publish("a", "from-a")
	b.Publish("b", "from-b")

	received := make(map[string]string)
	for range 2 {
		select {
		case msg := <-ch:
			received[msg.Topic] = msg.Data
		case <-time.After(time.Second):
			t.Fatalf("timed out, got %d/2", len(received))
		}
	}

	assert.Equal(t, "from-a", received["a"])
	assert.Equal(t, "from-b", received["b"])
}

func TestAddTopic_DuplicateIsNoop(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	_, unsub := b.SubscribeMultiWithMeta(SubscribeMeta{ID: "sub1"}, "a")
	defer unsub()

	ok := b.AddTopic("sub1", "a", false)
	assert.False(t, ok, "adding duplicate topic should return false")
}

func TestAddTopic_UnknownSubscriber(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	ok := b.AddTopic("nonexistent", "a", false)
	assert.False(t, ok)
}

func TestRemoveTopic_Basic(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	ch, unsub := b.SubscribeMultiWithMeta(SubscribeMeta{ID: "sub1"}, "a", "b")
	defer unsub()

	// Drain any initial messages
	ok := b.RemoveTopic("sub1", "b", false)
	assert.True(t, ok)

	b.Publish("a", "from-a")
	b.Publish("b", "from-b-should-not-arrive")

	select {
	case msg := <-ch:
		assert.Equal(t, "a", msg.Topic)
		assert.Equal(t, "from-a", msg.Data)
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for message from topic a")
	}

	// Ensure no message from b arrives
	select {
	case msg := <-ch:
		t.Fatalf("unexpected message: %+v", msg)
	case <-time.After(100 * time.Millisecond):
		// Expected: no message from removed topic
	}
}

func TestRemoveTopic_UnknownTopic(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	_, unsub := b.SubscribeMultiWithMeta(SubscribeMeta{ID: "sub1"}, "a")
	defer unsub()

	ok := b.RemoveTopic("sub1", "nonexistent", false)
	assert.False(t, ok)
}

func TestRemoveTopic_UnknownSubscriber(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	ok := b.RemoveTopic("nonexistent", "a", false)
	assert.False(t, ok)
}

func TestAddTopic_FiresOnFirstSubscriber(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	fired := make(chan string, 1)
	b.OnFirstSubscriber("new-topic", func(topic string) {
		fired <- topic
	})

	_, unsub := b.SubscribeMultiWithMeta(SubscribeMeta{ID: "sub1"}, "a")
	defer unsub()

	b.AddTopic("sub1", "new-topic", false)

	select {
	case topic := <-fired:
		assert.Equal(t, "new-topic", topic)
	case <-time.After(time.Second):
		t.Fatal("OnFirstSubscriber did not fire")
	}
}

func TestRemoveTopic_FiresOnLastUnsubscribe(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	fired := make(chan string, 1)
	b.OnLastUnsubscribe("b", func(topic string) {
		fired <- topic
	})

	_, unsub := b.SubscribeMultiWithMeta(SubscribeMeta{ID: "sub1"}, "a", "b")
	defer unsub()

	b.RemoveTopic("sub1", "b", false)

	select {
	case topic := <-fired:
		assert.Equal(t, "b", topic)
	case <-time.After(time.Second):
		t.Fatal("OnLastUnsubscribe did not fire")
	}
}

func TestAddTopic_ControlEvent(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	ch, unsub := b.SubscribeMultiWithMeta(SubscribeMeta{ID: "sub1"}, "a")
	defer unsub()

	b.AddTopic("sub1", "b", true)

	// Should receive a control event
	select {
	case msg := <-ch:
		assert.Equal(t, topicsChangedEvent, msg.Topic)
		assert.True(t, strings.Contains(msg.Data, "tavern-topics-changed"))
		assert.True(t, strings.Contains(msg.Data, `"action":"added"`))
		assert.True(t, strings.Contains(msg.Data, `"topic":"b"`))
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for control event")
	}
}

func TestRemoveTopic_ControlEvent(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	ch, unsub := b.SubscribeMultiWithMeta(SubscribeMeta{ID: "sub1"}, "a", "b")
	defer unsub()

	b.RemoveTopic("sub1", "b", true)

	select {
	case msg := <-ch:
		assert.Equal(t, topicsChangedEvent, msg.Topic)
		assert.True(t, strings.Contains(msg.Data, "tavern-topics-changed"))
		assert.True(t, strings.Contains(msg.Data, `"action":"removed"`))
		assert.True(t, strings.Contains(msg.Data, `"topic":"b"`))

		// Parse the JSON data portion to verify topics list
		dataIdx := strings.Index(msg.Data, "data: ")
		require.NotEqual(t, -1, dataIdx)
		dataStr := strings.TrimSpace(msg.Data[dataIdx+6:])
		var payload map[string]any
		require.NoError(t, json.Unmarshal([]byte(dataStr), &payload))
		topics := payload["topics"].([]any)
		assert.Len(t, topics, 1)
		assert.Equal(t, "a", topics[0].(string))
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for control event")
	}
}

func TestAddTopicForScope(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	// Create subscriber with a scoped subscription so scope metadata is set.
	ch1, unsub1 := b.SubscribeMultiWithMeta(SubscribeMeta{ID: "sub1"}, "a")
	defer unsub1()

	// Manually set scope on the subscriber's metadata for the scope lookup.
	b.mu.Lock()
	for _, info := range b.subscriberMeta {
		if info.ID == "sub1" {
			info.Scope = "user:123"
		}
	}
	b.mu.Unlock()

	count := b.AddTopicForScope("user:123", "admin:panel", false)
	assert.Equal(t, 1, count)

	b.Publish("admin:panel", "admin-msg")

	select {
	case msg := <-ch1:
		assert.Equal(t, "admin:panel", msg.Topic)
		assert.Equal(t, "admin-msg", msg.Data)
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for message on added topic")
	}
}

func TestRemoveTopicForScope(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	ch, unsub := b.SubscribeMultiWithMeta(SubscribeMeta{ID: "sub1"}, "a", "b")
	defer unsub()

	// Set scope on subscriber metadata.
	b.mu.Lock()
	for _, info := range b.subscriberMeta {
		if info.ID == "sub1" {
			info.Scope = "user:123"
		}
	}
	b.mu.Unlock()

	count := b.RemoveTopicForScope("user:123", "b", false)
	assert.Equal(t, 1, count)

	b.Publish("a", "from-a")
	b.Publish("b", "should-not-arrive")

	select {
	case msg := <-ch:
		assert.Equal(t, "a", msg.Topic)
	case <-time.After(time.Second):
		t.Fatal("timed out")
	}

	select {
	case msg := <-ch:
		t.Fatalf("unexpected message from removed topic: %+v", msg)
	case <-time.After(100 * time.Millisecond):
		// Expected
	}
}

func TestAddTopic_ThreadSafety(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	ch, unsub := b.SubscribeMultiWithMeta(SubscribeMeta{ID: "sub1"}, "base")
	defer unsub()

	var wg sync.WaitGroup
	var addCount atomic.Int32

	// Concurrently add topics and publish messages
	for i := range 20 {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			topic := "topic-" + strings.Repeat("x", idx)
			if b.AddTopic("sub1", topic, false) {
				addCount.Add(1)
			}
		}(i)
	}

	// Concurrently publish to base topic
	for range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			b.Publish("base", "concurrent-msg")
		}()
	}

	wg.Wait()

	assert.Equal(t, int32(20), addCount.Load(), "all 20 topics should have been added")

	// Drain messages
	drained := 0
	for {
		select {
		case <-ch:
			drained++
		case <-time.After(200 * time.Millisecond):
			goto done
		}
	}
done:
	// We should have received at least some messages
	assert.Greater(t, drained, 0)
}

func TestRemoveTopic_ThreadSafety(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	topics := make([]string, 20)
	for i := range topics {
		topics[i] = "topic-" + strings.Repeat("x", i)
	}

	_, unsub := b.SubscribeMultiWithMeta(SubscribeMeta{ID: "sub1"}, topics...)
	defer unsub()

	var wg sync.WaitGroup
	var removeCount atomic.Int32

	for _, topic := range topics {
		wg.Add(1)
		go func(t string) {
			defer wg.Done()
			if b.RemoveTopic("sub1", t, false) {
				removeCount.Add(1)
			}
		}(topic)
	}

	wg.Wait()
	assert.Equal(t, int32(20), removeCount.Load())
}

func TestAddAndRemoveTopic_Concurrent(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	_, unsub := b.SubscribeMultiWithMeta(SubscribeMeta{ID: "sub1"}, "base")
	defer unsub()

	var wg sync.WaitGroup

	// Concurrently add and remove topics
	for i := range 50 {
		wg.Add(2)
		topic := "dynamic-" + strings.Repeat("x", i%10)
		go func() {
			defer wg.Done()
			b.AddTopic("sub1", topic, false)
		}()
		go func() {
			defer wg.Done()
			b.RemoveTopic("sub1", topic, false)
		}()
	}

	wg.Wait()
	// Test passes if no panics or deadlocks
}

func TestSubscribeMultiWithMeta_DeregistersOnUnsub(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	_, unsub := b.SubscribeMultiWithMeta(SubscribeMeta{ID: "sub1"}, "a")
	unsub()

	// After unsubscribe, AddTopic should fail
	ok := b.AddTopic("sub1", "b", false)
	assert.False(t, ok, "should not be able to add topic after unsubscribe")
}

func TestAddTopic_MessagesDeliveredCorrectly(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	ch, unsub := b.SubscribeMultiWithMeta(SubscribeMeta{ID: "sub1"}, "a")
	defer unsub()

	// Add two more topics
	b.AddTopic("sub1", "b", false)
	b.AddTopic("sub1", "c", false)

	// Publish to all three
	b.Publish("a", "msg-a")
	b.Publish("b", "msg-b")
	b.Publish("c", "msg-c")

	received := make(map[string]string)
	for range 3 {
		select {
		case msg := <-ch:
			received[msg.Topic] = msg.Data
		case <-time.After(time.Second):
			t.Fatalf("timed out, got %d/3 messages", len(received))
		}
	}

	assert.Equal(t, "msg-a", received["a"])
	assert.Equal(t, "msg-b", received["b"])
	assert.Equal(t, "msg-c", received["c"])
}

func TestAddTopic_NoControlEventWhenDisabled(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	ch, unsub := b.SubscribeMultiWithMeta(SubscribeMeta{ID: "sub1"}, "a")
	defer unsub()

	b.AddTopic("sub1", "b", false)
	b.Publish("b", "real-msg")

	select {
	case msg := <-ch:
		// Should be the real message, not a control event
		assert.Equal(t, "b", msg.Topic)
		assert.Equal(t, "real-msg", msg.Data)
	case <-time.After(time.Second):
		t.Fatal("timed out")
	}
}

func TestMultipleSubscribers_IndependentTopicChanges(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	ch1, unsub1 := b.SubscribeMultiWithMeta(SubscribeMeta{ID: "sub1"}, "shared")
	defer unsub1()

	ch2, unsub2 := b.SubscribeMultiWithMeta(SubscribeMeta{ID: "sub2"}, "shared")
	defer unsub2()

	// Add topic only to sub1
	b.AddTopic("sub1", "exclusive", false)

	b.Publish("exclusive", "exclusive-msg")

	select {
	case msg := <-ch1:
		assert.Equal(t, "exclusive", msg.Topic)
		assert.Equal(t, "exclusive-msg", msg.Data)
	case <-time.After(time.Second):
		t.Fatal("sub1 did not receive exclusive message")
	}

	// sub2 should NOT receive the exclusive message
	select {
	case msg := <-ch2:
		t.Fatalf("sub2 should not receive exclusive message, got: %+v", msg)
	case <-time.After(100 * time.Millisecond):
		// Expected
	}
}

func TestSubscribeMultiWithMeta_BrokerClose(t *testing.T) {
	b := NewSSEBroker()

	ch, unsub := b.SubscribeMultiWithMeta(SubscribeMeta{ID: "sub1"}, "a", "b")
	defer unsub()

	b.Close()

	// Drain; channel should eventually close
	for range ch {
	}
}

// ---------------------------------------------------------------------------
// SubscriberTopics — query current topic set
// ---------------------------------------------------------------------------

func TestSubscriberTopics_ReflectsCurrentState(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	_, unsub := b.SubscribeMultiWithMeta(SubscribeMeta{ID: "sub1"}, "a", "b")
	defer unsub()

	topics := b.SubscriberTopics("sub1")
	assert.Len(t, topics, 2)
	assert.ElementsMatch(t, []string{"a", "b"}, topics)

	b.AddTopic("sub1", "c", false)
	topics = b.SubscriberTopics("sub1")
	assert.Len(t, topics, 3)
	assert.ElementsMatch(t, []string{"a", "b", "c"}, topics)

	b.RemoveTopic("sub1", "a", false)
	topics = b.SubscriberTopics("sub1")
	assert.Len(t, topics, 2)
	assert.ElementsMatch(t, []string{"b", "c"}, topics)
}

func TestSubscriberTopics_UnknownSubscriber(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	topics := b.SubscriberTopics("nonexistent")
	assert.Nil(t, topics)
}

func TestSubscriberTopics_AfterUnsubscribe(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	_, unsub := b.SubscribeMultiWithMeta(SubscribeMeta{ID: "sub1"}, "a")
	unsub()

	topics := b.SubscriberTopics("sub1")
	assert.Nil(t, topics, "should return nil after unsubscribe deregisters the multi-sub")
}

// ---------------------------------------------------------------------------
// AddTopic does NOT replay historical messages
// ---------------------------------------------------------------------------

func TestAddTopic_NoHistoricalReplay(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	b.SetReplayPolicy("hot-topic", 10)

	// Publish messages BEFORE subscribing via AddTopic.
	for i := 0; i < 5; i++ {
		b.Publish("hot-topic", "pre-existing-"+strings.Repeat("x", i))
	}

	ch, unsub := b.SubscribeMultiWithMeta(SubscribeMeta{ID: "sub1"}, "control")
	defer unsub()

	b.AddTopic("sub1", "hot-topic", false)

	// Publish a new message after AddTopic.
	b.Publish("hot-topic", "after-add")

	msg := <-ch
	assert.Equal(t, "hot-topic", msg.Topic)
	assert.Equal(t, "after-add", msg.Data, "should only receive messages published after AddTopic, not historical replay")

	// No more messages should arrive.
	select {
	case extra := <-ch:
		t.Fatalf("unexpected extra message: %+v", extra)
	case <-time.After(100 * time.Millisecond):
		// Expected
	}
}

// ---------------------------------------------------------------------------
// RemoveTopic while messages are in-flight
// ---------------------------------------------------------------------------

func TestRemoveTopic_InFlightMessages(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	ch, unsub := b.SubscribeMultiWithMeta(SubscribeMeta{ID: "sub1"}, "keep", "remove-me")
	defer unsub()

	// Rapidly publish to the topic we're about to remove.
	for i := 0; i < 10; i++ {
		b.Publish("remove-me", "inflight-msg")
	}

	// Remove topic while messages may still be in the fan-in pipeline.
	ok := b.RemoveTopic("sub1", "remove-me", false)
	assert.True(t, ok)

	// Publish to the kept topic to verify the subscriber is still alive.
	b.Publish("keep", "still-alive")

	// Drain all messages and verify we eventually get our "keep" message.
	deadline := time.After(2 * time.Second)
	gotKeep := false
	for !gotKeep {
		select {
		case msg := <-ch:
			if msg.Topic == "keep" && msg.Data == "still-alive" {
				gotKeep = true
			}
			// Ignore any in-flight "remove-me" messages that arrived before cleanup.
		case <-deadline:
			t.Fatal("timed out waiting for 'keep' topic message after RemoveTopic")
		}
	}
}

// ---------------------------------------------------------------------------
// Unsubscribe while AddTopic goroutines may still be running
// ---------------------------------------------------------------------------

func TestUnsubscribe_WhileAddTopicInProgress(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	_, unsub := b.SubscribeMultiWithMeta(SubscribeMeta{ID: "sub1"}, "base")

	var wg sync.WaitGroup

	// Spin up concurrent AddTopic calls.
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			b.AddTopic("sub1", "dyn-"+strings.Repeat("x", idx), false)
		}(i)
	}

	// Immediately unsubscribe while AddTopic goroutines are likely in-flight.
	unsub()

	wg.Wait()
	// Test passes if no panic, deadlock, or goroutine leak.
}

// ---------------------------------------------------------------------------
// AddTopic/RemoveTopic after the subscriber has been unsubscribed
// ---------------------------------------------------------------------------

func TestAddTopic_AfterUnsubscribe(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	_, unsub := b.SubscribeMultiWithMeta(SubscribeMeta{ID: "sub1"}, "a")
	unsub()

	ok := b.AddTopic("sub1", "b", false)
	assert.False(t, ok, "AddTopic on unsubscribed subscriber should return false")
}

func TestRemoveTopic_AfterUnsubscribe(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	_, unsub := b.SubscribeMultiWithMeta(SubscribeMeta{ID: "sub1"}, "a")
	unsub()

	ok := b.RemoveTopic("sub1", "a", false)
	assert.False(t, ok, "RemoveTopic on unsubscribed subscriber should return false")
}

// ---------------------------------------------------------------------------
// AddTopicForScope / RemoveTopicForScope during concurrent reconnects
// ---------------------------------------------------------------------------

func TestScopeOps_DuringConcurrentActivity(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	// Create multiple subscribers with the same scope.
	var unsubs []func()
	for i := 0; i < 5; i++ {
		_, unsub := b.SubscribeMultiWithMeta(SubscribeMeta{ID: "sub-" + strings.Repeat("x", i)}, "base")
		b.mu.Lock()
		for _, info := range b.subscriberMeta {
			if info.ID == "sub-"+strings.Repeat("x", i) {
				info.Scope = "team:42"
			}
		}
		b.mu.Unlock()
		unsubs = append(unsubs, unsub)
	}
	defer func() {
		for _, u := range unsubs {
			u()
		}
	}()

	var wg sync.WaitGroup

	// Concurrently add and remove topics for the scope while publishing.
	for i := 0; i < 10; i++ {
		wg.Add(3)
		topic := "scope-topic-" + strings.Repeat("x", i%3)
		go func() {
			defer wg.Done()
			b.AddTopicForScope("team:42", topic, false)
		}()
		go func() {
			defer wg.Done()
			b.RemoveTopicForScope("team:42", topic, false)
		}()
		go func() {
			defer wg.Done()
			b.Publish("base", "ping")
		}()
	}

	wg.Wait()
	// Test passes if no panics or deadlocks.
}
