package tavern

import (
	"testing"
	"time"

	"github.com/catgoose/tavern/backend"
	"github.com/catgoose/tavern/backend/memory"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBackendEnvelope_TTLAndID(t *testing.T) {
	mem := memory.New()
	defer mem.Close()

	b1 := NewSSEBroker(WithBackend(mem))
	fork := mem.Fork()
	b2 := NewSSEBroker(WithBackend(fork))
	defer b1.Close()
	defer b2.Close()

	ch, unsub := b2.Subscribe("topic")
	defer unsub()

	time.Sleep(20 * time.Millisecond)

	// Publish via b1 — basic envelope should be delivered.
	b1.Publish("topic", "hello")

	select {
	case msg := <-ch:
		assert.Equal(t, "hello", msg)
	case <-time.After(500 * time.Millisecond):
		t.Fatal("timeout waiting for backend message")
	}
}

func TestBackendEnvelope_TTLFieldPopulated(t *testing.T) {
	// Verify the envelope struct supports TTL and ID fields.
	env := backend.MessageEnvelope{
		Topic: "t",
		Data:  "d",
		TTL:   5000,
		ID:    "msg-1",
	}
	assert.Equal(t, int64(5000), env.TTL)
	assert.Equal(t, "msg-1", env.ID)
}

func TestHealthAwareBackend_SkipPublishWhenUnhealthy(t *testing.T) {
	mem := memory.New()
	defer mem.Close()

	fork := mem.Fork()
	b1 := NewSSEBroker(WithBackend(mem))
	b2 := NewSSEBroker(WithBackend(fork))
	defer b1.Close()
	defer b2.Close()

	ch, unsub := b2.Subscribe("orders")
	defer unsub()

	time.Sleep(20 * time.Millisecond)

	// Mark backend as unhealthy.
	mem.SetHealthy(false)

	// Publish on b1 — should be silently skipped because backend is unhealthy.
	b1.Publish("orders", "should-not-arrive")

	select {
	case msg := <-ch:
		t.Fatalf("should not receive message when backend unhealthy, got: %s", msg)
	case <-time.After(100 * time.Millisecond):
		// Expected: no message received.
	}

	// Mark healthy again.
	mem.SetHealthy(true)

	time.Sleep(20 * time.Millisecond)

	// Now publish should work.
	b1.Publish("orders", "recovered")

	select {
	case msg := <-ch:
		assert.Equal(t, "recovered", msg)
	case <-time.After(500 * time.Millisecond):
		t.Fatal("timeout after recovery")
	}
}

func TestHealthAwareBackend_ReconnectResubscribes(t *testing.T) {
	mem := memory.New()
	defer mem.Close()

	fork := mem.Fork()
	b1 := NewSSEBroker(WithBackend(mem))
	b2 := NewSSEBroker(WithBackend(fork))
	defer b1.Close()
	defer b2.Close()

	// Subscribe on b2.
	ch, unsub := b2.Subscribe("events")
	defer unsub()

	time.Sleep(20 * time.Millisecond)

	// Verify initial publish works.
	b1.Publish("events", "before")

	select {
	case msg := <-ch:
		assert.Equal(t, "before", msg)
	case <-time.After(500 * time.Millisecond):
		t.Fatal("timeout before disconnect")
	}

	// Simulate disconnect/reconnect cycle.
	mem.SetHealthy(false)
	time.Sleep(20 * time.Millisecond)
	mem.SetHealthy(true)

	// Wait for reconnect handler to fire and resubscribe.
	time.Sleep(50 * time.Millisecond)

	// Publish after reconnect should be delivered.
	b1.Publish("events", "after-reconnect")

	select {
	case msg := <-ch:
		assert.Equal(t, "after-reconnect", msg)
	case <-time.After(500 * time.Millisecond):
		t.Fatal("timeout after reconnect")
	}
}

func TestHealthAwareBackend_MemoryImplementsInterface(t *testing.T) {
	mem := memory.New()
	defer mem.Close()

	var hab backend.HealthAwareBackend = mem
	assert.True(t, hab.Healthy())

	mem.SetHealthy(false)
	assert.False(t, hab.Healthy())

	mem.SetHealthy(true)
	assert.True(t, hab.Healthy())
}

func TestHealthAwareBackend_ClosedBackendUnhealthy(t *testing.T) {
	mem := memory.New()
	mem.Close()

	assert.False(t, mem.Healthy())
}

func TestBackendPublishWithMeta(t *testing.T) {
	mem := memory.New()
	defer mem.Close()

	b := NewSSEBroker(WithBackend(mem))
	defer b.Close()

	// Exercise backendPublishWithMeta — it should not panic.
	b.backendPublishWithMeta("topic", "data", "", 5000, "id-1")
}

func TestObservableBackend_Interface(t *testing.T) {
	// Verify the ObservableBackend interface compiles and can be type-asserted.
	var _ backend.ObservableBackend = (*observableTestBackend)(nil)
}

// observableTestBackend is a minimal implementation for compile-time checking.
type observableTestBackend struct {
	memory.Backend
}

func (o *observableTestBackend) Stats() backend.BackendStats {
	return backend.BackendStats{Connected: true}
}

func TestHealthAwareBackend_OnReconnectCallback(t *testing.T) {
	mem := memory.New()
	defer mem.Close()

	var called bool
	mem.OnReconnect(func() {
		called = true
	})

	// Transition: healthy → unhealthy → healthy should fire callback.
	mem.SetHealthy(false)
	assert.False(t, called)

	mem.SetHealthy(true)
	require.True(t, called)
}
