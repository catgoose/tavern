package tavern

import (
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func recv(ch <-chan string, timeout time.Duration) (string, bool) {
	select {
	case msg := <-ch:
		return msg, true
	case <-time.After(timeout):
		return "", false
	}
}

func TestBatchPublish(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	ch, unsub := b.Subscribe("t")
	defer unsub()

	batch := b.Batch()
	batch.Publish("t", "one")
	batch.Publish("t", "two")
	batch.Flush()

	msg, ok := recv(ch, time.Second)
	require.True(t, ok, "expected a message")
	assert.Contains(t, msg, "one")
	assert.Contains(t, msg, "two")

	// Only one channel send should have happened.
	select {
	case <-ch:
		t.Fatal("expected exactly one channel send, got a second")
	case <-time.After(50 * time.Millisecond):
	}
}

func TestBatchPublishOOB(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	ch, unsub := b.Subscribe("t")
	defer unsub()

	batch := b.Batch()
	batch.PublishOOB("t", Replace("el-1", "<p>a</p>"))
	batch.PublishOOB("t", Replace("el-2", "<p>b</p>"))
	batch.Flush()

	msg, ok := recv(ch, time.Second)
	require.True(t, ok)
	assert.Contains(t, msg, `id="el-1"`)
	assert.Contains(t, msg, `id="el-2"`)

	select {
	case <-ch:
		t.Fatal("expected exactly one send")
	case <-time.After(50 * time.Millisecond):
	}
}

func TestBatchPublishToScoped(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	ch1, unsub1 := b.SubscribeScoped("t", "user:1")
	defer unsub1()
	ch2, unsub2 := b.SubscribeScoped("t", "user:2")
	defer unsub2()

	batch := b.Batch()
	batch.PublishTo("t", "user:1", "for-user-1")
	batch.PublishTo("t", "user:2", "for-user-2")
	batch.Flush()

	msg1, ok := recv(ch1, time.Second)
	require.True(t, ok)
	assert.Equal(t, "for-user-1", msg1)

	msg2, ok := recv(ch2, time.Second)
	require.True(t, ok)
	assert.Equal(t, "for-user-2", msg2)
}

func TestBatchPublishOOBToScoped(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	ch, unsub := b.SubscribeScoped("t", "admin")
	defer unsub()

	batch := b.Batch()
	batch.PublishOOBTo("t", "admin", Replace("stats", "<b>42</b>"))
	batch.PublishOOBTo("t", "admin", Append("log", "<li>event</li>"))
	batch.Flush()

	msg, ok := recv(ch, time.Second)
	require.True(t, ok)
	assert.Contains(t, msg, `id="stats"`)
	assert.Contains(t, msg, `id="log"`)
}

func TestBatchDiscard(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	ch, unsub := b.Subscribe("t")
	defer unsub()

	batch := b.Batch()
	batch.Publish("t", "should-not-arrive")
	batch.Discard()
	batch.Flush() // flush after discard should be no-op

	select {
	case msg := <-ch:
		t.Fatalf("expected no message, got %q", msg)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestBatchEmptyFlush(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	// Flush with no buffered ops should not panic.
	batch := b.Batch()
	batch.Flush()
}

func TestBatchNoSubscribers(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	// Publishing to a topic with no subscribers should not panic.
	batch := b.Batch()
	batch.Publish("ghost", "hello")
	batch.Flush()
}

func TestBatchMixedTopics(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	chA, unsubA := b.Subscribe("topicA")
	defer unsubA()
	chB, unsubB := b.Subscribe("topicB")
	defer unsubB()

	batch := b.Batch()
	batch.Publish("topicA", "a1")
	batch.Publish("topicB", "b1")
	batch.Publish("topicA", "a2")
	batch.Flush()

	msgA, ok := recv(chA, time.Second)
	require.True(t, ok)
	assert.Contains(t, msgA, "a1")
	assert.Contains(t, msgA, "a2")
	assert.NotContains(t, msgA, "b1")

	msgB, ok := recv(chB, time.Second)
	require.True(t, ok)
	assert.Equal(t, "b1", msgB)
}

func TestBatchConcurrentPublish(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	ch, unsub := b.Subscribe("t")
	defer unsub()

	batch := b.Batch()
	var wg sync.WaitGroup
	for i := range 10 {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			batch.Publish("t", strings.Repeat("x", n+1))
		}(i)
	}
	wg.Wait()
	batch.Flush()

	msg, ok := recv(ch, time.Second)
	require.True(t, ok)
	// All 10 publishes should be concatenated into one send.
	// Total length = 1+2+3+...+10 = 55
	assert.Len(t, msg, 55)

	select {
	case <-ch:
		t.Fatal("expected exactly one send")
	case <-time.After(50 * time.Millisecond):
	}
}

func TestBatchMixedScopedAndUnscoped(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	// Unscoped subscriber gets Publish but not PublishTo.
	chAll, unsubAll := b.Subscribe("t")
	defer unsubAll()

	// Scoped subscriber gets PublishTo but not Publish.
	chScoped, unsubScoped := b.SubscribeScoped("t", "s1")
	defer unsubScoped()

	batch := b.Batch()
	batch.Publish("t", "broadcast")
	batch.PublishTo("t", "s1", "scoped-msg")
	batch.Flush()

	msgAll, ok := recv(chAll, time.Second)
	require.True(t, ok)
	assert.Equal(t, "broadcast", msgAll)

	msgScoped, ok := recv(chScoped, time.Second)
	require.True(t, ok)
	assert.Equal(t, "scoped-msg", msgScoped)
}

func TestBatchMultipleSubscribersSameTopic(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	const n = 5
	channels := make([]<-chan string, n)
	unsubs := make([]func(), n)
	for i := range n {
		channels[i], unsubs[i] = b.Subscribe("t")
	}
	t.Cleanup(func() {
		for i := range n {
			unsubs[i]()
		}
	})

	batch := b.Batch()
	batch.Publish("t", "hello")
	batch.Publish("t", " world")
	batch.Flush()

	for i := range n {
		msg, ok := recv(channels[i], time.Second)
		require.True(t, ok, "subscriber %d should receive message", i)
		assert.Equal(t, "hello world", msg)
	}
}

func TestBatchScopedIsolation(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	ch1, unsub1 := b.SubscribeScoped("t", "a")
	defer unsub1()
	ch2, unsub2 := b.SubscribeScoped("t", "b")
	defer unsub2()

	batch := b.Batch()
	batch.PublishTo("t", "a", "only-a")
	batch.Flush()

	msg, ok := recv(ch1, time.Second)
	require.True(t, ok)
	assert.Equal(t, "only-a", msg)

	select {
	case msg := <-ch2:
		t.Fatalf("scope b should not receive message, got %q", msg)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestBatchOOBConcatenation(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	ch, unsub := b.Subscribe("t")
	defer unsub()

	batch := b.Batch()
	batch.PublishOOB("t", Replace("a", "<p>1</p>"))
	batch.PublishOOB("t", Delete("b"))
	batch.PublishOOB("t", Append("c", "<li>new</li>"))
	batch.Flush()

	msg, ok := recv(ch, time.Second)
	require.True(t, ok)

	// All three fragments in a single message.
	assert.Contains(t, msg, `id="a"`)
	assert.Contains(t, msg, `id="b"`)
	assert.Contains(t, msg, `id="c"`)
	assert.Contains(t, msg, `hx-swap-oob="delete"`)
	assert.Contains(t, msg, `hx-swap-oob="outerHTML"`)
	assert.Contains(t, msg, `hx-swap-oob="beforeend"`)
}

func TestBatchFlushOrder(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	ch, unsub := b.Subscribe("t")
	defer unsub()

	batch := b.Batch()
	batch.Publish("t", "A")
	batch.Publish("t", "B")
	batch.Publish("t", "C")
	batch.Flush()

	msg, ok := recv(ch, time.Second)
	require.True(t, ok)
	// Messages are concatenated in order.
	assert.Equal(t, "ABC", msg)
}

func TestBatchScopedMultipleOpsToSameScope(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	ch, unsub := b.SubscribeScoped("t", "u1")
	defer unsub()

	batch := b.Batch()
	batch.PublishTo("t", "u1", "first")
	batch.PublishTo("t", "u1", "second")
	batch.Flush()

	msg, ok := recv(ch, time.Second)
	require.True(t, ok)
	assert.Equal(t, "firstsecond", msg)

	select {
	case <-ch:
		t.Fatal("expected exactly one send")
	case <-time.After(50 * time.Millisecond):
	}
}

func TestBatchDropCountsAsSingle(t *testing.T) {
	// Use a tiny buffer so we can test the drop path.
	b := NewSSEBroker(WithBufferSize(1))
	defer b.Close()

	ch, unsub := b.Subscribe("t")
	defer unsub()

	// Fill the buffer so the batch flush will drop.
	b.Publish("t", "fill")

	batch := b.Batch()
	batch.Publish("t", "will-drop")
	batch.Flush()

	// Drain the original fill message.
	msg, ok := recv(ch, time.Second)
	require.True(t, ok)
	assert.Equal(t, "fill", msg)

	// The batch message was dropped; verify drop counter incremented.
	assert.GreaterOrEqual(t, b.PublishDrops(), int64(1))
}

func TestBatchMultipleScopedSubscribersSameScope(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	ch1, unsub1 := b.SubscribeScoped("t", "team")
	defer unsub1()
	ch2, unsub2 := b.SubscribeScoped("t", "team")
	defer unsub2()

	batch := b.Batch()
	batch.PublishTo("t", "team", "msg")
	batch.Flush()

	msgs := make([]string, 2)
	var ok bool
	msgs[0], ok = recv(ch1, time.Second)
	require.True(t, ok)
	msgs[1], ok = recv(ch2, time.Second)
	require.True(t, ok)
	sort.Strings(msgs)
	assert.Equal(t, []string{"msg", "msg"}, msgs)
}

func TestBatchFlushFiresAfterHooks(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	ch, unsub := b.Subscribe("t")
	defer unsub()

	var hookCalled int32
	done := make(chan struct{}, 2)
	b.After("t", func() {
		hookCalled++
		done <- struct{}{}
	})

	batch := b.Batch()
	batch.Publish("t", "one")
	batch.Publish("t", "two")
	batch.Flush()

	// Drain the message.
	_, ok := recv(ch, time.Second)
	require.True(t, ok)

	// Wait for the after hook to fire.
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("after hook was not called")
	}
	assert.Equal(t, int32(1), hookCalled)
}

func TestBatchFlushDispatchesToGlobSubscribers(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	// Regular subscriber.
	ch, unsub := b.Subscribe("events/click")
	defer unsub()

	// Glob subscriber matching the topic.
	globCh, globUnsub := b.SubscribeGlob("events/*")
	defer globUnsub()

	batch := b.Batch()
	batch.Publish("events/click", "a")
	batch.Publish("events/click", "b")
	batch.Flush()

	// Regular subscriber gets concatenated message.
	msg, ok := recv(ch, time.Second)
	require.True(t, ok)
	assert.Equal(t, "ab", msg)

	// Glob subscriber should also receive the message.
	select {
	case tm := <-globCh:
		assert.Equal(t, "events/click", tm.Topic)
		assert.Equal(t, "ab", tm.Data)
	case <-time.After(time.Second):
		t.Fatal("glob subscriber did not receive batch message")
	}
}

func TestBatchFlushRunsMiddleware(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	var middlewareCalled int32
	b.Use(func(next PublishFunc) PublishFunc {
		return func(topic, msg string) {
			middlewareCalled++
			next(topic, msg)
		}
	})

	ch, unsub := b.Subscribe("t")
	defer unsub()

	batch := b.Batch()
	batch.Publish("t", "hello")
	batch.Flush()

	_, ok := recv(ch, time.Second)
	require.True(t, ok)
	assert.Equal(t, int32(1), middlewareCalled)
}
