package tavern

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
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

func TestSubscribeScoped_IsolatesMessages(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	chA, unsubA := b.SubscribeScoped("events", "acct-1")
	defer unsubA()
	chB, unsubB := b.SubscribeScoped("events", "acct-2")
	defer unsubB()

	b.PublishTo("events", "acct-1", "for-acct-1")
	b.PublishTo("events", "acct-2", "for-acct-2")

	select {
	case msg := <-chA:
		assert.Equal(t, "for-acct-1", msg)
	case <-time.After(time.Second):
		t.Fatal("acct-1 timed out")
	}

	select {
	case msg := <-chB:
		assert.Equal(t, "for-acct-2", msg)
	case <-time.After(time.Second):
		t.Fatal("acct-2 timed out")
	}

	// Verify no cross-delivery.
	select {
	case msg := <-chA:
		t.Fatalf("acct-1 received unexpected message: %s", msg)
	default:
	}
	select {
	case msg := <-chB:
		t.Fatalf("acct-2 received unexpected message: %s", msg)
	default:
	}
}

func TestPublishTo_NoMatchingScope(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	ch, unsub := b.SubscribeScoped("events", "acct-1")
	defer unsub()

	// Publish to a scope that has no subscribers.
	b.PublishTo("events", "acct-999", "ghost")

	select {
	case msg := <-ch:
		t.Fatalf("should not have received message: %s", msg)
	default:
		// expected — no delivery
	}
}

func TestPublishOOBTo(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	ch, unsub := b.SubscribeScoped("updates", "sess-1")
	defer unsub()

	b.PublishOOBTo("updates", "sess-1", Replace("status", "<b>online</b>"))

	select {
	case msg := <-ch:
		assert.Contains(t, msg, `hx-swap-oob="outerHTML"`)
		assert.Contains(t, msg, `id="status"`)
		assert.Contains(t, msg, "<b>online</b>")
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for OOB message")
	}
}

func TestSubscribeScoped_UnsubscribeCleanup(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	_, unsub := b.SubscribeScoped("events", "acct-1")

	require.True(t, b.HasSubscribers("events"))
	require.Equal(t, 1, b.SubscriberCount())

	unsub()

	require.False(t, b.HasSubscribers("events"))
	require.Equal(t, 0, b.SubscriberCount())

	// Double-unsub should not panic.
	assert.NotPanics(t, func() { unsub() })
}

func TestPublishTo_DropsWhenChannelFull(t *testing.T) {
	b := NewSSEBroker(WithBufferSize(1))
	defer b.Close()

	ch, unsub := b.SubscribeScoped("events", "user-1")
	defer unsub()

	// Fill the buffer.
	b.PublishTo("events", "user-1", "first")
	assert.Equal(t, int64(0), b.PublishDrops())

	// Overflow — should be dropped.
	b.PublishTo("events", "user-1", "overflow")
	assert.Equal(t, int64(1), b.PublishDrops())

	// Drain the one message that was buffered.
	select {
	case msg := <-ch:
		assert.Equal(t, "first", msg)
	case <-time.After(time.Second):
		t.Fatal("timed out")
	}
}

func TestPublishTo_NonExistentTopic(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	// Should not panic.
	assert.NotPanics(t, func() { b.PublishTo("ghost", "scope-x", "msg") })
}

func TestClose_WithScopedSubscribers(t *testing.T) {
	b := NewSSEBroker()

	ch1, _ := b.SubscribeScoped("events", "acct-1")
	ch2, _ := b.SubscribeScoped("events", "acct-2")

	// Should not panic.
	b.Close()

	// Channels should be closed.
	_, ok1 := <-ch1
	_, ok2 := <-ch2
	assert.False(t, ok1)
	assert.False(t, ok2)
}

func TestClose_EmptyBroker(t *testing.T) {
	b := NewSSEBroker()
	// Close on a broker with no subscribers should not panic.
	assert.NotPanics(t, func() { b.Close() })
}

