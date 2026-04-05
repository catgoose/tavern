package tavern

import (
	"testing"
	"time"

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
