package tavern

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAfterHookFires(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	ch, unsub := b.Subscribe("orders:list")
	defer unsub()

	var fired atomic.Bool
	done := make(chan struct{})
	b.After("orders:list", func() {
		fired.Store(true)
		close(done)
	})

	b.Publish("orders:list", "data")
	<-ch // drain the published message

	select {
	case <-done:
		assert.True(t, fired.Load())
	case <-time.After(time.Second):
		t.Fatal("After hook did not fire within timeout")
	}
}

func TestAfterHookMultipleHooks(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	ch, unsub := b.Subscribe("topic")
	defer unsub()

	var order []int
	var mu sync.Mutex
	done := make(chan struct{})

	b.After("topic", func() {
		mu.Lock()
		order = append(order, 1)
		mu.Unlock()
	})
	b.After("topic", func() {
		mu.Lock()
		order = append(order, 2)
		mu.Unlock()
		close(done)
	})

	b.Publish("topic", "msg")
	<-ch

	select {
	case <-done:
		mu.Lock()
		assert.Equal(t, []int{1, 2}, order)
		mu.Unlock()
	case <-time.After(time.Second):
		t.Fatal("hooks did not fire")
	}
}

func TestAfterHookAsync(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	ch, unsub := b.Subscribe("topic")
	defer unsub()

	hookStarted := make(chan struct{})
	hookDone := make(chan struct{})
	b.After("topic", func() {
		close(hookStarted)
		<-hookDone // block the hook
	})

	b.Publish("topic", "msg")
	<-ch

	// The hook should have started asynchronously — Publish returns before the hook finishes.
	select {
	case <-hookStarted:
		// good, hook started in background
	case <-time.After(time.Second):
		t.Fatal("After hook did not start asynchronously")
	}
	close(hookDone)
}

func TestAfterHookCrossTopicChain(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	chA, unsubA := b.Subscribe("A")
	defer unsubA()
	chB, unsubB := b.Subscribe("B")
	defer unsubB()

	var bFired atomic.Bool
	done := make(chan struct{})

	// After A publishes, publish to B.
	b.After("A", func() {
		b.Publish("B", "from-A-hook")
	})
	// After B publishes, record it.
	b.After("B", func() {
		bFired.Store(true)
		close(done)
	})

	b.Publish("A", "trigger")
	<-chA

	select {
	case <-done:
		assert.True(t, bFired.Load())
	case <-time.After(time.Second):
		t.Fatal("chained After hook did not fire")
	}

	// B should have received the message from A's hook.
	select {
	case msg := <-chB:
		assert.Equal(t, "from-A-hook", msg)
	case <-time.After(time.Second):
		t.Fatal("B did not receive message from A's hook")
	}
}

func TestAfterHookCycleDetection(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	chA, unsubA := b.Subscribe("A")
	defer unsubA()
	_, unsubB := b.Subscribe("B")
	defer unsubB()

	var aCount, bCount atomic.Int32
	done := make(chan struct{})

	// A -> B -> A would be a cycle.
	b.After("A", func() {
		aCount.Add(1)
		b.Publish("B", "from-A")
	})
	b.After("B", func() {
		bCount.Add(1)
		b.Publish("A", "from-B")
		// Signal completion after B's hook runs.
		select {
		case <-done:
		default:
			close(done)
		}
	})

	b.Publish("A", "start")
	<-chA

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("cycle detection did not terminate hooks")
	}

	// Give a bit of time for any additional hooks to (not) fire.
	time.Sleep(50 * time.Millisecond)

	// A's hook should fire once (for the initial publish).
	// B's hook should fire once (triggered by A's hook).
	// The cycle A->B->A should be stopped by the seen-set.
	assert.Equal(t, int32(1), aCount.Load(), "A's After hook should fire exactly once")
	assert.Equal(t, int32(1), bCount.Load(), "B's After hook should fire exactly once")
}