func TestSubscribeAfterClose(t *testing.T) {
	b := NewSSEBroker()
	b.Close()

	ch, unsub := b.Subscribe("topic")
	defer unsub()

	// Channel must be closed immediately.
	select {
	case _, ok := <-ch:
		assert.False(t, ok, "channel returned from Subscribe after Close must be closed")
	default:
		t.Fatal("channel from Subscribe after Close was not immediately closed")
	}
}

func TestSubscribeScopedAfterClose(t *testing.T) {
	b := NewSSEBroker()
	b.Close()

	ch, unsub := b.SubscribeScoped("topic", "scope")
	defer unsub()

	select {
	case _, ok := <-ch:
		assert.False(t, ok, "channel returned from SubscribeScoped after Close must be closed")
	default:
		t.Fatal("channel from SubscribeScoped after Close was not immediately closed")
	}
}

func TestPublishAfterClose(t *testing.T) {
	b := NewSSEBroker()
	b.Close()

	// Publish after Close should not panic.
	assert.NotPanics(t, func() { b.Publish("topic", "msg") })
	assert.NotPanics(t, func() { b.PublishTo("topic", "scope", "msg") })
}

func TestScopedAndUnscopedCoexist(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	// Unscoped subscriber.
	chGlobal, unsubGlobal := b.Subscribe("events")
	defer unsubGlobal()

	// Scoped subscriber.
	chScoped, unsubScoped := b.SubscribeScoped("events", "acct-1")
	defer unsubScoped()

	// Unscoped publish reaches only unscoped subscribers.
	b.Publish("events", "broadcast")

	select {
	case msg := <-chGlobal:
		assert.Equal(t, "broadcast", msg)
	case <-time.After(time.Second):
		t.Fatal("global subscriber timed out")
	}

	select {
	case msg := <-chScoped:
		t.Fatalf("scoped subscriber should not receive unscoped publish: %s", msg)
	default:
	}

	// Scoped publish reaches only matching scoped subscribers.
	b.PublishTo("events", "acct-1", "scoped-msg")

	select {
	case msg := <-chScoped:
		assert.Equal(t, "scoped-msg", msg)
	case <-time.After(time.Second):
		t.Fatal("scoped subscriber timed out")
	}

	select {
	case msg := <-chGlobal:
		t.Fatalf("global subscriber should not receive scoped publish: %s", msg)
	default:
	}

	// Both count toward HasSubscribers and SubscriberCount.
	assert.True(t, b.HasSubscribers("events"))
	assert.Equal(t, 2, b.SubscriberCount())

	counts := b.TopicCounts()
	assert.Equal(t, 2, counts["events"])

	s := b.Stats()
	assert.Equal(t, 1, s.Topics)
	assert.Equal(t, 2, s.Subscribers)
}

func TestPublishWithReplay_NewSubscriberGetsReplay(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	b.PublishWithReplay("topic", "cached-msg")

	ch, unsub := b.Subscribe("topic")
	defer unsub()

	select {
	case msg := <-ch:
		assert.Equal(t, "cached-msg", msg)
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for replay message")
	}
}

func TestPublishWithReplay_UpdatesCache(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	b.PublishWithReplay("topic", "first")
	b.PublishWithReplay("topic", "second")

	ch, unsub := b.Subscribe("topic")
	defer unsub()

	select {
	case msg := <-ch:
		assert.Equal(t, "second", msg)
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for replay message")
	}

	// Only one replay message should be delivered.
	select {
	case msg := <-ch:
		t.Fatalf("unexpected extra replay message: %s", msg)
	default:
	}
}

func TestPublishWithReplay_ExistingSubscribersGetMessage(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	ch, unsub := b.Subscribe("topic")
	defer unsub()

	b.PublishWithReplay("topic", "live-msg")

	select {
	case msg := <-ch:
		assert.Equal(t, "live-msg", msg)
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for message")
	}
}

func TestPublishWithReplay_NoReplayForRegularPublish(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	b.Publish("topic", "regular-msg")

	ch, unsub := b.Subscribe("topic")
	defer unsub()

	select {
	case msg := <-ch:
		t.Fatalf("should not have received replay for regular publish: %s", msg)
	default:
		// expected — no replay
	}
}

