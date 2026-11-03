package tavern

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/catgoose/tavern/backend"
	"github.com/catgoose/tavern/backend/memory"
	"github.com/stretchr/testify/require"
)

func TestBackendCrossInstancePublish(t *testing.T) {
	mem := memory.New()
	defer mem.Close()

	b1 := NewSSEBroker(WithBackend(mem))
	b2 := NewSSEBroker(WithBackend(mem.Fork()))
	defer b1.Close()
	defer b2.Close()

	// Subscribe on b2.
	ch, unsub := b2.Subscribe("orders")
	defer unsub()

	// Give the backend fan-in goroutine a moment to start.
	time.Sleep(20 * time.Millisecond)

	// Publish on b1 — should reach b2's subscriber.
	b1.Publish("orders", "order-1")

	select {
	case got := <-ch:
		require.Equal(t, "order-1", got)
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for cross-instance message")
	}
}

func TestBackendScopedCrossInstance(t *testing.T) {
	mem := memory.New()
	defer mem.Close()

	b1 := NewSSEBroker(WithBackend(mem))
	b2 := NewSSEBroker(WithBackend(mem.Fork()))
	defer b1.Close()
	defer b2.Close()

	ch, unsub := b2.SubscribeScoped("orders", "user:42")
	defer unsub()

	time.Sleep(20 * time.Millisecond)

	b1.PublishTo("orders", "user:42", "scoped-msg")

	select {
	case got := <-ch:
		require.Equal(t, "scoped-msg", got)
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for scoped cross-instance message")
	}
}

func TestBackendLocalPublishStillWorks(t *testing.T) {
	mem := memory.New()
	defer mem.Close()

	b := NewSSEBroker(WithBackend(mem))
	defer b.Close()

	ch, unsub := b.Subscribe("orders")
	defer unsub()

	b.Publish("orders", "local")

	select {
	case got := <-ch:
		require.Equal(t, "local", got)
	case <-time.After(time.Second):
		t.Fatal("local message not received")
	}
}

func TestBackendUnsubscribeOnLastLeave(t *testing.T) {
	mem := memory.New()
	defer mem.Close()

	b := NewSSEBroker(WithBackend(mem))
	defer b.Close()

	_, unsub := b.Subscribe("orders")
	unsub()

	// After unsubscribe, the backend subscription for the topic should be
	// cleaned up. Publishing should not panic or block.
	b2 := NewSSEBroker(WithBackend(mem.Fork()))
	defer b2.Close()
	b2.Publish("orders", "after-unsub")
	// No assertion needed — just verify no deadlock or panic.
}

func TestBackendNilDoesNotPanic(t *testing.T) {
	b := NewSSEBroker() // no backend
	defer b.Close()

	ch, unsub := b.Subscribe("orders")
	defer unsub()

	b.Publish("orders", "hello")

	select {
	case got := <-ch:
		require.Equal(t, "hello", got)
	case <-time.After(time.Second):
		t.Fatal("message not received")
	}
}

func TestBackendGlobSubscribers(t *testing.T) {
	mem := memory.New()
	defer mem.Close()

	b1 := NewSSEBroker(WithBackend(mem))
	b2 := NewSSEBroker(WithBackend(mem.Fork()))
	defer b1.Close()
	defer b2.Close()

	// Subscribe a regular subscriber on b2 to trigger backend subscription.
	_, regularUnsub := b2.Subscribe("orders")
	defer regularUnsub()

	// Subscribe a glob subscriber on b2.
	globCh, globUnsub := b2.SubscribeGlob("orders")
	defer globUnsub()

	time.Sleep(20 * time.Millisecond)

	// Publish on b1 — should reach b2's glob subscriber.
	b1.Publish("orders", "glob-msg")

	select {
	case got := <-globCh:
		require.Equal(t, "orders", got.Topic)
		require.Equal(t, "glob-msg", got.Data)
	case <-time.After(time.Second):
		t.Fatal("glob subscriber did not receive cross-instance message")
	}
}

func TestBackendScopedGlobSubscribers(t *testing.T) {
	mem := memory.New()
	defer mem.Close()

	b1 := NewSSEBroker(WithBackend(mem))
	b2 := NewSSEBroker(WithBackend(mem.Fork()))
	defer b1.Close()
	defer b2.Close()

	// Subscribe a regular scoped subscriber on b2 to trigger backend subscription.
	_, regularUnsub := b2.SubscribeScoped("orders", "user:42")
	defer regularUnsub()

	// Subscribe a scoped glob subscriber on b2.
	globCh, globUnsub := b2.SubscribeGlobScoped("orders", "user:42")
	defer globUnsub()

	time.Sleep(20 * time.Millisecond)

	// Scoped publish on b1 — should reach b2's scoped glob subscriber.
	b1.PublishTo("orders", "user:42", "scoped-glob-msg")

	select {
	case got := <-globCh:
		require.Equal(t, "orders", got.Topic)
		require.Equal(t, "scoped-glob-msg", got.Data)
	case <-time.After(time.Second):
		t.Fatal("scoped glob subscriber did not receive cross-instance message")
	}
}