func TestAfterHookMaxDepth(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	// Create a chain of topics: t0 -> t1 -> t2 -> ... -> t(maxAfterDepth+2)
	// Only the first maxAfterDepth should fire.
	const chainLen = maxAfterDepth + 3
	var counts [chainLen]atomic.Int32
	unsubs := make([]func(), chainLen)

	for i := range chainLen {
		topic := "t" + string(rune('0'+i))
		_, unsub := b.Subscribe(topic)
		unsubs[i] = unsub
	}
	defer func() {
		for _, u := range unsubs {
			u()
		}
	}()

	done := make(chan struct{})
	for i := range chainLen - 1 {
		idx := i
		src := "t" + string(rune('0'+idx))
		dst := "t" + string(rune('0'+idx+1))
		b.After(src, func() {
			counts[idx].Add(1)
			b.Publish(dst, "chain")
			if idx == chainLen-2 || counts[idx].Load() == 1 {
				select {
				case <-done:
				default:
				}
			}
		})
	}
	// Last topic's hook just counts.
	lastTopic := "t" + string(rune('0'+chainLen-1))
	b.After(lastTopic, func() {
		counts[chainLen-1].Add(1)
	})

	b.Publish("t0", "start")

	// Wait for the chain to settle.
	time.Sleep(200 * time.Millisecond)

	// First maxAfterDepth hooks should fire (depth 1 through maxAfterDepth).
	// The chain is: publish to t0 (depth 0, not a hook), After t0 fires (depth 1),
	// publishes to t1, After t1 fires (depth 2), etc.
	fired := 0
	for i := range chainLen {
		if counts[i].Load() > 0 {
			fired++
		}
	}
	assert.LessOrEqual(t, fired, maxAfterDepth,
		"no more than maxAfterDepth hooks should fire in a chain")
}

func TestAfterHookOnClosedBroker(t *testing.T) {
	b := NewSSEBroker()
	b.Close()

	// Should not panic.
	b.After("topic", func() {
		t.Fatal("should not fire on closed broker")
	})
}

func TestAfterHookFiresOnPublishIfChanged(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	ch, unsub := b.Subscribe("topic")
	defer unsub()

	var fired atomic.Bool
	done := make(chan struct{})
	b.After("topic", func() {
		fired.Store(true)
		close(done)
	})

	b.PublishIfChanged("topic", "data")
	<-ch

	select {
	case <-done:
		assert.True(t, fired.Load())
	case <-time.After(time.Second):
		t.Fatal("After hook did not fire for PublishIfChanged")
	}
}

func TestAfterHookDoesNotFireOnDuplicate(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	ch, unsub := b.Subscribe("topic")
	defer unsub()

	var count atomic.Int32
	b.After("topic", func() {
		count.Add(1)
	})

	b.PublishIfChanged("topic", "same")
	<-ch

	// Second publish with same content should be skipped.
	changed := b.PublishIfChanged("topic", "same")
	assert.False(t, changed)

	time.Sleep(100 * time.Millisecond)
	assert.Equal(t, int32(1), count.Load(), "hook should fire only once for deduplicated publishes")
}

func TestAfterHookFiresOnPublishWithReplay(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	ch, unsub := b.Subscribe("topic")
	defer unsub()

	var fired atomic.Bool
	done := make(chan struct{})
	b.After("topic", func() {
		fired.Store(true)
		close(done)
	})

	b.PublishWithReplay("topic", "data")
	<-ch

	select {
	case <-done:
		assert.True(t, fired.Load())
	case <-time.After(time.Second):
		t.Fatal("After hook did not fire for PublishWithReplay")
	}
}

func TestAfterHookFiresOnPublishTo(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	ch, unsub := b.SubscribeScoped("topic", "scope1")
	defer unsub()

	var fired atomic.Bool
	done := make(chan struct{})
	b.After("topic", func() {
		fired.Store(true)
		close(done)
	})

	b.PublishTo("topic", "scope1", "data")
	<-ch

	select {
	case <-done:
		assert.True(t, fired.Load())
	case <-time.After(time.Second):
		t.Fatal("After hook did not fire for PublishTo")
	}
}

// --- OnMutate / NotifyMutate tests ---

func TestOnMutateNotifyMutate(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	var received MutationEvent
	b.OnMutate("orders", func(e MutationEvent) {
		received = e
	})

	b.NotifyMutate("orders", MutationEvent{ID: "42", Data: "order-data"})

	assert.Equal(t, "42", received.ID)
	assert.Equal(t, "order-data", received.Data)
}

func TestOnMutateMultipleHandlers(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	var order []int
	b.OnMutate("orders", func(_ MutationEvent) {
		order = append(order, 1)
	})
	b.OnMutate("orders", func(_ MutationEvent) {
		order = append(order, 2)
	})

	b.NotifyMutate("orders", MutationEvent{ID: "1"})

	assert.Equal(t, []int{1, 2}, order)
}