func TestPublishWithReplay_ScopedSubscriberGetsReplay(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	b.PublishWithReplay("events", "cached-event")

	ch, unsub := b.SubscribeScoped("events", "acct-1")
	defer unsub()

	select {
	case msg := <-ch:
		assert.Equal(t, "cached-event", msg)
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for replay message on scoped subscriber")
	}
}

func TestClearReplay(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	b.PublishWithReplay("topic", "cached-msg")
	b.ClearReplay("topic")

	ch, unsub := b.Subscribe("topic")
	defer unsub()

	select {
	case msg := <-ch:
		t.Fatalf("should not have received replay after clear: %s", msg)
	default:
		// expected — no replay
	}
}

func TestPublishWithReplay_AfterClose(t *testing.T) {
	b := NewSSEBroker()
	b.Close()

	assert.NotPanics(t, func() { b.PublishWithReplay("topic", "msg") })
}

func TestRunPublisher_ContextCancellation(t *testing.T) {
	b := NewSSEBroker()
	ctx, cancel := context.WithCancel(context.Background())

	var ran atomic.Bool
	b.RunPublisher(ctx, func(ctx context.Context) {
		ran.Store(true)
		<-ctx.Done()
	})

	// Give the goroutine time to start.
	time.Sleep(10 * time.Millisecond)
	require.True(t, ran.Load())

	cancel()
	// Close should return promptly because publisher exits on cancel.
	done := make(chan struct{})
	go func() {
		b.Close()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Close blocked — publisher did not exit after context cancel")
	}
}

func TestRunPublisher_PanicRecovery(t *testing.T) {
	b := NewSSEBroker(WithLogger(slog.Default()))

	b.RunPublisher(context.Background(), func(_ context.Context) {
		panic("test panic")
	})

	// Close should return without hanging — the panicking publisher is recovered.
	done := make(chan struct{})
	go func() {
		b.Close()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Close blocked — panic was not recovered")
	}
}

func TestRunPublisher_PublishesMessages(t *testing.T) {
	b := NewSSEBroker()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch, unsub := b.Subscribe("tick")
	defer unsub()

	b.RunPublisher(ctx, func(ctx context.Context) {
		b.Publish("tick", "hello from publisher")
		<-ctx.Done()
	})

	select {
	case msg := <-ch:
		assert.Equal(t, "hello from publisher", msg)
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for publisher message")
	}

	cancel()
	b.Close()
}

func TestRunPublisher_CloseWaitsForAll(t *testing.T) {
	b := NewSSEBroker()
	ctx, cancel := context.WithCancel(context.Background())

	var count atomic.Int32
	for range 5 {
		b.RunPublisher(ctx, func(ctx context.Context) {
			count.Add(1)
			<-ctx.Done()
		})
	}

	time.Sleep(10 * time.Millisecond)
	require.Equal(t, int32(5), count.Load())

	cancel()
	b.Close()
	// All publishers have exited if Close returned.
}

func TestWithKeepalive_ReceivesHeartbeats(t *testing.T) {
	b := NewSSEBroker(WithKeepalive(50 * time.Millisecond))
	defer b.Close()

	ch, unsub := b.Subscribe("topic")
	defer unsub()

	select {
	case msg := <-ch:
		assert.Equal(t, ": keepalive\n", msg)
	case <-time.After(200 * time.Millisecond):
		t.Fatal("timed out waiting for keepalive")
	}
}

func TestWithKeepalive_StopsOnClose(t *testing.T) {
	b := NewSSEBroker(WithKeepalive(50 * time.Millisecond))

	ch, _ := b.Subscribe("topic")

	// Wait for at least one keepalive to confirm it's running.
	select {
	case <-ch:
	case <-time.After(200 * time.Millisecond):
		t.Fatal("timed out waiting for initial keepalive")
	}

	b.Close()

	// After close, no more messages should arrive (channel is closed).
	select {
	case _, ok := <-ch:
		assert.False(t, ok, "channel should be closed after Close")
	case <-time.After(200 * time.Millisecond):
		t.Fatal("timed out — channel was not closed")
	}
}