func TestBackendFilteredSubscribers(t *testing.T) {
	mem := memory.New()
	defer mem.Close()

	b1 := NewSSEBroker(WithBackend(mem))
	b2 := NewSSEBroker(WithBackend(mem.Fork()))
	defer b1.Close()
	defer b2.Close()

	// A regular subscriber triggers the backend subscription for this topic.
	_, triggerUnsub := b2.Subscribe("orders")
	defer triggerUnsub()

	// Filtered subscriber on b2 that only accepts messages containing "important".
	ch, unsub := b2.SubscribeWithFilter("orders", func(msg string) bool {
		return msg == "important"
	})
	defer unsub()

	time.Sleep(20 * time.Millisecond)

	// Publish messages on b1.
	b1.Publish("orders", "boring")
	b1.Publish("orders", "important")

	select {
	case got := <-ch:
		require.Equal(t, "important", got)
	case <-time.After(time.Second):
		t.Fatal("filtered subscriber did not receive matching cross-instance message")
	}

	// Verify "boring" was not delivered.
	select {
	case msg := <-ch:
		t.Fatalf("unexpected message: %s", msg)
	case <-time.After(50 * time.Millisecond):
		// expected: no more messages
	}
}

func TestBackendFilteredScopedSubscribers(t *testing.T) {
	mem := memory.New()
	defer mem.Close()

	b1 := NewSSEBroker(WithBackend(mem))
	b2 := NewSSEBroker(WithBackend(mem.Fork()))
	defer b1.Close()
	defer b2.Close()

	// A regular scoped subscriber triggers the backend subscription.
	_, triggerUnsub := b2.SubscribeScoped("orders", "user:42")
	defer triggerUnsub()

	// Filtered scoped subscriber on b2.
	ch, unsub := b2.SubscribeScopedWithFilter("orders", "user:42", func(msg string) bool {
		return msg == "match"
	})
	defer unsub()

	time.Sleep(20 * time.Millisecond)

	// Publish messages on b1.
	b1.PublishTo("orders", "user:42", "nomatch")
	b1.PublishTo("orders", "user:42", "match")
	b1.PublishTo("orders", "user:99", "match") // wrong scope

	select {
	case got := <-ch:
		require.Equal(t, "match", got)
	case <-time.After(time.Second):
		t.Fatal("filtered scoped subscriber did not receive matching message")
	}

	// Verify no extra messages.
	select {
	case msg := <-ch:
		t.Fatalf("unexpected message: %s", msg)
	case <-time.After(50 * time.Millisecond):
		// expected
	}
}

func TestBackendFanIn_NoRaceOnUnsubscribe(t *testing.T) {
	mem := memory.New()
	defer mem.Close()

	b1 := NewSSEBroker(WithBackend(mem))
	b2 := NewSSEBroker(WithBackend(mem.Fork()))
	defer b1.Close()
	defer b2.Close()

	for range 100 {
		ch, unsub := b2.Subscribe("race-test")
		// Give backend fan-in a moment to start.
		time.Sleep(time.Millisecond)
		// Publish while unsubscribing concurrently.
		go b1.Publish("race-test", "msg")
		unsub()
		_ = ch
	}
}

// countingBackend records how many backend subscriptions are active per topic.
type countingBackend struct {
	*memory.Backend
	mu     sync.Mutex
	active map[string]int
}

func newCountingBackend(mem *memory.Backend) *countingBackend {
	return &countingBackend{Backend: mem, active: make(map[string]int)}
}

func (c *countingBackend) Subscribe(ctx context.Context, topic string) (<-chan backend.MessageEnvelope, error) {
	c.mu.Lock()
	c.active[topic]++
	c.mu.Unlock()
	return c.Backend.Subscribe(ctx, topic)
}

func (c *countingBackend) Unsubscribe(topic string) error {
	c.mu.Lock()
	c.active[topic]--
	c.mu.Unlock()
	return c.Backend.Unsubscribe(topic)
}

func (c *countingBackend) activeFor(topic string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.active[topic]
}

func TestBackendDisconnectOnLossCrossInstance(t *testing.T) {
	mem := memory.New()
	defer mem.Close()
	remote := newCountingBackend(mem.Fork())

	b1 := NewSSEBroker(WithBackend(mem))
	b2 := NewSSEBroker(WithBackend(remote))
	defer b1.Close()
	defer b2.Close()

	ch, unsub := b2.SubscribeWith("orders", SubWithScope("u1"), SubWithDisconnectOnLoss())
	require.Equal(t, 1, remote.activeFor("orders"))

	b1.PublishTo("orders", "u1", "order-1")
	select {
	case got := <-ch:
		require.Equal(t, "order-1", got)
	case <-time.After(time.Second):
		t.Fatal("loss-aware subscriber did not receive cross-instance message")
	}

	unsub()
	require.Equal(t, 0, remote.activeFor("orders"))
}

func TestBackendDisconnectOnLossAggregateCleanup(t *testing.T) {
	mem := memory.New()
	defer mem.Close()
	counting := newCountingBackend(mem)

	b := NewSSEBroker(WithBufferSize(1), WithBackend(counting))
	defer b.Close()

	ch, unsub := b.SubscribeMultiWith([]string{"a", "b"}, SubWithDisconnectOnLoss())
	defer unsub()
	require.Equal(t, 1, counting.activeFor("a"))
	require.Equal(t, 1, counting.activeFor("b"))

	// Nobody reads the aggregate, so "a" eventually overflows and is evicted;
	// termination then cancels "b".
	for i := 0; b.HasSubscribers("a"); i++ {
		require.Less(t, i, 1000, "inner subscription never overflowed")
		b.Publish("a", "msg")
	}
	require.Eventually(t, func() bool { return !b.HasSubscribers("b") }, 2*time.Second, time.Millisecond)
	drainMultiUntilClosed(t, ch)

	require.Equal(t, 0, counting.activeFor("a"), "eviction releases the backend subscription")
	require.Equal(t, 0, counting.activeFor("b"), "sibling cancellation releases the backend subscription")

	// A new subscriber registers the topic exactly once again.
	_, unsubAgain := b.SubscribeWith("a", SubWithDisconnectOnLoss())
	defer unsubAgain()
	require.Equal(t, 1, counting.activeFor("a"))
}