func TestNotifyMutateNoHandlers(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	// Should not panic.
	b.NotifyMutate("nonexistent", MutationEvent{ID: "1"})
}

func TestOnMutateClosedBroker(t *testing.T) {
	b := NewSSEBroker()
	b.Close()

	// Should not panic.
	b.OnMutate("orders", func(_ MutationEvent) {
		t.Fatal("should not be registered on closed broker")
	})

	// NotifyMutate on closed broker should be safe too.
	b.NotifyMutate("orders", MutationEvent{ID: "1"})
}

func TestOnMutateTriggerPublish(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	ch, unsub := b.Subscribe("orders:list")
	defer unsub()

	b.OnMutate("orders", func(e MutationEvent) {
		b.Publish("orders:list", "updated-"+e.ID)
	})

	b.NotifyMutate("orders", MutationEvent{ID: "42"})

	select {
	case msg := <-ch:
		assert.Equal(t, "updated-42", msg)
	case <-time.After(time.Second):
		t.Fatal("did not receive publish from OnMutate handler")
	}
}

func TestOnMutateWithAfterHook(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	chList, unsubList := b.Subscribe("orders:list")
	defer unsubList()
	chStats, unsubStats := b.Subscribe("dashboard:stats")
	defer unsubStats()

	b.OnMutate("orders", func(e MutationEvent) {
		b.Publish("orders:list", "row-"+e.ID)
	})

	var afterFired atomic.Bool
	done := make(chan struct{})
	b.After("orders:list", func() {
		b.Publish("dashboard:stats", "stats-updated")
		afterFired.Store(true)
		close(done)
	})

	b.NotifyMutate("orders", MutationEvent{ID: "7"})

	select {
	case msg := <-chList:
		assert.Equal(t, "row-7", msg)
	case <-time.After(time.Second):
		t.Fatal("orders:list did not receive message")
	}

	select {
	case <-done:
		assert.True(t, afterFired.Load())
	case <-time.After(time.Second):
		t.Fatal("After hook did not fire from OnMutate publish")
	}

	select {
	case msg := <-chStats:
		assert.Equal(t, "stats-updated", msg)
	case <-time.After(time.Second):
		t.Fatal("dashboard:stats did not receive message from After hook")
	}
}

func TestOnMutateMultipleResources(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	var ordersFired, usersFired atomic.Bool

	b.OnMutate("orders", func(_ MutationEvent) {
		ordersFired.Store(true)
	})
	b.OnMutate("users", func(_ MutationEvent) {
		usersFired.Store(true)
	})

	b.NotifyMutate("orders", MutationEvent{ID: "1"})

	assert.True(t, ordersFired.Load())
	assert.False(t, usersFired.Load(), "users handler should not fire for orders mutation")
}

func TestNotifyMutateConcurrent(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	var count atomic.Int32
	b.OnMutate("orders", func(_ MutationEvent) {
		count.Add(1)
	})

	var wg sync.WaitGroup
	for i := range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			b.NotifyMutate("orders", MutationEvent{ID: string(rune('0' + i))})
		}()
	}
	wg.Wait()

	assert.Equal(t, int32(50), count.Load())
}

func TestAfterHookConcurrentPublish(t *testing.T) {
	b := NewSSEBroker(WithBufferSize(50))
	defer b.Close()

	ch, unsub := b.Subscribe("topic")
	defer unsub()

	var hookCount atomic.Int32
	b.After("topic", func() {
		hookCount.Add(1)
	})

	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			b.Publish("topic", "msg")
		}()
	}
	wg.Wait()

	// Drain messages.
	for range 20 {
		<-ch
	}

	// Wait for async hooks to complete.
	require.Eventually(t, func() bool {
		return hookCount.Load() == 20
	}, time.Second, 10*time.Millisecond, "each publish should trigger its After hook")
}

func TestGoroutineID(t *testing.T) {
	id := goroutineID()
	assert.NotZero(t, id)

	// Different goroutine should get a different ID.
	var otherID uint64
	done := make(chan struct{})
	go func() {
		otherID = goroutineID()
		close(done)
	}()
	<-done

	assert.NotEqual(t, id, otherID)
}