func TestWithKeepalive_DefaultOff(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	ch, unsub := b.Subscribe("topic")
	defer unsub()

	select {
	case msg := <-ch:
		t.Fatalf("unexpected message received: %s", msg)
	case <-time.After(100 * time.Millisecond):
		// expected — no keepalive
	}
}

func TestWithKeepalive_ScopedSubscribers(t *testing.T) {
	b := NewSSEBroker(WithKeepalive(50 * time.Millisecond))
	defer b.Close()

	ch, unsub := b.SubscribeScoped("events", "acct-1")
	defer unsub()

	select {
	case msg := <-ch:
		assert.Equal(t, ": keepalive\n", msg)
	case <-time.After(200 * time.Millisecond):
		t.Fatal("timed out waiting for keepalive on scoped subscriber")
	}
}

func TestWithKeepalive_DoesNotCountDrops(t *testing.T) {
	b := NewSSEBroker(WithKeepalive(50*time.Millisecond), WithBufferSize(1))

	// Subscribe but don't drain — buffer will fill.
	_, _ = b.Subscribe("topic")

	// Wait long enough for several keepalive ticks.
	time.Sleep(200 * time.Millisecond)

	// Stop the keepalive goroutine before asserting to avoid races on cleanup.
	b.Close()

	// Keepalive drops must not be counted.
	assert.Equal(t, int64(0), b.PublishDrops())
}

func TestWithTopicTTL_CleansUpEmptyTopics(t *testing.T) {
	b := NewSSEBroker(WithTopicTTL(50 * time.Millisecond))
	defer b.Close()

	_, unsub := b.Subscribe("ephemeral")
	unsub()

	// Topic map entry still exists right after unsub.
	counts := b.TopicCounts()
	assert.Contains(t, counts, "ephemeral")

	// After the sweep, topic should be removed.
	assert.Eventually(t, func() bool {
		return len(b.TopicCounts()) == 0
	}, time.Second, 10*time.Millisecond)
}

func TestWithTopicTTL_DoesNotCleanActiveTopics(t *testing.T) {
	b := NewSSEBroker(WithTopicTTL(50 * time.Millisecond))
	defer b.Close()

	_, unsub := b.Subscribe("active")
	defer unsub()

	// Wait longer than the TTL.
	time.Sleep(100 * time.Millisecond)

	counts := b.TopicCounts()
	assert.Equal(t, 1, counts["active"])
}

func TestWithTopicTTL_ResubscribeResetsTimer(t *testing.T) {
	b := NewSSEBroker(WithTopicTTL(80 * time.Millisecond))
	defer b.Close()

	// First subscribe/unsubscribe.
	_, unsub1 := b.Subscribe("topic")
	unsub1()

	// Wait less than TTL.
	time.Sleep(30 * time.Millisecond)

	// Resubscribe resets the timer, then unsubscribe again.
	_, unsub2 := b.Subscribe("topic")
	unsub2()

	// Wait less than TTL from the second unsubscribe — topic should still exist.
	time.Sleep(30 * time.Millisecond)
	counts := b.TopicCounts()
	assert.Contains(t, counts, "topic")

	// Eventually it should be cleaned up.
	assert.Eventually(t, func() bool {
		return len(b.TopicCounts()) == 0
	}, time.Second, 10*time.Millisecond)
}

func TestWithTopicTTL_DefaultOff(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	_, unsub := b.Subscribe("persistent")
	unsub()

	// Without TTL, the empty topic map entry persists.
	time.Sleep(50 * time.Millisecond)
	counts := b.TopicCounts()
	assert.Contains(t, counts, "persistent")
}

func TestWithTopicTTL_StopsOnClose(t *testing.T) {
	b := NewSSEBroker(WithTopicTTL(50 * time.Millisecond))

	_, unsub := b.Subscribe("topic")
	unsub()

	// Close should not panic and should stop the sweep goroutine.
	assert.NotPanics(t, func() { b.Close() })
	// Double close should also not panic.
	assert.NotPanics(t, func() { b.Close() })
}

func TestWithTopicTTL_ScopedTopics(t *testing.T) {
	b := NewSSEBroker(WithTopicTTL(50 * time.Millisecond))
	defer b.Close()

	_, unsub := b.SubscribeScoped("events", "acct-1")
	unsub()

	// After TTL, scoped topic entry should be cleaned up.
	assert.Eventually(t, func() bool {
		return len(b.TopicCounts()) == 0
	}, time.Second, 10*time.Millisecond)
}

func TestPublishIfChanged_PublishesNewMessage(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	ch, unsub := b.Subscribe("t")
	defer unsub()

	ok := b.PublishIfChanged("t", "hello")
	require.True(t, ok)

	select {
	case msg := <-ch:
		assert.Equal(t, "hello", msg)
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for message")
	}
}

func TestPublishIfChanged_SkipsDuplicate(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	ch, unsub := b.Subscribe("t")
	defer unsub()

	ok1 := b.PublishIfChanged("t", "same")
	require.True(t, ok1)

	ok2 := b.PublishIfChanged("t", "same")
	require.False(t, ok2)

	// Drain the one message that was delivered.
	select {
	case msg := <-ch:
		assert.Equal(t, "same", msg)
	case <-time.After(time.Second):
		t.Fatal("timed out")
	}

	// No second message should be present.
	select {
	case msg := <-ch:
		t.Fatalf("unexpected second message: %s", msg)
	default:
	}
}

func TestPublishIfChanged_PublishesAfterChange(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	ch, unsub := b.Subscribe("t")
	defer unsub()

	ok1 := b.PublishIfChanged("t", "A")
	require.True(t, ok1)

	ok2 := b.PublishIfChanged("t", "B")
	require.True(t, ok2)

	select {
	case msg := <-ch:
		assert.Equal(t, "A", msg)
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for first message")
	}

	select {
	case msg := <-ch:
		assert.Equal(t, "B", msg)
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for second message")
	}
}

func TestPublishIfChanged_PerTopic(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	ch1, unsub1 := b.Subscribe("topic1")
	defer unsub1()
	ch2, unsub2 := b.Subscribe("topic2")
	defer unsub2()

	// Same message to different topics — both should publish.
	ok1 := b.PublishIfChanged("topic1", "same")
	ok2 := b.PublishIfChanged("topic2", "same")
	require.True(t, ok1)
	require.True(t, ok2)

	select {
	case msg := <-ch1:
		assert.Equal(t, "same", msg)
	case <-time.After(time.Second):
		t.Fatal("topic1 timed out")
	}

	select {
	case msg := <-ch2:
		assert.Equal(t, "same", msg)
	case <-time.After(time.Second):
		t.Fatal("topic2 timed out")
	}
}

func TestPublishIfChanged_AfterClose(t *testing.T) {
	b := NewSSEBroker()
	b.Close()

	assert.NotPanics(t, func() {
		ok := b.PublishIfChanged("t", "msg")
		assert.False(t, ok)
	})
}

func TestClearDedup(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	ch, unsub := b.Subscribe("t")
	defer unsub()

	ok1 := b.PublishIfChanged("t", "msg")
	require.True(t, ok1)

	// Clear dedup state.
	b.ClearDedup("t")

	// Same message should now publish again.
	ok2 := b.PublishIfChanged("t", "msg")
	require.True(t, ok2)

	// Drain both messages.
	for range 2 {
		select {
		case <-ch:
		case <-time.After(time.Second):
			t.Fatal("timed out")
		}
	}
}

func TestPublishIfChanged_EmptyMessage(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	ch, unsub := b.Subscribe("t")
	defer unsub()

	ok1 := b.PublishIfChanged("t", "")
	require.True(t, ok1)

	ok2 := b.PublishIfChanged("t", "")
	require.False(t, ok2)

	select {
	case msg := <-ch:
		assert.Equal(t, "", msg)
	case <-time.After(time.Second):
		t.Fatal("timed out")
	}

	select {
	case msg := <-ch:
		t.Fatalf("unexpected second message: %q", msg)
	default:
	}
}

func TestOnFirstSubscriber_FiresOnFirstSub(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	fired := make(chan string, 1)
	b.OnFirstSubscriber("t", func(topic string) { fired <- topic })

	_, unsub := b.Subscribe("t")
	defer unsub()

	select {
	case got := <-fired:
		assert.Equal(t, "t", got)
	case <-time.After(time.Second):
		t.Fatal("hook did not fire")
	}
}

func TestOnLastUnsubscribe_FiresWhenLastLeaves(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	fired := make(chan string, 1)
	b.OnLastUnsubscribe("t", func(topic string) { fired <- topic })

	_, unsub := b.Subscribe("t")
	unsub()

	select {
	case got := <-fired:
		assert.Equal(t, "t", got)
	case <-time.After(time.Second):
		t.Fatal("hook did not fire")
	}
}

func TestLifecycleHooks_MultipleHooksPerTopic(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	fired := make(chan int, 2)
	b.OnFirstSubscriber("t", func(string) { fired <- 1 })
	b.OnFirstSubscriber("t", func(string) { fired <- 2 })

	_, unsub := b.Subscribe("t")
	defer unsub()

	got := make(map[int]bool)
	for range 2 {
		select {
		case v := <-fired:
			got[v] = true
		case <-time.After(time.Second):
			t.Fatal("hook did not fire")
		}
	}
	assert.True(t, got[1])
	assert.True(t, got[2])
}

func TestLifecycleHooks_SurvivesSubscriberCycles(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	count := make(chan string, 2)
	b.OnFirstSubscriber("t", func(topic string) { count <- topic })

	// First cycle.
	_, unsub1 := b.Subscribe("t")
	select {
	case <-count:
	case <-time.After(time.Second):
		t.Fatal("first hook did not fire")
	}
	unsub1()

	// Second cycle — hook should fire again.
	_, unsub2 := b.Subscribe("t")
	defer unsub2()
	select {
	case <-count:
	case <-time.After(time.Second):
		t.Fatal("hook did not fire on second cycle")
	}
}

func TestLifecycleHooks_CountsBothScopedAndUnscoped(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	lastFired := make(chan string, 1)
	b.OnLastUnsubscribe("t", func(topic string) { lastFired <- topic })

	// Subscribe unscoped then scoped — total 2.
	_, unsubUnscoped := b.Subscribe("t")
	_, unsubScoped := b.SubscribeScoped("t", "s1")

	// Remove unscoped — total still 1, onLast should NOT fire.
	unsubUnscoped()
	select {
	case <-lastFired:
		t.Fatal("onLast fired while scoped subscriber still active")
	case <-time.After(100 * time.Millisecond):
		// expected
	}

	// Remove scoped — total now 0, onLast should fire.
	unsubScoped()
	select {
	case got := <-lastFired:
		assert.Equal(t, "t", got)
	case <-time.After(time.Second):
		t.Fatal("onLast did not fire")
	}
}

func TestLifecycleHooks_ClosedBroker(t *testing.T) {
	b := NewSSEBroker()
	b.Close()

	fired := make(chan string, 1)
	b.OnFirstSubscriber("t", func(topic string) { fired <- topic })

	_, unsub := b.Subscribe("t")
	defer unsub()

	select {
	case <-fired:
		t.Fatal("hook should not fire on closed broker")
	case <-time.After(100 * time.Millisecond):
		// expected
	}
}

func TestOnFirstSubscriber_NonBlocking(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	b.OnFirstSubscriber("t", func(string) {
		time.Sleep(500 * time.Millisecond)
	})

	done := make(chan struct{})
	go func() {
		_, unsub := b.Subscribe("t")
		defer unsub()
		close(done)
	}()

	select {
	case <-done:
		// Subscribe returned quickly — not blocked by slow hook.
	case <-time.After(200 * time.Millisecond):
		t.Fatal("Subscribe was blocked by slow hook")
	}
}
